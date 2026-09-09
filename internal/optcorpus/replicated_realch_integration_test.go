//go:build integration

// replicated_realch_integration_test.go — the corpus table's ENGINE against a
// REAL ClickHouse whose database is Replicated.
//
// # WHY THIS LANE EXISTS
//
// #3225 made the corpus DDL cluster-aware, which fixed where the TABLE exists.
// It did not fix where the ROWS live, and those are different properties. A
// Replicated DATABASE replicates DDL on its own but does NOT convert a
// MergeTree into a ReplicatedMergeTree: the plain engine is ACCEPTED, the
// CREATE succeeds, every INSERT succeeds, and each replica quietly ends up
// holding only the rows written through it. internal/routerrules then reads the
// corpus with an ordinary single-node SELECT and fits the route A/B go/no-go
// analysis to one replica's slice, reporting it as the whole corpus (cerberus
// issue #3241).
//
// No string assertion can see that. `ENGINE = MergeTree` is valid DDL inside a
// Replicated database, so a unit test over the rendered statement proves only
// that cerberus emits what it meant to emit — not that the server treats it as
// a replicating table. Only the server distinguishes the two, and it does so in
// exactly one place: system.replicas holds a row per table whose engine
// actually replicates its DATA. A plain-MergeTree corpus table is absent from
// it while existing perfectly well in system.tables.
//
// This is the same defect internal/schema/ddl's own
// TestApply_ReplicatedDatabase exists for on the signal tables (its "rc.2 bug"
// — tables that existed but never replicated, system.replicas 0), applied to
// the one table internal/schema/ddl does not provision.
//
// Gated by the `integration` build tag (Docker required); run by
// `just router-corpus-integration`, which the required `strict-scan` lane runs.
package optcorpus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chsql"
)

const (
	// replicatedCorpusDatabase is the database the corpus table is created in,
	// matching the chart's own `otel`.
	replicatedCorpusDatabase = "otel"

	// replicatedCorpusZooPath is the Keeper path the Replicated database engine
	// coordinates on, mirroring the chart's own
	// `/clickhouse/databases/<database>/{shard}/{replica}` default.
	replicatedCorpusZooPath = "/clickhouse/databases/" + replicatedCorpusDatabase

	// replicatedCorpusUser / replicatedCorpusPassword are the container's
	// credentials; the bootstrap connection lands in the always-present
	// `default` database, since the Replicated one does not exist yet.
	replicatedCorpusUser     = "cerberus"
	replicatedCorpusPassword = "cerberus"
	replicatedBootstrapDB    = "default"

	// replicatedCorpusPingTimeout bounds the post-dial readiness check, and
	// replicatedKeeperOperationTimeout the Keeper coordination requests the
	// Replicated database issues. The container's wait strategy covers
	// ClickHouse's HTTP endpoint, not the embedded Keeper election, so a
	// saturated runner can accept SQL before the single Keeper node can serve
	// its first coordination request — the same hazard
	// internal/schema/ddl's replicated lane documents.
	replicatedCorpusPingTimeout      = 30 * time.Second
	replicatedKeeperOperationTimeout = 60 * time.Second
)

// replicatedServerConfigTemplate turns on an embedded clickhouse-keeper, points
// the server's ZooKeeper client at it, and defines the {shard} / {replica}
// macros — the minimum a Replicated database needs to coordinate. One node is
// enough: the property under test is whether ClickHouse registers the corpus
// table as a REPLICA at all, which is a per-table engine fact, not a
// multi-node one.
const replicatedServerConfigTemplate = `<clickhouse>
    <keeper_server>
        <tcp_port>9181</tcp_port>
        <server_id>1</server_id>
        <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
        <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
        <coordination_settings>
            <operation_timeout_ms>%d</operation_timeout_ms>
            <session_timeout_ms>30000</session_timeout_ms>
        </coordination_settings>
        <raft_configuration>
            <server>
                <id>1</id>
                <hostname>localhost</hostname>
                <port>9234</port>
            </server>
        </raft_configuration>
    </keeper_server>
    <zookeeper>
        <node>
            <host>localhost</host>
            <port>9181</port>
        </node>
    </zookeeper>
    <macros>
        <shard>01</shard>
        <replica>replica1</replica>
    </macros>
</clickhouse>
`

// TestCorpusReplicatedEngineRealClickHouse is the behavioural pin for cerberus
// issue #3241: inside a Replicated database, the corpus table cerberus creates
// must actually REPLICATE ITS ROWS, and the server is the only witness.
//
// It asserts three things a rendered-SQL test cannot:
//
//  1. The engine cerberus emits is ACCEPTED. A Replicated database rejects
//     explicit ReplicatedMergeTree arguments with code 36, so the bare form is
//     not a stylistic choice — the CREATE fails without it.
//  2. system.replicas carries the corpus table. This is the definitive "the
//     DATA replicates" check; a plain MergeTree is absent from it while sitting
//     in system.tables looking healthy, which is exactly how the corpus came to
//     be partitioned per replica with nothing saying so.
//  3. A row written through the production sink lands and reads back. A
//     replicating engine that could not take the columnar batch would trade one
//     silent defect for a loud one.
//
// The re-run at the end covers the reconciliation path over the now-existing
// table: `CREATE TABLE IF NOT EXISTS` is a no-op there, so this is the first
// construction that actually exercises the deployed-engine verify against a
// server rather than a fake.
func TestCorpusReplicatedEngineRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), queryExitCHStartTimeout)
	defer cancel()

	bootstrap := startReplicatedCH(ctx, t)

	createDB := chsql.CreateDatabase(replicatedCorpusDatabase).
		IfNotExists().
		Engine(chsql.DatabaseEngineReplicated(replicatedCorpusZooPath, "{shard}", "{replica}")).
		SQL()
	if err := bootstrap.Exec(ctx, createDB); err != nil {
		t.Fatalf("create the Replicated database (%s): %v", createDB, err)
	}

	conn := openReplicatedCH(ctx, t, replicatedCorpusDatabase)

	topology := CorpusTableTopology{DatabaseReplicated: true}
	sink, err := NewCHTableSink(ctx, conn, topology)
	if err != nil {
		t.Fatalf("build the CH table sink inside a Replicated database: %v", err)
	}

	// The definitive check. A plain MergeTree — the engine this sink emitted
	// before #3241 — creates fine here and reports 0.
	var replicas uint64
	if err := conn.QueryRow(
		ctx,
		"SELECT count() FROM system.replicas WHERE database = ? AND table = ?",
		replicatedCorpusDatabase, CorpusTableName,
	).Scan(&replicas); err != nil {
		t.Fatalf("query system.replicas: %v", err)
	}
	if replicas != 1 {
		var engine string
		if err := conn.QueryRow(
			ctx,
			"SELECT engine FROM system.tables WHERE database = ? AND name = ?",
			replicatedCorpusDatabase, CorpusTableName,
		).Scan(&engine); err != nil {
			t.Fatalf("read the deployed engine after an unreplicated create: %v", err)
		}
		t.Fatalf("%s is registered in system.replicas %d time(s), want 1 — its engine is %q, "+
			"which does not replicate its rows: every replica would hold only what was written through it",
			CorpusTableName, replicas, engine)
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
		t.Fatalf("rebuild the sink over the provisioned replicated table: %v", err)
	}
}

// startReplicatedCH spins up a ClickHouse configured with an embedded Keeper and
// {shard}/{replica} macros, and returns a connection bound to the built-in
// `default` database — the Replicated one does not exist yet, exactly like a
// cold bootstrap against a clustered ClickHouse. The container's address is
// recorded on the test so openReplicatedCH can dial it again once the database
// exists.
func startReplicatedCH(ctx context.Context, t *testing.T) driver.Conn {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "replicated.xml")
	cfg := fmt.Sprintf(replicatedServerConfigTemplate, replicatedKeeperOperationTimeout.Milliseconds())
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write the replicated server config: %v", err)
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
	replicatedCHAddr = host + ":" + port.Port()

	return openReplicatedCH(ctx, t, replicatedBootstrapDB)
}

// replicatedCHAddr is the running container's native-protocol address, set by
// startReplicatedCH so the second dial (into the Replicated database, once it
// exists) reaches the same server. The lane runs one container per test and does
// not run in parallel, so a package-level value is the whole state it needs.
var replicatedCHAddr string

// openReplicatedCH dials the running container, bound to database.
func openReplicatedCH(ctx context.Context, t *testing.T, database string) driver.Conn {
	t.Helper()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{replicatedCHAddr},
		Auth: clickhouse.Auth{
			Database: database,
			Username: replicatedCorpusUser,
			Password: replicatedCorpusPassword,
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open a connection to %q: %v", database, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	pingCtx, pingCancel := context.WithTimeout(ctx, replicatedCorpusPingTimeout)
	defer pingCancel()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("ping %q: %v", database, err)
	}
	return conn
}
