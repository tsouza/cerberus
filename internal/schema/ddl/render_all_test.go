package ddl

import (
	"context"
	"strings"
	"testing"
)

// TestRenderAll_MatchesApply is the core contract: the offline RenderAll must
// return exactly the statements — same text, same order — that ApplyWithConfig
// executes against a live connection. It runs ApplyWithConfig against a
// recording conn and asserts equality with RenderAll, so the two can never
// drift (an offline schema preview that lies about what apply would run is
// worse than no preview at all).
func TestRenderAll_MatchesApply(t *testing.T) {
	cfg := Config{Database: "otel"}

	rc := &recordingConn{}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}

	rendered, err := RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}

	if len(rendered) != len(rc.execs) {
		t.Fatalf("RenderAll returned %d statements, ApplyWithConfig executed %d", len(rendered), len(rc.execs))
	}
	for i := range rendered {
		if rendered[i] != rc.execs[i] {
			t.Errorf("statement %d differs:\n  render: %s\n  apply:  %s", i, rendered[i], rc.execs[i])
		}
	}
}

// TestRenderAll_SkipDatabaseCreate mirrors the externally-managed-database
// path: with SkipDatabaseCreate the rendered DDL omits CREATE DATABASE but
// still renders the (qualified) table creates — again identical to apply.
func TestRenderAll_SkipDatabaseCreate(t *testing.T) {
	cfg := Config{Database: "otel", SkipDatabaseCreate: true}

	rc := &recordingConn{}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}
	rendered, err := RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}

	if len(rendered) != len(rc.execs) {
		t.Fatalf("RenderAll returned %d statements, ApplyWithConfig executed %d", len(rendered), len(rc.execs))
	}
	for _, s := range rendered {
		if len(s) >= len("CREATE DATABASE") && s[:len("CREATE DATABASE")] == "CREATE DATABASE" {
			t.Errorf("SkipDatabaseCreate must omit CREATE DATABASE, got: %s", s)
		}
	}
}

// TestRenderAll_NoSignals pins the empty-selector no-op: no signals means no
// tables and therefore no database, so RenderAll returns nothing rather than a
// stray CREATE DATABASE (matches ApplyWithConfig's early return).
func TestRenderAll_NoSignals(t *testing.T) {
	rendered, err := RenderAll(Config{Database: "otel"}, nil)
	if err != nil {
		t.Fatalf("RenderAll(nil signals): %v", err)
	}
	if len(rendered) != 0 {
		t.Errorf("expected no statements for empty signal set, got %d: %v", len(rendered), rendered)
	}
}

// TestRenderAll_MatchesApply_ColumnStatistics extends the core contract to
// ColumnStatisticsEnabled=true (issue #2766): the offline preview must
// include the same ADD STATISTICS ALTERs, in the same order, that
// ApplyWithConfig executes against a live (non-refusing) connection.
func TestRenderAll_MatchesApply_ColumnStatistics(t *testing.T) {
	cfg := Config{Database: "otel", ColumnStatisticsEnabled: true}

	rc := &recordingConn{}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}

	rendered, err := RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}

	if len(rendered) != len(rc.execs) {
		t.Fatalf("RenderAll returned %d statements, ApplyWithConfig executed %d", len(rendered), len(rc.execs))
	}
	for i := range rendered {
		if rendered[i] != rc.execs[i] {
			t.Errorf("statement %d differs:\n  render: %s\n  apply:  %s", i, rendered[i], rc.execs[i])
		}
	}
}

// TestRenderAll_ReplicatedRequiresZooPath pins that the Replicated-engine
// validation fires at render time exactly as it does at apply time, so the
// preview surfaces the misconfiguration before the operator ever dials CH.
func TestRenderAll_ReplicatedRequiresZooPath(t *testing.T) {
	cfg := Config{
		Database:       "otel",
		DatabaseEngine: DatabaseEngine{Replicated: true}, // no zoo path
	}
	if _, err := RenderAll(cfg, All); err == nil {
		t.Fatal("expected error: Replicated engine without a ZooKeeper/Keeper path must be rejected")
	}
}

// TestRenderAll_ReplicatedDatabaseClusterAttachesEveryHost pins how Cluster
// combines with a Replicated database engine: the ON CLUSTER clause lands on
// the CREATE DATABASE statement — the one statement that has to run on every
// host, because a Replicated database replicates DDL only to the hosts that
// have attached it — and on nothing else. Every table statement stays bare:
// the database replicates it, and ClickHouse rejects a table-level ON CLUSTER
// inside a Replicated database outright ("ON CLUSTER is not allowed for
// Replicated database", code 80). This is the shape the bundled chart's
// replicas>1 tier renders (cerberus issue #3581): without the fan-out the
// database was attached on the one replica cerberus dialled and the others
// never received a table.
func TestRenderAll_ReplicatedDatabaseClusterAttachesEveryHost(t *testing.T) {
	cfg := Config{
		Database: "otel",
		Cluster:  "bwc_cluster",
		DatabaseEngine: DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel",
		},
	}
	stmts, err := RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	const wantDB = "CREATE DATABASE IF NOT EXISTS otel ON CLUSTER `bwc_cluster` " +
		"ENGINE = Replicated('/clickhouse/databases/otel', '{shard}', '{replica}')"
	if stmts[0] != wantDB {
		t.Errorf("CREATE DATABASE must fan out over the cluster:\n got: %s\nwant: %s", stmts[0], wantDB)
	}
	for i, s := range stmts[1:] {
		if strings.Contains(s, "ON CLUSTER") {
			t.Errorf("statement %d carries ON CLUSTER inside a Replicated database (ClickHouse rejects it, code 80):\n%s", i+1, s)
		}
	}

	// The externally-managed-database path has no CREATE DATABASE to fan out,
	// so a Cluster alongside SkipDatabaseCreate renders no ON CLUSTER at all.
	cfg.SkipDatabaseCreate = true
	stmts, err = RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll (SkipDatabaseCreate): %v", err)
	}
	for i, s := range stmts {
		if strings.Contains(s, "ON CLUSTER") {
			t.Errorf("SkipDatabaseCreate: statement %d carries ON CLUSTER inside a Replicated database:\n%s", i, s)
		}
	}
}
