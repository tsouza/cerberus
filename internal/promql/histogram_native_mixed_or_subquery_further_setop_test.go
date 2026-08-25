package promql_test

import (
	"context"
	"fmt"
	"strings"
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
// Unlike the label_replace/label_join wrapper family (#2581's already-
// closed first family, wrapMixedOrSubqueryInner's own doc), a further
// `and`/`unless` needs NO new recognizer here at all — the outer-fn-
// composition question resolves entirely from #2597's already-merged,
// already-deliberate design for a plain `and`/`unless`-forwarded histogram
// subquery inner, which applies identically whether the forwarded operand
// is a bare histogram selector or a mixed `or`:
//
//   - Mixed `or` on the RIGHT of `and`/`unless` (`c and/unless (a or b)`):
//     `and`/`unless` always forward the LEFT operand's own row shape
//     verbatim (never the right's), so this subquery inner is ALWAYS
//     plain float-shaped — [lowerSubqueryOverBinary] already composes it
//     correctly via cerberus issue #2555's [lowerVectorSetOpOperand], with
//     no dependency on the mixed `or`'s own type at all. See
//     TestFurtherSetOpRHS_AlreadyComposes below.
//   - Mixed `or` on the LEFT (`(a or b) and/unless c`): the histogram arm
//     alone, once the mixed `or` is conceptually split apart, is
//     `<fn>((a and/unless c)[range:step])` — EXACTLY the AST shape
//     cerberus issue #2589's own regression test
//     (TestOuterRangeFnOverAndUnlessMixedSubquery_CleanRejection,
//     subquery_and_unless_mixed_histogram_outer_test.go) pins as a
//     DELIBERATE clean rejection for every outer range-vector function
//     with no dedicated recognizer for it (rate, count_over_time, and
//     every other of the fifteen SELECT/FOLD-family names — none of them
//     has one). Widening #2545/#2569's own dedicated subquery recognizers
//     ([rangeFnOverExpHistogramSubquery] / [selectFnOverExpHistogramSubquery])
//     to also admit an `and`/`unless`-forwarded histogram-valued subquery
//     inner would directly reverse that already-merged, already-tested
//     decision — not a narrow extension, a regression against PR #2597's
//     own explicit scope line. See TestFurtherSetOpLHS_CleanlyRejects
//     below, which proves the identical rejection holds when the
//     forwarded operand is a mixed `or` rather than a bare selector (a
//     shape #2597's own tests never exercised).
//   - A further `or` (either order) still rejects too — the mixed `or`'s
//     own type genuinely propagates through an outer `or`'s union
//     (unlike `and`/`unless`, which never forward the other side's type),
//     so this subquery inner really is Histogram/Mixed-shaped and hits the
//     SAME guard, with no plain-float escape hatch the `and`/`unless`
//     right-hand case has. See TestFurtherSetOpOr_CleanlyRejects.
//
// Net effect: nothing in cerberus's existing, already-merged infrastructure
// needs to change for this wrapper family under the outer-fn composition —
// it is fully accounted for already, just previously untested for the
// mixed-`or`-specific case. #2581 stays open on this family exactly as it
// does on `sum`/`avg` (wrapMixedOrSubqueryInner's own doc), tracked by the
// same rejection-parity catalogue entry.

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

// TestFurtherSetOpLHS_CleanlyRejects proves `<fn>((((a) or (b))
// and/unless c)[5m:1m])` — the mixed `or` on the LEFT of a further
// `and`/`unless`, so the subquery inner genuinely forwards a
// Histogram/Mixed-shaped row — rejects cleanly with the same message
// cerberus issue #2589's own [lowerOuterRangeFnOverSubquery] guard already
// gives a bare-selector `and`/`unless`-forwarded histogram subquery inner
// (TestOuterRangeFnOverAndUnlessMixedSubquery_CleanRejection,
// subquery_and_unless_mixed_histogram_outer_test.go) — for every one of
// the fifteen names, not merely `rate`. No recognizer here composes this
// shape; #2581 leaves it as an open divergence for exactly the reason that
// existing test's own doc gives.
func TestFurtherSetOpLHS_CleanlyRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`
	const wantErrSubstr = "wrapping a native-histogram-valued shape is unsupported"

	for _, op := range []string{"and", "unless"} {
		for _, fn := range selectFoldFamilyNames {
			t.Run(op+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := furtherSetOpQuery(fn, mixedOr, op, "http_requests_total")
				expr := parseExprExp(t, query)
				_, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err == nil {
					t.Fatalf("lower(%q): want a clean rejection, got none", query)
				}
				if !strings.Contains(err.Error(), wantErrSubstr) {
					t.Errorf("lower(%q) error = %v, want it to contain %q", query, err, wantErrSubstr)
				}
			})
		}
	}
}

// TestFurtherSetOpOr_CleanlyRejects proves a further `or` wrapping the
// mixed `or` (either operand order) also rejects cleanly under the
// outer-fn composition: unlike `and`/`unless`, `or` genuinely propagates
// the mixed `or`'s own Histogram/Mixed type into the subquery inner's
// published row shape regardless of operand order, so it always hits
// [lowerOuterRangeFnOverSubquery]'s guard — there is no plain-float escape
// hatch here the way there is for `and`/`unless`'s right-hand case.
func TestFurtherSetOpOr_CleanlyRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mixedOr := `((latency_exp_hist) or (num_cpus))`
	const wantErrSubstr = "wrapping a native-histogram-valued shape is unsupported"

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"mixed_or_lhs", furtherSetOpQuery("count_over_time", mixedOr, "or", "http_requests_total")},
		{"mixed_or_rhs", furtherSetOpQuery("count_over_time", "http_requests_total", "or", mixedOr)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)
			_, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err == nil {
				t.Fatalf("lower(%q): want a clean rejection, got none", tc.query)
			}
			if !strings.Contains(err.Error(), wantErrSubstr) {
				t.Errorf("lower(%q) error = %v, want it to contain %q", tc.query, err, wantErrSubstr)
			}
		})
	}
}
