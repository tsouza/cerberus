package lsyntax

import (
	"strings"
	"testing"
)

// LogQL's `sort` / `sort_desc` are the two vector-aggregation operators that
// accept NO grouping clause at all: `sort by (x) (…)` is a query the reference
// parser rejects outright, because the operator sorts the whole instant vector
// and has no per-group meaning. validateSortGrouping is the one place that
// rule lives, and validateSampleExpr's *VectorAggregationExpr arm is the only
// place it is consulted.
//
// Both of those sites survived mutation on `phase4-logql-parser` (cerberus
// issue #2913) — the leg's only two survivors. Nothing in the corpus ever
// wrote a grouping clause onto a sort, so:
//
//   - negating validateSortGrouping's `g != nil` nil-guard was invisible: with
//     no case that reaches it carrying a grouping, the function returned nil
//     either way; and
//   - negating the `err != nil` check on its result in validateSampleExpr was
//     invisible for the same reason — an always-nil error is indistinguishable
//     from an always-ignored one.
//
// The rejection is therefore asserted directly, in all three surface forms the
// grammar admits (`by`, `without`, and `sort_desc`'s own), alongside the
// accepted no-grouping baseline that proves the operator itself parses. The
// accepted case is what makes the rejections specific: it is the same query
// with the grouping clause removed, so nothing but the clause can explain the
// difference.

// sortGroupingRejection is the reference wording validateSortGrouping raises.
// Matching on it (not merely on "some error") is what pins the rejection to
// THIS rule rather than to an unrelated parse failure elsewhere in the query.
const sortGroupingRejection = "sort and sort_desc doesn't allow grouping by"

// sortableInner is the range aggregation every case below sorts. It is a
// well-formed selector, so it contributes no error of its own.
const sortableInner = `rate({app="a"}[1m])`

func TestParseExpr_SortAcceptsNoGroupingClause(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"sort(" + sortableInner + ")",
		"sort_desc(" + sortableInner + ")",
	} {
		expr, err := ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q) = %v; want the query to parse — the rejection cases below are this same query plus a grouping clause, and prove nothing if the baseline is already broken", q, err)
		}
		agg, ok := expr.(*VectorAggregationExpr)
		if !ok {
			t.Fatalf("ParseExpr(%q) = %T; want *VectorAggregationExpr", q, expr)
		}
		if agg.Grouping != nil && len(agg.Grouping.Groups) != 0 {
			t.Errorf("ParseExpr(%q) produced grouping %#v; want none", q, agg.Grouping)
		}
	}
}

func TestParseExpr_SortRejectsEveryGroupingClause(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"sort by (app) (" + sortableInner + ")",
		"sort without (app) (" + sortableInner + ")",
		"sort_desc by (app) (" + sortableInner + ")",
		"sort_desc without (app) (" + sortableInner + ")",
	} {
		_, err := ParseExpr(q)
		if err == nil {
			t.Errorf("ParseExpr(%q) = nil error; want the grouping clause rejected", q)
			continue
		}
		if !strings.Contains(err.Error(), sortGroupingRejection) {
			t.Errorf("ParseExpr(%q) = %v; want an error containing %q", q, err, sortGroupingRejection)
		}
	}
}

// TestParseExpr_SortStillValidatesItsOperand pins the OTHER half of the
// *VectorAggregationExpr arm: the grouping check is a guard placed BEFORE the
// recursive descent into the sorted expression, not a replacement for it. A
// sort over an operand that is itself invalid must still be rejected on the
// operand's own grounds — which is what stops the grouping check from being
// wired as an early `return` on the sort arm.
func TestParseExpr_SortStillValidatesItsOperand(t *testing.T) {
	t.Parallel()
	// `app=~".*"` is empty-compatible: it matches every stream, which
	// validateMatchers rejects. The sort wrapper must not mask it.
	const q = `sort(rate({app=~".*"}[1m]))`
	if _, err := ParseExpr(q); err == nil {
		t.Fatalf("ParseExpr(%q) = nil error; want the empty-compatible matcher inside the sort rejected", q)
	}
}
