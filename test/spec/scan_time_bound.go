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
// invariant rejects it.
//
// NormalizeScanTimeBound establishes the instant windowed-array leaf scan bound
// and RequireScanTimeBound verifies every such leaf carries it; a panic here
// means a real corpus plan reached the analyzer unbounded, i.e. the invariant
// would block production traffic for that query shape. Wiring this into the
// per-head lower harnesses turns the whole promql / logql / traceql fixture
// corpus into a standing proof that NormalizeScanTimeBound's reach matches
// RequireScanTimeBound's demand across every lowered shape.
//
// Run never mutates its input, so calling this after the emit-golden assertion
// leaves the caller's plan untouched.
func AssertScanTimeBoundAccepts(t *testing.T, plan chplan.Node) {
	t.Helper()
	if plan == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("scan-time-bound invariant rejected a real lowered plan: %v", r)
		}
	}()
	_ = optimizer.Default().Run(context.Background(), plan)
}
