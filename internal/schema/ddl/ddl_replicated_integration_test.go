//go:build integration

package ddl_test

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

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// replicatedServerConfig is a single-node ClickHouse config that turns on an
// embedded clickhouse-keeper, points the server's ZooKeeper client at it, and
// defines the {shard} / {replica} macros — the minimum a Replicated database
// engine needs to coordinate. It is deliberately a ONE-node cluster: that is
// enough to prove the DDL cerberus emits is ACCEPTED by ClickHouse and that the
// tables register in system.replicas (i.e. they actually replicate DATA), which
// is the exact behaviour string-assertion unit tests can never observe.
//
// It runs against ClickHouse 24.8 ON PURPOSE — cerberus's documented minimum
// supported server (the preflight version floor) — so the bare engine is proven
// to work on the oldest server cerberus claims to support. The complementary
// guard that cerberus EMITS the bare form (the rc.3 regression) lives in the
// unit tests; this test proves that form is valid DDL and replicates DATA, the
// rc.2 gap. (Asserting bare-vs-explicit via system.tables.engine_full does not
// work: ClickHouse normalises a bare `ReplicatedMergeTree` to the full
// `ReplicatedMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}')` on
// store, so both forms read identically there. The strict-server rejection of
// explicit args — code 36 on the production cluster — is environment-, not
// version-deterministic, so it is not relied on here.)
const replicatedServerConfig = `<clickhouse>
    <keeper_server>
        <tcp_port>9181</tcp_port>
        <server_id>1</server_id>
        <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
        <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
        <coordination_settings>
            <operation_timeout_ms>10000</operation_timeout_ms>
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

// startClickHouseReplicated spins up a real ClickHouse whose ONLY database is
// the built-in `default`, configured with an embedded Keeper + {shard}/{replica}
// macros so a Replicated database can be created. The returned conn is bound to
// `default` (the Replicated `otel` database does not exist yet — Apply creates
// it), exactly like a cold-cluster bootstrap against a clustered ClickHouse.
func startClickHouseReplicated(t *testing.T) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfgPath := filepath.Join(t.TempDir(), "replicated.xml")
	if err := os.WriteFile(cfgPath, []byte(replicatedServerConfig), 0o644); err != nil {
		t.Fatalf("write replicated config: %v", err)
	}

	// 24.8 is cerberus's minimum supported server and the version where a
	// Replicated database rejects explicit ReplicatedMergeTree args (code 36) —
	// the regression this test exists to catch. See replicatedServerConfig.
	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:24.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithConfigFile(cfgPath),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%s", host, port.Port())},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: "cerberus",
			Password: "cerberus",
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pingCancel()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return conn
}

// countReplicas reports how many of the database's tables are registered in
// system.replicas — i.e. how many carry a ReplicatedMergeTree engine that
// actually replicates DATA. A Replicated database that left its tables as plain
// MergeTree (the rc.2 bug) reports 0 here even though the tables exist.
func countReplicas(ctx context.Context, t *testing.T, conn driver.Conn, database string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM system.replicas WHERE database = ?", database).Scan(&n); err != nil {
		t.Fatalf("query system.replicas: %v", err)
	}
	return n
}

// engines returns the (non-full) engine name ClickHouse recorded for every
// engine-bearing table of the database (the MV has none, so it is excluded by
// the non-empty filter). Note: system.tables.engine_full can NOT distinguish
// the bare engine cerberus emits from the explicit-args form — ClickHouse
// normalises a bare `ReplicatedMergeTree` to the full
// `ReplicatedMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}')` when
// it stores the table, auto-filling the default coordinates. So this asserts on
// `engine` (the bare family name) only; that cerberus emits the bare form is
// pinned by the unit tests (TestEngineReplicatedMergeTree /
// TestRenderSignal_ReplicatedDatabaseDefaultsToReplicatedMergeTree), and THIS
// test proves that form is accepted and actually replicates.
func engines(ctx context.Context, t *testing.T, conn driver.Conn, database string) map[string]string {
	t.Helper()
	rows, err := conn.Query(ctx,
		"SELECT name, engine FROM system.tables WHERE database = ? AND engine_full != '' ORDER BY name", database)
	if err != nil {
		t.Fatalf("query engine: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, eng string
		if err := rows.Scan(&name, &eng); err != nil {
			t.Fatalf("scan engine: %v", err)
		}
		out[name] = eng
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestApply_ReplicatedDatabase is the end-to-end regression test for the
// Replicated-cluster behaviour string-assertion unit tests cannot observe:
// that the DDL cerberus emits is ACCEPTED by a real Replicated database and
// that the tables actually replicate DATA. It closes the gap that let the rc.2
// bug ship — a Replicated database left its tables as plain MergeTree, so they
// existed but never replicated (system.replicas would be 0). It also exercises
// the bare ReplicatedMergeTree engine (the rc.3/rc.4 fix) against a real server,
// proving it is valid DDL; that cerberus emits the bare form rather than the
// explicit-args form a strict cluster rejects (code 36) is pinned by the unit
// tests.
//
// Against a real ClickHouse with an embedded Keeper, Apply must (a) succeed —
// proving the engine cerberus emits is accepted — and (b) leave every
// engine-bearing table registered in system.replicas with a ReplicatedMergeTree
// engine, proving the DATA actually replicates. The MV
// (otel_traces_trace_id_ts_mv) has no engine of its own, so the expected count
// is the 8 engine-bearing tables.
func TestApply_ReplicatedDatabase(t *testing.T) {
	conn := startClickHouseReplicated(t)
	ctx := context.Background()

	const target = "otel"
	if databaseExists(ctx, t, conn, target) {
		t.Fatalf("precondition: database %q already exists", target)
	}

	cfg := ddl.Config{
		Database: target,
		DatabaseEngine: ddl.DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel",
		},
	}
	// On a strict cluster (database_replicated_allow_replicated_engine_arguments
	// = 0) the rc.3 explicit-args engine fails HERE with code 36; cerberus's
	// bare ReplicatedMergeTree is accepted on strict and lenient servers alike.
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply against Replicated database: %v", err)
	}

	if !databaseExists(ctx, t, conn, target) {
		t.Fatalf("Replicated database %q was not created by Apply", target)
	}

	// The definitive "tables actually replicate DATA" check the production
	// report calls out: system.replicas must be populated. A Replicated database
	// that left its tables as plain MergeTree (rc.2) would report 0 here.
	const wantReplicas = 8 // 5 metrics + logs + spans + trace_id_ts lookup (the MV has no engine)
	if got := countReplicas(ctx, t, conn, target); got != wantReplicas {
		tables := listTables(ctx, t, conn, target)
		t.Fatalf("system.replicas count for %q = %d; want %d (tables: %v)", target, got, wantReplicas, tables)
	}

	// Every engine-bearing table must carry a ReplicatedMergeTree engine (not
	// plain MergeTree) — the family name, which is all engine_full can tell us
	// (ClickHouse normalises the bare form to the full one on store). That
	// cerberus emits the BARE form is pinned by the unit tests.
	got := engines(ctx, t, conn, target)
	if len(got) != wantReplicas {
		t.Fatalf("engine-bearing tables = %d; want %d (%v)", len(got), wantReplicas, got)
	}
	for name, eng := range got {
		if eng != "ReplicatedMergeTree" {
			t.Errorf("table %q: engine = %q; want ReplicatedMergeTree", name, eng)
		}
	}

	// Re-apply must stay clean now that the Replicated database + tables exist.
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply (rerun against provisioned Replicated database): %v", err)
	}
}
