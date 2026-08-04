package spec

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// AssertScanTimeBoundAccepts runs the full optimizer pipeline — which includes
// the fail-closed analyzer.scan-time-bound batch (NormalizeScanTimeBound then
// RequireScanTimeBound) — over a real lowered plan and fails the test if the
// invariant rejects it. It returns the optimized plan so callers can reuse it
// instead of running the optimizer a second time.
//
// NormalizeScanTimeBound establishes the instant windowed-array leaf scan bound
// and RequireScanTimeBound verifies every such leaf carries it; a panic here
// means a real corpus plan reached the analyzer unbounded, i.e. the invariant
// would block production traffic for that query shape. Wiring this into the
// per-head lower harnesses turns the whole promql / logql / traceql fixture
// corpus into a standing proof that NormalizeScanTimeBound's reach matches
// RequireScanTimeBound's demand across every lowered shape.
//
// Production always runs the optimizer before chsql.Emit (see
// internal/engine/engine.go's Parse → ProjectSamples → Optimize → Emit
// order), so the plan returned here — not the raw pre-optimizer plan the
// caller's own `-- sql --` / `-- chplan --` golden sections capture — is
// what actually reaches ClickHouse in production. Callers use it to re-emit
// and round-trip against chDB (see RunRoundTripSQL), closing the gap
// described in issue #1700: a fixture corpus that only ever validated the
// pre-optimizer plan.
//
// Run never mutates its input, so calling this after the emit-golden assertion
// leaves the caller's plan untouched.
func AssertScanTimeBoundAccepts(t *testing.T, plan chplan.Node) chplan.Node {
	t.Helper()
	if plan == nil {
		return nil
	}
	var optimized chplan.Node
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("scan-time-bound invariant rejected a real lowered plan: %v", r)
		}
	}()
	optimized = optimizer.Default().Run(context.Background(), plan)
	return optimized
}
