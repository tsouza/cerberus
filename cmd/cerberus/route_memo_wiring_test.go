package main

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/tsouza/cerberus/internal/solver"
)

// TestBuildRouteMemo_NilSolverNeverWires pins the nil-safety guard: a nil
// *solver.Solver (a head that never built one) must never dereference into
// Cfg — buildRouteMemo must return nil, not panic.
func TestBuildRouteMemo_NilSolverNeverWires(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if memo := buildRouteMemo(nil, logger); memo != nil {
		t.Fatalf("buildRouteMemo(nil, ...) = %v, want nil", memo)
	}
}

// TestBuildRouteMemo_DefaultOffLeavesEngineUnwired pins the byte-identical
// production contract this whole feature depends on: a Solver built from
// the library default Config (RouteMemoEnabled=false, the resolved value
// for any deployment that hasn't set CERBERUS_SOLVER_ROUTE_MEMO_ENABLED)
// must produce a nil route memo — the same "feature off" state
// engine.Engine.RouteMemo already treats as a complete no-op.
func TestBuildRouteMemo_DefaultOffLeavesEngineUnwired(t *testing.T) {
	cfg := solver.DefaultConfig()
	if cfg.RouteMemoEnabled {
		t.Fatalf("solver.DefaultConfig().RouteMemoEnabled = true, want false — this test's premise (library default is off) no longer holds")
	}
	sv := &solver.Solver{Cfg: cfg}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	if memo := buildRouteMemo(sv, logger); memo != nil {
		t.Fatalf("buildRouteMemo with RouteMemoEnabled=false = %v, want nil", memo)
	}
}

// TestBuildRouteMemo_EnabledWiresAMemo pins the opt-in path: with
// RouteMemoEnabled=true, buildRouteMemo must return a real, usable
// *routememo.Memo — the thing that actually makes engine.Engine.RouteMemo
// non-nil and the whole mechanism reachable outside a unit test.
func TestBuildRouteMemo_EnabledWiresAMemo(t *testing.T) {
	cfg := solver.DefaultConfig()
	cfg.RouteMemoEnabled = true
	sv := &solver.Solver{Cfg: cfg}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	memo := buildRouteMemo(sv, logger)
	if memo == nil {
		t.Fatal("buildRouteMemo with RouteMemoEnabled=true = nil, want a real *routememo.Memo")
	}
	// A real, usable Memo: Stats() should not panic and should report an
	// empty resident set for a freshly constructed instance.
	if stats := memo.Stats(); stats.Entries != 0 {
		t.Errorf("freshly constructed Memo.Stats().Entries = %d, want 0", stats.Entries)
	}
	if logBuf.Len() == 0 {
		t.Error("buildRouteMemo with RouteMemoEnabled=true logged nothing — an operator enabling a new runtime-behavior-changing feature should see it confirmed at startup")
	}
}
