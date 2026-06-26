package optimizer

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file wires the scan time-bound contract into the optimizer as two
// must-run analyzer rules:
//
//   - NormalizeScanTimeBound establishes the IR-level bound
//     (chplan.AttachInstantScanTimeBounds, applied per node) so the rest of
//     the pipeline — and the verification rule below — sees it.
//   - RequireScanTimeBound is the FAIL-CLOSED invariant: it rejects, at
//     plan-build time, any plan where an instant windowed-array leaf Scan
//     reaches emit without a time bound. This turns the recurring
//     unbounded-scan bug class (#1027 / #1048 / #1056 / #1059 / #1080 /
//     #1088 / #1089 / #1098) from a per-emitter memory into an enforced
//     contract.
//
// Both are AnalyzerRules: must-run (they execute before any heuristic
// OptimizerRule) and idempotent (Normalize is a no-op once the bound is set;
// Require never mutates). They live in a single analyzer batch, Normalize
// before Require, so a real lowered plan is bound and then verified in one
// pass. The narrow, honestly fail-closed set is the instant windowed-array
// leaf shape (chplan.IsInstantWindowedLeaf); the matrix / native / LWR /
// resample / MetricsAggregate emitters bound at emit time and the lowering
// `@` / offset / staleness paths carry a sibling Filter — those are out of
// this analyzer's IR-verified scope by design (see the file-level comment in
// internal/chplan/scan_time_bound.go and docs/engine.md).

// ScanTimeBoundViolation is the typed panic value RequireScanTimeBound raises
// when an instant windowed-array leaf RangeWindow would reach emit without a
// ScanTimeBound. It is a panic (not a returned error) because the optimizer
// Driver.Run signature carries no error channel and the existing analyzer
// contract already fails closed via panic (see analyzer.go's idempotence
// guard); the HTTP layer's panic-recovery middleware turns it into a 500
// rather than serving a plan that would melt ClickHouse with a full-retention
// groupArray. Tests recover it to assert the invariant fires.
type ScanTimeBoundViolation struct {
	// Func is the RangeWindow's range function (rate / increase / …).
	Func string
	// TimestampColumn is the column the (missing) bound would constrain.
	TimestampColumn string
}

func (e *ScanTimeBoundViolation) Error() string {
	return fmt.Sprintf(
		"optimizer: instant windowed-array RangeWindow (Func=%q, ts=%q) reaches emit without a ScanTimeBound — "+
			"this is the unbounded-scan bug class; the innermost groupArray would read full retention. "+
			"chplan.AttachInstantScanTimeBounds / NormalizeScanTimeBound must establish the bound",
		e.Func, e.TimestampColumn,
	)
}

// NormalizeScanTimeBound is the must-run analyzer rule that records the
// IR-level instant scan time bound on every instant windowed-array leaf Scan.
// It delegates the derivation to chplan so the bound is single-sourced with
// the emit-path establishment (chsql.Emit also calls
// chplan.AttachInstantScanTimeBounds for the optimizer-skipping spec lane).
type NormalizeScanTimeBound struct{}

func (NormalizeScanTimeBound) Name() string { return "normalize-scan-time-bound" }

func (NormalizeScanTimeBound) isAnalyzerRule() {}

func (NormalizeScanTimeBound) Apply(n chplan.Node) (chplan.Node, bool) {
	rw, ok := n.(*chplan.RangeWindow)
	if !ok {
		return n, false
	}
	bound, changed := chplan.WithInstantScanTimeBound(rw)
	return bound, changed
}

// RequireScanTimeBound is the must-run, fail-closed analyzer rule that
// verifies every instant windowed-array leaf Scan carries a TimeBound. It
// never mutates the tree (so it is trivially idempotent); on a violation it
// panics with a ScanTimeBoundViolation. Running it after
// NormalizeScanTimeBound means a real lowered plan always passes — a panic
// signals either a normalize gap (a bug in the establishment) or a hand-built
// plan that bypassed it.
type RequireScanTimeBound struct{}

func (RequireScanTimeBound) Name() string { return "require-scan-time-bound" }

func (RequireScanTimeBound) isAnalyzerRule() {}

func (RequireScanTimeBound) Apply(n chplan.Node) (chplan.Node, bool) {
	rw, ok := n.(*chplan.RangeWindow)
	if !ok {
		return n, false
	}
	if chplan.IsInstantWindowedLeaf(rw) && !rw.InstantScanBounded {
		panic(&ScanTimeBoundViolation{Func: rw.Func, TimestampColumn: rw.TimestampColumn})
	}
	return n, false
}
