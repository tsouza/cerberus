//go:build integration

// classiccluster_realch_integration_test.go — the corpus table's ENGINE against
// a REAL ClickHouse running the classic ON CLUSTER model.
//
// # WHY THIS LANE EXISTS
//
// #3241 made the corpus replicate under a Replicated DATABASE. The classic
// ON CLUSTER cluster is the other replicating topology, and there is no
// Replicated database in it: the operator declares replication by pinning an
// explicit engine for the SIGNAL tables through CERBERUS_SCHEMA_TABLE_ENGINE.
// The corpus sink did not read that knob, so the corpus table stayed a plain
// MergeTree, every node held only the rows written through it, and
// internal/routerrules' ordinary single-node SELECT fitted the route A/B
// go/no-go analysis to one node's slice while reporting it as the whole corpus
// (cerberus issue #3250). That topology is reachable both from the chart
// (dataShards.count > 1 with replicas > 1) and from an operator pointing
// cerberus at their own classic cluster with two documented, fully supported
// knobs.
//
// No string assertion can see the defect. `ENGINE = MergeTree` is valid DDL on
// a classic cluster — the CREATE succeeds on every node, every INSERT succeeds —
// so a unit test over the rendered statement proves only that cerberus emits
// what it meant to. Only the server distinguishes an accepted table from a
// replicating one, and it does so in one place: system.replicas holds a row per
// table whose engine actually replicates its DATA. A plain-MergeTree corpus
// table is absent from it while sitting in system.tables looking healthy.
//
// It is the sibling of replicated_realch_integration_test.go, one topology
// over, and shares that lane's server bootstrap (keeperServerConfigSection).
//
// Gated by the `integration` build tag (Docker required); run by
// `just router-corpus-integration`, which the required `strict-scan` lane runs.
package optcorpus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chsql"
)

const (
	// classicClusterName is the <remote_servers> cluster the server below
	// defines and every ON CLUSTER statement here names — what a real
	// deployment carries in CERBERUS_SCHEMA_CLUSTER.
	classicClusterName = "cerberus_test"

	// classicCorpusDatabase is the database the corpus table is created in,
	// matching the chart's own `otel`. Unlike the Replicated-database lane's,
	// it is an ordinary Atomic database: on the classic model the DDL is
	// propagated by the cluster, not by the database engine.
	classicCorpusDatabase = "otel"

	// classicSignalTableEngine is the CERBERUS_SCHEMA_TABLE_ENGINE an operator
	// pins on a classic cluster, and classicSignalTable is a stand-in SIGNAL
	// table created WITH it — the thing the corpus table must not be confused
	// with.
	//
	// The Keeper path deliberately ends in a LITERAL name rather than the
	// {table} macro. That is a legal, ordinary thing for an operator to write,
	// and it is what makes reusing the operator's expression for the corpus
	// table unsound: the corpus would land on this table's replica path and
	// collide with the replica already registered there. Cerberus reads the
	// knob only as a declaration and emits its own engine, so the two tables
	// have independent coordinates — which is what this lane asserts rather
	// than assumes.
	classicSignalTableEngine = "ReplicatedMergeTree('/clickhouse/tables/{shard}/{database}/signal', '{replica}')"
	classicSignalTable       = "otel_signal_stand_in"

	// classicNonReplicatingEngine is what the corpus table is deliberately
	// re-created with for the migration half of the test: the plain MergeTree an
	// older cerberus left behind on this topology.
	classicNonReplicatingEngine = "MergeTree"
)

// classicClusterRemoteServers defines a one-node <remote_servers> cluster. One
// node carries the whole claim: whether the corpus table registers as a REPLICA
// is a per-table engine fact, and whether ON CLUSTER reaches every node is
// already pinned by TestCorpusDDL_OnCluster. What a second node would add is
// coverage of ClickHouse's own distributed-DDL propagation, which is not
// cerberus's behaviour to prove.
const classicClusterRemoteServers = `    <remote_servers>
        <` + classicClusterName + `>
            <shard>
                <internal_replication>true</internal_replication>
                <replica>
                    <host>localhost</host>
                    <port>9000</port>
                </replica>
            </shard>
        </` + classicClusterName + `>
    </remote_servers>
`

// classicClusterServerConfigTemplate is the Replicated lane's Keeper server plus
// the cluster definition ON CLUSTER statements resolve against. The Keeper is
// needed for two independent reasons here: distributed DDL queues through it,
// and a ReplicatedMergeTree coordinates through it.
const classicClusterServerConfigTemplate = "<clickhouse>\n" +
	keeperServerConfigSection + classicClusterRemoteServers + "</clickhouse>\n"

// TestCorpusClassicClusterEngineRealClickHouse is the behavioural pin for
// cerberus issue #3250: on a classic ON CLUSTER cluster whose operator declared
// replication through CERBERUS_SCHEMA_TABLE_ENGINE, the corpus table cerberus
// creates must actually REPLICATE ITS ROWS, and the server is the only witness.
//
// It asserts four things a rendered-SQL test cannot:
//
//  1. The engine cerberus emits is ACCEPTED here. The bare ReplicatedMergeTree
//     carries no Keeper coordinates of its own; on this topology there is no
//     Replicated database to supply them, so they come from the server's
//     default_replica_path / default_replica_name. That this resolves at all is
//     the premise the whole design rests on, and it is a server fact.
//  2. system.replicas carries the corpus table. This is the definitive "the
//     DATA replicates" check; a plain MergeTree is absent from it while sitting
//     in system.tables looking healthy, which is exactly how the corpus came to
//     accumulate per node with nothing saying so.
//  3. The corpus table's Keeper path is its OWN — not the path the operator's
//     engine expression names for the signal tables. This is the assertion that
//     discriminates this design from the rejected one: had cerberus threaded
//     the operator's expression into the corpus DDL, the corpus would have
//     landed on the stand-in signal table's replica path (see
//     classicSignalTableEngine) and the CREATE would have failed outright.
//  4. A row written through the production sink lands and reads back. A
//     replicating engine that could not take the columnar batch would trade one
//     silent defect for a loud one.
//
// It then covers the migration half — the case corpusTableEngine cannot reach —
// by putting the table back the way an older cerberus left it, a plain
// MergeTree, and re-running construction: the verify must REFUSE it against the
// server's own answer, naming the knob and the ON CLUSTER'd remedy.
func TestCorpusClassicClusterEngineRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), queryExitCHStartTimeout)
	defer cancel()

	bootstrap, addr := startClassicClusterCH(ctx, t)

	createDB := chsql.CreateDatabase(classicCorpusDatabase).
		IfNotExists().
		OnCluster(classicClusterName).
		SQL()
	if err := bootstrap.Exec(ctx, createDB); err != nil {
		t.Fatalf("create the database ON CLUSTER (%s): %v", createDB, err)
	}

	conn := openReplicatedCH(ctx, t, addr, classicCorpusDatabase)

	// The stand-in SIGNAL table, created with the operator's OWN engine
	// expression exactly as internal/schema/ddl splices it into the upstream
	// templates. It claims the replica path that expression names, which is
	// what makes assertion 3 below a real check rather than a restatement.
	createSignal := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s ON CLUSTER `%s` (shape_id String) ENGINE = %s ORDER BY shape_id",
		classicSignalTable, classicClusterName, classicSignalTableEngine,
	)
	if err := conn.Exec(ctx, createSignal); err != nil {
		t.Fatalf("create the stand-in signal table (%s): %v", createSignal, err)
	}

	topology := CorpusTableTopology{
		Cluster:     classicClusterName,
		TableEngine: classicSignalTableEngine,
	}
	sink, err := NewCHTableSink(ctx, conn, topology)
	if err != nil {
		t.Fatalf("build the CH table sink on a classic ON CLUSTER cluster: %v", err)
	}

	// The definitive check. A plain MergeTree — the engine this sink emitted
	// on this topology before #3250 — creates fine here and reports 0.
	corpusPath := replicaZooPath(ctx, t, conn, CorpusTableName)
	if corpusPath == "" {
		var engine string
		if err := conn.QueryRow(
			ctx,
			"SELECT engine FROM system.tables WHERE database = ? AND name = ?",
			classicCorpusDatabase, CorpusTableName,
		).Scan(&engine); err != nil {
			t.Fatalf("read the deployed engine after an unreplicated create: %v", err)
		}
		t.Fatalf("%s is absent from system.replicas — its engine is %q, which does not replicate its "+
			"rows: every node would hold only what was written through it",
			CorpusTableName, engine)
	}

	// The corpus must not have taken the signal table's coordinates. Had the
	// operator's expression been threaded into the corpus DDL, these would be
	// equal — and in fact the CREATE above would already have failed, since
	// this replica name is taken.
	signalPath := replicaZooPath(ctx, t, conn, classicSignalTable)
	if signalPath == "" {
		t.Fatalf("the stand-in signal table is absent from system.replicas; the lane's premise "+
			"(that %q replicates) does not hold", classicSignalTableEngine)
	}
	if corpusPath == signalPath {
		t.Errorf("%s replicates on the SIGNAL tables' Keeper path %q — the corpus must resolve its own, "+
			"or two unrelated tables share replication coordinates", CorpusTableName, corpusPath)
	}

	// The row proves the replicating engine still takes the production
	// columnar batch — the write path this table exists for.
	const wantShapeID = "cerb:vector_selector"
	if err := sink.Write([]Row{{
		ShapeID:    wantShapeID,
		Language:   "promql",
		Route:      "A",
		ExitStatus: ExitOK.String(),
	}}); err != nil {
		t.Fatalf("write a corpus row through the replicating engine: %v", err)
	}
	var landedShapeID string
	if err := conn.QueryRow(ctx, "SELECT shape_id FROM "+CorpusTableName).Scan(&landedShapeID); err != nil {
		t.Fatalf("read the written row back: %v", err)
	}
	if landedShapeID != wantShapeID {
		t.Errorf("shape_id read back as %q; want %q", landedShapeID, wantShapeID)
	}

	// Re-construction over the existing table: the CREATE is a no-op, so this
	// is the deployed-engine verify running against a real server's own answer.
	if _, err := NewCHTableSink(ctx, conn, topology); err != nil {
		t.Fatalf("rebuild the sink over the provisioned replicating table: %v", err)
	}

	// The migration half. Put the table back the way an older cerberus left it
	// on this topology — a plain MergeTree, which CREATE IF NOT EXISTS will
	// never replace and no ALTER converts — and construction must refuse it.
	dropCorpus := chsql.DropTable("", CorpusTableName).OnCluster(classicClusterName).SQL()
	if err := conn.Exec(ctx, dropCorpus); err != nil {
		t.Fatalf("drop the corpus table (%s): %v", dropCorpus, err)
	}
	createPlain := fmt.Sprintf(
		"CREATE TABLE %s ON CLUSTER `%s` (shape_id String) ENGINE = %s ORDER BY shape_id",
		CorpusTableName, classicClusterName, classicNonReplicatingEngine,
	)
	if err := conn.Exec(ctx, createPlain); err != nil {
		t.Fatalf("recreate the corpus table as a plain MergeTree (%s): %v", createPlain, err)
	}
	_, err = NewCHTableSink(ctx, conn, topology)
	if err == nil {
		t.Fatal("NewCHTableSink over a plain-MergeTree corpus table on a replicating classic cluster: " +
			"want an error, got nil — the corpus would accumulate per node with nothing saying so")
	}
	// The operator has to act on this, so the message must name the knob they
	// set and hand back a DROP that reaches every node.
	for _, want := range []string{
		CorpusTableName,
		classicNonReplicatingEngine,
		envSchemaTableEngine,
		dropCorpus,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// replicaZooPath returns the Keeper path ClickHouse registered for table, or ""
// when the table is absent from system.replicas — which is the server's own way
// of saying "this table's engine does not replicate its DATA", however healthy
// it looks in system.tables.
func replicaZooPath(ctx context.Context, t *testing.T, conn driver.Conn, table string) string {
	t.Helper()

	var path string
	if err := conn.QueryRow(
		ctx,
		"SELECT any(zookeeper_path) FROM system.replicas WHERE database = ? AND table = ?",
		classicCorpusDatabase, table,
	).Scan(&path); err != nil {
		t.Fatalf("query system.replicas for %s: %v", table, err)
	}
	return path
}

// startClassicClusterCH spins up a ClickHouse configured as a classic
// ON CLUSTER deployment: an embedded Keeper (for both the distributed-DDL queue
// and ReplicatedMergeTree coordination), the {shard}/{replica} macros, and a
// <remote_servers> cluster. It returns a connection bound to the built-in
// `default` database — `otel` does not exist yet, exactly like a cold bootstrap
// against a clustered ClickHouse — plus the container's native-protocol address,
// so the caller can dial it again once that database does exist.
func startClassicClusterCH(ctx context.Context, t *testing.T) (driver.Conn, string) {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "classic-cluster.xml")
	cfg := fmt.Sprintf(classicClusterServerConfigTemplate, replicatedKeeperOperationTimeout.Milliseconds())
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write the classic-cluster server config: %v", err)
	}

	container, err := tcclickhouse.Run(
		ctx,
		queryExitCHImage,
		tcclickhouse.WithUsername(replicatedCorpusUser),
		tcclickhouse.WithPassword(replicatedCorpusPassword),
		tcclickhouse.WithConfigFile(cfgPath),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	addr := host + ":" + port.Port()

	return openReplicatedCH(ctx, t, addr, replicatedBootstrapDB), addr
}
