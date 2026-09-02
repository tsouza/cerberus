package promql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The two native-grid eligibility funnels — [nativeTSGridMatrixNode] (the
// timeSeries*ToGrid family) and [nativeLastOverTimeNode] (the stale-resample
// sibling) — are pure classifiers: every clause they check decides whether a
// window is handed to a ClickHouse aggregate that has NO equivalent of the
// fan-out path's per-anchor arrayJoin. A clause that stops firing does not make
// a query slower, it makes it WRONG — the native aggregate would be asked to
// materialise a grid it cannot express (an unpinned or zero-step window), or to
// read a series identity it does not group on.
//
// The lowering-level fixtures reach both funnels, but only ever down the
// accepting arm: they pin the SQL a well-shaped window emits, so a guard that
// was deleted outright still produces the same golden text for every input the
// corpus happens to contain. That is exactly the gap mutation testing reports —
// each `||` in the two guards below survived as an unkilled mutant on
// `phase4-promql-lower` (issue #2883).
//
// The rejections are therefore asserted one clause at a time, each case
// perturbing EXACTLY ONE field of a window the same test proves is otherwise
// accepted. Holding every other field at an accepted value is what makes each
// assertion specific: a nil result can only be attributed to the field under
// test, so short-circuiting one disjunct into a conjunction (`||` -> `&&`)
// flips that single case and nothing else.

// nativeGuardStep is the eval-grid resolution the accepted baseline windows
// carry — any strictly positive duration clears the `Step > 0` clause.
const nativeGuardStep = 30 * time.Second

// nativeGuardRange is the PromQL matrix `[range]` the baseline windows read.
const nativeGuardRange = 5 * time.Minute

// nativeGuardWindowSpan is the distance between the baseline Start and End
// anchors, i.e. the eval grid the native aggregate materialises.
const nativeGuardWindowSpan = time.Hour

// nativeGuardScalarHorizon is the literal `t` a predict_linear-shaped window
// would carry in Scalars; here it is only a non-empty marker value.
const nativeGuardScalarHorizon = 60.0

// nativeGuardStart / nativeGuardEnd are the pinned grid endpoints. They are
// wall-clock-independent so the classifier's verdict is deterministic.
func nativeGuardStart() time.Time {
	return time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
}

func nativeGuardEnd() time.Time {
	return nativeGuardStart().Add(nativeGuardWindowSpan)
}

// nativeGuardInput is the row-shape relation both funnels accept: a plain
// Filter-over-Scan chain ([isPlainScanFilter]).
func nativeGuardInput(s schema.Metrics) chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Database: "otel", Table: "otel_metrics_sum"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: s.MetricNameColumn},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
}

// acceptedGridWindow is a window [nativeTSGridMatrixNode] accepts for `rate`.
func acceptedGridWindow(s schema.Metrics) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           nativeGuardInput(s),
		Func:            "rate",
		Range:           nativeGuardRange,
		Step:            nativeGuardStep,
		Start:           nativeGuardStart(),
		End:             nativeGuardEnd(),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
}

// acceptedLastOverTimeWindow is a window [nativeLastOverTimeNode] accepts.
func acceptedLastOverTimeWindow(s schema.Metrics) *chplan.RangeWindow {
	rw := acceptedGridWindow(s)
	rw.Func = "last_over_time"
	return rw
}

// TestNativeTSGridMatrixNode_GridGuardRejectsEachUnpinnedClause pins that the
// materialised-grid precondition rejects on EVERY one of its four clauses
// independently: an identity (bare-vector subquery) window, a window with no
// eval step, and a window with either endpoint unpinned.
//
// The native emitter renders `timeSeries*ToGrid(start, end, step, …)`, whose
// three grid parameters have no fan-out fallback inside the aggregate. A window
// that reaches it with Step == 0 asks ClickHouse to divide the span by zero
// buckets; one with a zero Start or End substitutes the emitter's `now64()`
// anchor, which silently answers a DIFFERENT time range than the request. Each
// case below is the only one of the four that a single short-circuit collapse
// of this guard would let through.
func TestNativeTSGridMatrixNode_GridGuardRejectsEachUnpinnedClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	if got := nativeTSGridMatrixNode(acceptedGridWindow(s), "rate", s, false); got == nil {
		t.Fatal("baseline pinned-grid rate window was refused; the rejection cases below would be vacuous")
	}

	for _, tc := range []struct {
		name    string
		perturb func(*chplan.RangeWindow)
	}{
		{
			// Identity is the bare-vector subquery no-op path: the
			// emitter must read the last sample in the window, not
			// apply `rate`'s per-second reduction.
			name:    "identity window",
			perturb: func(rw *chplan.RangeWindow) { rw.Identity = true },
		},
		{
			// Step == 0 is the instant shape, owned by
			// nativeTSGridInstantNode. It is also the exact boundary
			// the `<= 0` comparison exists for: `< 0` would admit it.
			name:    "zero step",
			perturb: func(rw *chplan.RangeWindow) { rw.Step = 0 },
		},
		{
			name:    "unpinned start",
			perturb: func(rw *chplan.RangeWindow) { rw.Start = time.Time{} },
		},
		{
			name:    "unpinned end",
			perturb: func(rw *chplan.RangeWindow) { rw.End = time.Time{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := acceptedGridWindow(s)
			tc.perturb(rw)
			if got := nativeTSGridMatrixNode(rw, "rate", s, false); got != nil {
				t.Errorf("nativeTSGridMatrixNode accepted a %s: %#v; want nil so the fan-out path emits it", tc.name, got)
			}
		})
	}
}

// TestNativeLastOverTimeNode_GridGuardRejectsEachUnpinnedClause is the
// stale-resample sibling of the case above. chplan.RangeWindowStaleResample
// carries Start / End / Step straight into the emitted resample grid, so the
// same four clauses guard the same wrong answer.
func TestNativeLastOverTimeNode_GridGuardRejectsEachUnpinnedClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	if got := nativeLastOverTimeNode(acceptedLastOverTimeWindow(s), s); got == nil {
		t.Fatal("baseline pinned-grid last_over_time window was refused; the rejection cases below would be vacuous")
	}

	for _, tc := range []struct {
		name    string
		perturb func(*chplan.RangeWindow)
	}{
		{
			name:    "identity window",
			perturb: func(rw *chplan.RangeWindow) { rw.Identity = true },
		},
		{
			name:    "zero step",
			perturb: func(rw *chplan.RangeWindow) { rw.Step = 0 },
		},
		{
			name:    "unpinned start",
			perturb: func(rw *chplan.RangeWindow) { rw.Start = time.Time{} },
		},
		{
			name:    "unpinned end",
			perturb: func(rw *chplan.RangeWindow) { rw.End = time.Time{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := acceptedLastOverTimeWindow(s)
			tc.perturb(rw)
			if got := nativeLastOverTimeNode(rw, s); got != nil {
				t.Errorf("nativeLastOverTimeNode accepted a %s: %#v; want nil so the fan-out path emits it", tc.name, got)
			}
		})
	}
}

// TestNativeLastOverTimeNode_RequiresSoleIdentityGroupKey pins BOTH halves of
// the series-identity precondition, which is a disjunction of two independently
// sufficient rejections.
//
// chplan.RangeWindowStaleResample has no GroupBy field at all: it hard-codes its
// grouping to (MetricName, Attributes) in the emitter. Handing it a window that
// grouped on anything else — a second key, or a single key that is not the
// schema's attributes column — would therefore silently DISCARD that key and
// collapse distinct output series into one. Neither half can be dropped:
// "exactly one key" without "and it is the identity one" admits a lone wrong
// key, and the converse admits a wrong key alongside the right one.
func TestNativeLastOverTimeNode_RequiresSoleIdentityGroupKey(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name    string
		groupBy []chplan.Expr
	}{
		{
			// Arity fails, identity holds for element 0.
			name: "identity key plus a second key",
			groupBy: []chplan.Expr{
				&chplan.ColumnRef{Name: s.AttributesColumn},
				&chplan.ColumnRef{Name: s.MetricNameColumn},
			},
		},
		{
			// Arity holds, identity fails.
			name:    "sole non-identity key",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.MetricNameColumn}},
		},
		{
			// Arity holds, identity fails on the expression SHAPE
			// rather than the name: a computed key is not a bare
			// column reference the emitter can drop.
			name: "sole computed key",
			groupBy: []chplan.Expr{&chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.AttributesColumn},
				Key: &chplan.LitString{V: "job"},
			}},
		},
		{
			// Both halves fail at once.
			name:    "no group keys",
			groupBy: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := acceptedLastOverTimeWindow(s)
			rw.GroupBy = tc.groupBy
			if got := nativeLastOverTimeNode(rw, s); got != nil {
				t.Errorf("nativeLastOverTimeNode accepted %s: %#v; want nil so the fan-out path keeps the grouping", tc.name, got)
			}
		})
	}
}

// TestNativeLastOverTimeNode_RejectsEachFusedWindowCarrier pins the four-clause
// "this window carries more than one plain reduction" guard.
//
// chplan.RangeWindowStaleResample emits ONE resampled value column. Each of the
// four fields below is a carrier for extra per-window outputs that the node has
// nowhere to put: DeltaPrefixAggregateInput is the delta-temporality prefix
// relation, Variants is the fused multi-arm shape, and ScalarExprs / Scalars are
// the parametric arguments a function like predict_linear threads through.
// Accepting any of them would emit a query that silently answers only the first
// arm. Each field is set alone, so no case leans on another's rejection.
func TestNativeLastOverTimeNode_RejectsEachFusedWindowCarrier(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name    string
		perturb func(*chplan.RangeWindow)
	}{
		{
			name: "delta-temporality prefix relation",
			perturb: func(rw *chplan.RangeWindow) {
				rw.DeltaPrefixAggregateInput = nativeGuardInput(s)
			},
		},
		{
			name: "fused multi-arm variants",
			perturb: func(rw *chplan.RangeWindow) {
				rw.Variants = []chplan.RangeWindowVariant{
					{Func: "last_over_time", ValueColumn: s.ValueColumn},
				}
			},
		},
		{
			name: "parametric scalar expressions",
			perturb: func(rw *chplan.RangeWindow) {
				rw.ScalarExprs = []chplan.Expr{&chplan.LitFloat{V: nativeGuardScalarHorizon}}
			},
		},
		{
			name: "parametric scalar literals",
			perturb: func(rw *chplan.RangeWindow) {
				rw.Scalars = []float64{nativeGuardScalarHorizon}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := acceptedLastOverTimeWindow(s)
			tc.perturb(rw)
			if got := nativeLastOverTimeNode(rw, s); got != nil {
				t.Errorf("nativeLastOverTimeNode accepted a window carrying %s: %#v; want nil so every arm is emitted", tc.name, got)
			}
		})
	}
}
