package promql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_further_setop_test.go pins cerberus
// issue #2581's investigation of its own third named wrapper family — a
// further `and`/`or`/`unless` wrapping a mixed float/histogram `or`
// subquery inner ([wrapMixedOrSubqueryInner],
// histogram_native_mixed_or_subquery_range_fn.go) — for the SELECT/FOLD-
// family outer-fn composition specifically (`<fn>((wrapper((a) or
// (b)))[range:step])`), as distinct from the BARE-subquery composition
// (no outer `<fn>`) cerberus issue #2589's own fix (PR #2597) already
// proved sound end to end (TestSubqueryMixedOrAndBare_ChDB,
// subquery_and_unless_mixed_histogram_chdb_test.go).
//
// All three shapes now compose, for every one of the fifteen names, across
// all three grid modes:
//
//   - Mixed `or` on the RIGHT of `and`/`unless` (`c and/unless (a or b)`):
//     `and`/`unless` always forward the LEFT operand's own row shape
//     verbatim (never the right's), so this subquery inner is ALWAYS
//     plain float-shaped — [lowerSubqueryOverBinary] already composes it
//     correctly via cerberus issue #2555's [lowerVectorSetOpOperand], with
//     no dependency on the mixed `or`'s own type at all. See
//     TestFurtherSetOpRHS_AlreadyComposes below — unchanged by cerberus
//     issue #2724, since this shape never reached its guard in the first
//     place.
//   - Mixed `or` on the LEFT (`(a or b) and/unless c`) and a further `or`
//     wrapping the mixed `or` (either order) both reach
//     [lowerOuterRangeFnOverSubquery]'s Histogram/Mixed-shape guard with a
//     genuinely correctly-lowered inner ([lowerSubqueryOverBinary]'s own
//     cerberus issue #2589 fix for and/unless-forwarding, #2555's own
//     nested-Mixed-operand handling for a further `or`) — cerberus issue
//     #2724 answers both directly at that guard, dispatching on the row
//     shape alone rather than re-deriving a per-shape recognizer
//     (histogram_native_mixed_or_subquery_further_setop_range_fn.go's own
//     doc has the full account, including why a naive
//     distribute-then-recombine attempt does NOT work here the way it does
//     for label_replace/label_join). See TestFurtherSetOpLHS_Composes and
//     TestFurtherSetOpOr_Composes below.
//
// With all three shapes closed, [lowerOuterRangeFnOverSubquery]'s own
// Histogram/Mixed-shape rejection is now mathematically unreachable for any
// query: every outer.Func.Name reaching that guard is validated as one of
// chplan.IsPromQLRangeWindowFunc's 26 names, and
// [lowerHistogramOrMixedSubqueryOuterFnInput] plus
// [histogramSubqueryFloatOnlyDropFunc] between them cover the full 26 —
// see the rejection-parity catalogue's own updated classification for that
// site.

// furtherSetOpQuery builds `<fn>((<lhs> <op> <rhs>)[5m:1m])` for the
// fifteen-name sweep below.
func furtherSetOpQuery(fn, lhs, op, rhs string) string {
	return fmt.Sprintf(`%s((%s %s %s)[5m:1m])`, fn, lhs, op, rhs)
}

// selectFoldFamilyNames is the fifteen SELECT/FOLD-family names
// [isHistogramSubqueryOuterFnName] (histogram_native_mixed_or_subquery_range_fn.go)
// recognises.
var selectFoldFamilyNames = []string{
	"count_over_time", "present_over_time", "last_over_time", "first_over_time",
	"resets", "changes", "ts_of_first_over_time", "ts_of_last_over_time",
	"rate", "increase", "delta", "irate", "idelta", "sum_over_time", "avg_over_time",
}

// TestFurtherSetOpRHS_AlreadyComposes proves `<fn>((c and/unless ((a) or
// (b)))[5m:1m])` — the mixed `or` on the RIGHT of a further `and`/`unless`
// — already lowers successfully to a plain float shape for every one of
// the fifteen names, purely via cerberus issue #2555's
// [lowerVectorSetOpOperand] / issue #2589's [lowerSubqueryOverBinary] fix
// (PR #2597): `and`/`unless` forward the LEFT (float) operand's own row
// shape unconditionally, so the mixed `or`'s own type never has a chance
// to matter.
func TestFurtherSetOpRHS_AlreadyComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`

	for _, op := range []string{"and", "unless"} {
		for _, fn := range selectFoldFamilyNames {
			t.Run(op+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := furtherSetOpQuery(fn, "http_requests_total", op, mixedOr)
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): %v", query, err)
				}
				switch got := chplan.RowShapeOf(plan); got {
				case chplan.SampleRowShape, chplan.ReducedWindowRowShape:
				default:
					t.Errorf("lower(%q) RowShape = %s, want an ordinary float shape (and/unless always forwards its float LHS)", query, got)
				}
			})
		}
	}
}

// TestFurtherSetOpLHS_Composes proves `<fn>((((a) or (b)) and/unless
// c)[5m:1m])` — the mixed `or` on the LEFT of a further `and`/`unless`, so
// the subquery inner genuinely forwards a Histogram/Mixed-shaped row — now
// composes for every one of the fifteen names (cerberus issue #2724),
// where it used to hit [lowerOuterRangeFnOverSubquery]'s guard with the
// SAME rejection cerberus issue #2589's own
// TestOuterRangeFnOverAndUnlessMixedSubquery_CleanRejection
// (subquery_and_unless_mixed_histogram_outer_test.go) still pins for a
// BARE-selector and/unless-forwarded histogram subquery inner with no
// mixed `or` involved at all — that shape is unaffected by this fix; only
// the mixed-`or`-on-the-left case this file's own doc names is new.
func TestFurtherSetOpLHS_Composes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`

	for _, op := range []string{"and", "unless"} {
		for _, fn := range selectFoldFamilyNames {
			t.Run(op+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := furtherSetOpQuery(fn, mixedOr, op, "http_requests_total")
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): want success, got error: %v", query, err)
				}
				if plan == nil {
					t.Fatalf("lower(%q): want a non-nil plan", query)
				}
			})
		}
	}
}

// TestFurtherSetOpOr_Composes proves a further `or` wrapping the mixed `or`
// (either operand order) now composes too (cerberus issue #2724): unlike
// `and`/`unless`, `or` genuinely propagates the mixed `or`'s own
// Histogram/Mixed type into the subquery inner's published row shape
// regardless of operand order, so it always used to hit
// [lowerOuterRangeFnOverSubquery]'s guard — there was no plain-float escape
// hatch here the way there is for `and`/`unless`'s right-hand case — but
// [lowerHistogramOrMixedSubqueryOuterFnInput] answers a MixedRowShape inner
// the identical way regardless of which AST shape produced it.
func TestFurtherSetOpOr_Composes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`

	for _, op := range []string{"lhs", "rhs"} {
		for _, fn := range selectFoldFamilyNames {
			t.Run(op+"/"+fn, func(t *testing.T) {
				t.Parallel()
				var query string
				if op == "lhs" {
					query = furtherSetOpQuery(fn, mixedOr, "or", "http_requests_total")
				} else {
					query = furtherSetOpQuery(fn, "http_requests_total", "or", mixedOr)
				}
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): want success, got error: %v", query, err)
				}
				if plan == nil {
					t.Fatalf("lower(%q): want a non-nil plan", query)
				}
			})
		}
	}
}

// TestFurtherSetOp_RangeFanoutComposes proves the same two shapes compose
// under a TRUE query_range fan-out (no `@` pin) too — cerberus issue
// #2724's own [lowerFurtherWrapMixedOrSubqueryFoldFn] /
// [lowerFloatFoldOverSubqueryInput] are built with full three-grid-mode
// support from the start, unlike the sum/avg-wrapped composer's own FOLD
// family (cerberus issue #2715), which needed a dedicated follow-up for
// fan-out.
func TestFurtherSetOp_RangeFanoutComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`

	for _, fn := range selectFoldFamilyNames {
		t.Run("and/"+fn, func(t *testing.T) {
			t.Parallel()
			query := furtherSetOpQuery(fn, mixedOr, "and", "http_requests_total")
			expr := parseExprExp(t, query)
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) over query_range: want success, got error: %v", query, err)
			}
			if plan == nil {
				t.Fatalf("lower(%q) over query_range: want a non-nil plan", query)
			}
		})
		t.Run("or/"+fn, func(t *testing.T) {
			t.Parallel()
			query := furtherSetOpQuery(fn, mixedOr, "or", "http_requests_total")
			expr := parseExprExp(t, query)
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) over query_range: want success, got error: %v", query, err)
			}
			if plan == nil {
				t.Fatalf("lower(%q) over query_range: want a non-nil plan", query)
			}
		})
	}
}
