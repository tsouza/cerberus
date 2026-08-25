//go:build chdb

// chDB-backed proof that cerberus issue #2569's fix actually executes
// correctly against real ClickHouse — not merely that the emitted plan's Go
// shape looks right (the same discipline
// histogram_native_subquery_select_chdb_test.go's own sibling file
// established for #2545's root-only fix).
//
// #2545 gave `<fn>((h)[range:step])` its own dedicated lowering for the
// SELECT/COUNT family (count_over_time, present_over_time, last_over_time,
// first_over_time, resets, changes, ts_of_first_over_time,
// ts_of_last_over_time) and the FOLD family (rate, increase, delta, irate,
// idelta, sum_over_time, avg_over_time), but only reachable when that whole
// expression is the query's own ROOT — [lowerHistogramNativeRoot]'s
// dispatch table. The moment the SAME shape is nested under a further
// wrapper (an aggregation, label_replace, …), lowering used to fall through
// the generic `lower()` → [lowerCall] path straight to
// [lowerOuterRangeFnOverSubquery]'s histogram-shape guard and reject.
// Cerberus issue #2569 threads the identical recognizers into that generic
// path (lower.go's [lowerCall], for the six float-shaped SELECT-family
// names) and into [lowerExpHistogramValuedShape] /
// [isExpHistogramValuedShape] (histogram_native_float_fn.go /
// histogram_native_scalar_binop.go, for the two histogram-preserving
// SELECT-family names last_over_time/first_over_time — the FOLD family was
// already threaded there for cerberus issue #2545).
//
// Each case reuses subquery_select_histogram_chdb_test.go's own
// subqSelectHistFixture: a SINGLE series ("a") with two published samples,
// (00:01:00, Count=2, Sum=4.0, Bucket1=6) and (00:02:00, Count=3, Sum=9.0,
// Bucket1=12). A single-series `sum by (series) (...)` is therefore an
// identity transform — the wrapped answer must equal the un-wrapped
// baseline the sibling file already pins — which is exactly the property
// each assertion below checks.
package promql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestSubqueryHistogramSelectFamilyNestedUnderSum_ChDB proves
// count_over_time (a float-valued SELECT-family member) still answers
// correctly when nested under `sum by (series) (...)` rather than being the
// query's own root — the issue's own trigger_query shape.
func TestSubqueryHistogramSelectFamilyNestedUnderSum_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "sum by (series) (count_over_time(("+metric+")[2m:1m]))", s, evalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 2 {
		t.Errorf("sum by (series) (count_over_time(...)): series a = %v, want 2 (single-series sum is an identity)", got["a"])
	}
}

// TestSubqueryHistogramSelectFamilyNestedUnderLabelReplace_ChDB proves the
// same count_over_time shape composes under label_replace — a
// non-aggregation wrapper, the issue's other named example.
func TestSubqueryHistogramSelectFamilyNestedUnderLabelReplace_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, `label_replace(count_over_time((`+metric+`)[2m:1m]), "extra", "yes", "", "")`, s, evalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 2 {
		t.Errorf("label_replace(count_over_time(...)): series a = %v, want 2", got["a"])
	}
}

// TestSubqueryHistogramFoldFamilyNestedUnderSum_ChDB proves sum_over_time
// (a histogram-PRESERVING FOLD-family member) still folds the subquery's
// two published histograms correctly when nested under `sum by (series)
// (...)`.
func TestSubqueryHistogramFoldFamilyNestedUnderSum_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "sum by (series) (sum_over_time(("+metric+")[2m:1m]))", s, evalTS)
	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("sum by (series) (sum_over_time(...)): got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 5 || got.sum != 13.0 || got.bucket1 != 18 {
		t.Errorf("sum by (series) (sum_over_time(...)) = %+v, want Count=5 Sum=13 Bucket1=18 (2+3, 4+9, 6+12)", got)
	}
}

// TestSubqueryHistogramFoldFamilyNestedUnderLabelReplace_ChDB proves the
// same sum_over_time shape composes under label_replace, preserving the
// folded histogram payload verbatim under the rewritten labels.
func TestSubqueryHistogramFoldFamilyNestedUnderLabelReplace_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, `label_replace(sum_over_time((`+metric+`)[2m:1m]), "extra", "yes", "", "")`, s, evalTS)
	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("label_replace(sum_over_time(...)): got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 5 || got.sum != 13.0 || got.bucket1 != 18 {
		t.Errorf("label_replace(sum_over_time(...)) = %+v, want Count=5 Sum=13 Bucket1=18", got)
	}
}

// TestSubqueryHistogramLastOverTimeNestedUnderSumAndLabelReplace_ChDB
// proves last_over_time — the histogram-PRESERVING half of the SELECT
// family, threaded through [lowerExpHistogramValuedShape] /
// [isExpHistogramValuedShape] rather than [lowerCall] (see
// [selectFnHistogramPreservingSubquery]'s own doc) — composes under both a
// `sum by (series)` wrapper and label_replace without hitting
// [projectAttributesOverInner]'s histogram-shape guard.
func TestSubqueryHistogramLastOverTimeNestedUnderSumAndLabelReplace_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "sum by (series) (last_over_time(("+metric+")[2m:1m]))", s, evalTS)
	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("sum by (series) (last_over_time(...)): got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 3 || got.sum != 9.0 || got.bucket1 != 12 {
		t.Errorf("sum by (series) (last_over_time(...)) = %+v, want Count=3 Sum=9 Bucket1=12 (the 00:02 sample)", got)
	}

	sqlStr, args = lowerAndEmit(t, `label_replace(last_over_time((`+metric+`)[2m:1m]), "extra", "yes", "", "")`, s, evalTS)
	rows = subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("label_replace(last_over_time(...)): got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 3 || got.sum != 9.0 || got.bucket1 != 12 {
		t.Errorf("label_replace(last_over_time(...)) = %+v, want Count=3 Sum=9 Bucket1=12", got)
	}
}
