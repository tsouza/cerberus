package ddl

import (
	"context"
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
