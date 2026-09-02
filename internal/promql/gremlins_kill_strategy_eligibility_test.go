package promql

// The four boot-wired range-strategy eligibility predicates in
// lower_strategy.go — fixedAccumulatorEligible, sortedSlabOverTimeEligible,
// lagAdjacencyEligible and downsampleTierEligible — each open with one
// multi-clause `||` guard that decides whether a query takes the
// ClickHouse-native path or the unchanged fan-out. Every clause of every one
// of those guards survived mutation on `phase4-promql-g` (cerberus issue
// #2913): the corpus that reaches these strategies only ever carries a fully
// materialised matrix grid, so nothing distinguished `<= 0` from `< 0` on the
// window bounds, and nothing distinguished `||` from `&&` between the clauses.
//
// A `||` chain is only pinned by a case where the clauses DISAGREE: flipping
// one `||` to `&&` is invisible whenever every clause agrees, and invisible
// whenever the guard is never reached. So each table below starts from a
// baseline the test first proves IS routed to the native path, then perturbs
// exactly ONE clause of the guard and requires the strategy to decline. That
// makes the case impossible to satisfy vacuously: a broken baseline fails the
// accept assertion, and a guard that stopped guarding fails the decline
// assertion.
//
// The bounds clauses are asserted at their exact `== 0` boundary rather than
// with a negative duration, because `<= 0` and `< 0` differ ONLY at zero — a
// negative-duration case would pass against either operator and prove nothing.

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The baseline window every case below perturbs: a materialised query_range
// matrix grid over a whole number of downsample-tier buckets, anchored on a
// bucket boundary so the tier guard's alignment clauses are satisfied too.
const (
	// eligibilityBaselineRange is the window duration — one tier bucket, so
	// the same baseline satisfies downsampleTierEligible's
	// integer-multiple-of-bucket clause.
	eligibilityBaselineRange = schema.DownsampleTierBucket

	// eligibilityBaselineStep is the eval step — also one tier bucket, for
	// the same reason.
	eligibilityBaselineStep = schema.DownsampleTierBucket

	// eligibilityBaselineOuterRange is the fan-out span the three
	// non-tier guards require to be positive.
	eligibilityBaselineOuterRange = time.Hour

	// eligibilityBaselineStartUnix is the grid start, an exact multiple of
	// schema.DownsampleTierBucket in absolute Unix-epoch seconds
	// (1700000100 = 5666667 * 300) so downsampleTierEligible's
	// bucket-boundary clause holds.
	eligibilityBaselineStartUnix = 1700000100
)

// eligibilityBaseline returns the range window every case perturbs: eligible
// for all four strategies as written.
func eligibilityBaseline() *chplan.RangeWindow {
	start := time.Unix(eligibilityBaselineStartUnix, 0).UTC()
	return &chplan.RangeWindow{
		Func:                "rate",
		Range:               eligibilityBaselineRange,
		Step:                eligibilityBaselineStep,
		OuterRange:          eligibilityBaselineOuterRange,
		Start:               start,
		End:                 start.Add(eligibilityBaselineOuterRange),
		DownsampleTierInput: &chplan.Scan{Table: schema.DownsampleTierTable},
	}
}

// eligibilityCase is one perturbation of the baseline window plus the reason
// the guard under test must decline it.
type eligibilityCase struct {
	name string
	// perturb mutates exactly one clause's input on a fresh baseline.
	perturb func(rw *chplan.RangeWindow)
	// why explains, in the failure message, what the guard is for.
	why string
}

// boundsPerturbations are the four clauses fixedAccumulatorEligible,
// sortedSlabOverTimeEligible and lagAdjacencyEligible share verbatim:
//
//	if rw.OuterRange <= 0 || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero()
//
// Every one of them must independently send the query to the fan-out
// fallback.
var boundsPerturbations = []eligibilityCase{
	{
		name:    "OuterRange is exactly zero",
		perturb: func(rw *chplan.RangeWindow) { rw.OuterRange = 0 },
		why:     "a window with no fan-out span is not the materialised matrix grid these emitters need",
	},
	{
		name:    "Step is exactly zero",
		perturb: func(rw *chplan.RangeWindow) { rw.Step = 0 },
		why:     "a zero step is the instant shape, which has no anchor grid to decompose",
	},
	{
		name:    "Start is unset",
		perturb: func(rw *chplan.RangeWindow) { rw.Start = time.Time{} },
		why:     "an unpinned Start leaves the scan-prune bound these emitters rely on unbounded",
	},
	{
		name:    "End is unset",
		perturb: func(rw *chplan.RangeWindow) { rw.End = time.Time{} },
		why:     "an unpinned End leaves the scan-prune bound these emitters rely on unbounded",
	},
}

func TestFixedAccumulatorRateLowerer_DeclinesEveryUnboundedWindowClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	l := FixedAccumulatorRateLowerer{Fallback: FanoutRateLowerer{}}

	routed, ok := l.LowerRate(eligibilityBaseline(), s).(*chplan.RangeWindow)
	if !ok || !routed.FixedAccumulatorExtrapolated {
		t.Fatalf("baseline window was not routed to the fixed-accumulator path (%#v); every decline case below would then prove nothing", routed)
	}

	for _, tc := range boundsPerturbations {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := eligibilityBaseline()
			tc.perturb(rw)
			got, isWindow := l.LowerRate(rw, s).(*chplan.RangeWindow)
			if !isWindow {
				t.Fatalf("LowerRate returned %#v; want a *chplan.RangeWindow", got)
			}
			if got.FixedAccumulatorExtrapolated {
				t.Errorf("LowerRate routed to the fixed-accumulator path with %s; want the fan-out fallback — %s", tc.name, tc.why)
			}
		})
	}
}

func TestSortedSlabOverTimeLowerer_DeclinesEveryUnboundedWindowClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	l := SortedSlabOverTimeLowerer{Fallback: FanoutOverTimeLowerer{}}

	routed, ok := l.LowerOverTime(eligibilityBaseline(), s).(*chplan.RangeWindow)
	if !ok || !routed.SortedSlabOverTime {
		t.Fatalf("baseline window was not routed to the sorted-slab path (%#v); every decline case below would then prove nothing", routed)
	}

	for _, tc := range boundsPerturbations {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := eligibilityBaseline()
			tc.perturb(rw)
			got, isWindow := l.LowerOverTime(rw, s).(*chplan.RangeWindow)
			if !isWindow {
				t.Fatalf("LowerOverTime returned %#v; want a *chplan.RangeWindow", got)
			}
			if got.SortedSlabOverTime {
				t.Errorf("LowerOverTime routed to the sorted-slab path with %s; want the fan-out fallback — %s", tc.name, tc.why)
			}
		})
	}
}

func TestLagAdjacencyIrateLowerer_DeclinesEveryUnboundedWindowClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	l := LagAdjacencyIrateLowerer{Fallback: FanoutIrateLowerer{}}

	routed, ok := l.LowerIrate(eligibilityBaseline(), s).(*chplan.RangeWindow)
	if !ok || !routed.LagAdjacency {
		t.Fatalf("baseline window was not routed to the lag-adjacency path (%#v); every decline case below would then prove nothing", routed)
	}

	for _, tc := range boundsPerturbations {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := eligibilityBaseline()
			tc.perturb(rw)
			got, isWindow := l.LowerIrate(rw, s).(*chplan.RangeWindow)
			if !isWindow {
				t.Fatalf("LowerIrate returned %#v; want a *chplan.RangeWindow", got)
			}
			if got.LagAdjacency {
				t.Errorf("LowerIrate routed to the lag-adjacency path with %s; want the fan-out fallback — %s", tc.name, tc.why)
			}
		})
	}
}

// downsampleTierEligible's own opening guard differs from the three above: it
// leads with rw.Identity instead of an OuterRange bound (OuterRange carries no
// subquery-specific signal at this layer, per the predicate's doc), and its
// range clause lives on a separate line. Each clause gets its own case for the
// same reason.
var downsampleTierPerturbations = []eligibilityCase{
	{
		name:    "Identity window",
		perturb: func(rw *chplan.RangeWindow) { rw.Identity = true },
		why:     "the bare-vector subquery no-op path is not one of the tier's three owning functions",
	},
	{
		name:    "Step is exactly zero",
		perturb: func(rw *chplan.RangeWindow) { rw.Step = 0 },
		why:     "a zero step is the instant shape, which the tier's materialised grid cannot answer",
	},
	{
		name:    "Start is unset",
		perturb: func(rw *chplan.RangeWindow) { rw.Start = time.Time{} },
		why:     "an unpinned Start cannot be checked against the tier's absolute bucket boundaries",
	},
	{
		name:    "End is unset",
		perturb: func(rw *chplan.RangeWindow) { rw.End = time.Time{} },
		why:     "an unpinned End leaves the tier read unbounded",
	},
	{
		name:    "Range is exactly zero",
		perturb: func(rw *chplan.RangeWindow) { rw.Range = 0 },
		why:     "a zero-width window covers no whole tier bucket, and 0 % bucket == 0 makes the multiple check alone accept it",
	},
}

func TestDownsampleTierIrateLowerer_DeclinesEveryShapeClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	l := DownsampleTierIrateLowerer{Fallback: FanoutIrateLowerer{}}

	routed, ok := l.LowerIrate(eligibilityBaseline(), s).(*chplan.RangeWindow)
	if !ok || !routed.DownsampleTier {
		t.Fatalf("baseline window was not routed to the downsampled tier (%#v); every decline case below would then prove nothing", routed)
	}

	for _, tc := range downsampleTierPerturbations {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := eligibilityBaseline()
			tc.perturb(rw)
			got, isWindow := l.LowerIrate(rw, s).(*chplan.RangeWindow)
			if !isWindow {
				t.Fatalf("LowerIrate returned %#v; want a *chplan.RangeWindow", got)
			}
			if got.DownsampleTier {
				t.Errorf("LowerIrate routed to the downsampled tier with %s; want the raw-scan fallback — %s", tc.name, tc.why)
			}
		})
	}
}
