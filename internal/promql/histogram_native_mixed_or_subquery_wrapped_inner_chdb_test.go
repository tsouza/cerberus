//go:build chdb

// chDB-backed proof that cerberus issue #2581's fix actually executes
// correctly against real ClickHouse: `<fn>((label_replace((h) or (f),
// ...))[range:step])` for a SELECT-family member (count_over_time) and a
// FOLD-family member (sum_over_time) — the label_replace-wrapped-inner
// sibling of #2577's own bare-inner proof
// (histogram_native_mixed_or_subquery_range_fn_chdb_test.go), reusing that
// file's own seed/fixture/helpers so the assertions can be checked against
// the identical baseline numbers.
package promql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// wrappedMorQuery builds `<outer>(<fn>((label_replace((h) or (f), "extra",
// "yes", "", ""))[2m:1m]))` — morQuery's own label_replace-wrapped-inner
// sibling (histogram_native_mixed_or_subquery_range_fn_chdb_test.go). The
// label_replace call adds a constant "extra" label without touching
// "series", so every assertion below reads back against the SAME per-series
// values morQuery's own tests pin.
func wrappedMorQuery(outerFmt, fn string) string {
	subExpr := `(label_replace((` + morExpHistMetric + `) or (` + morGaugeMetric + `), "extra", "yes", "", ""))`
	inner := fn + "(" + subExpr + "[2m:1m])"
	if outerFmt == "" {
		return inner
	}
	return outerFmt + "(" + inner + ")"
}

// TestMixedOrSubqueryCountOverTimeWrappedInner_ChDB proves count_over_time
// (a SELECT-family, always-float-output member) composes over a subquery
// whose own inner is a mixed float/histogram `or` DIRECTLY wrapped in
// label_replace — cerberus issue #2581's own evidence shape. Both arms'
// windows hold exactly two in-window samples, matching
// TestMixedOrSubqueryCountOverTimeNestedUnderSum_ChDB's own bare-inner
// baseline.
func TestMixedOrSubqueryCountOverTimeWrappedInner_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := wrappedMorQuery("", "count_over_time")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["h"] != 2 {
		t.Errorf("count_over_time over label_replace-wrapped mixed-or subquery: series h = %v, want 2", got["h"])
	}
	if got["f"] != 2 {
		t.Errorf("count_over_time over label_replace-wrapped mixed-or subquery: series f = %v, want 2", got["f"])
	}
}

// TestMixedOrSubquerySumOverTimeWrappedInner_ChDB proves sum_over_time — a
// FOLD-family, histogram-preserving member whose output is a genuinely
// [chplan.MixedRowShape] node — composes correctly over the same
// label_replace-wrapped inner, reading back BOTH arms: the histogram-arm
// series ("h") folds its two published histograms exactly as
// TestMixedOrSubquerySumOverTimeNestedUnderLabelReplace_ChDB's own
// bare-inner baseline does (Count=5, Sum=13, Bucket1=18); the float-arm
// series ("f") sums its two plain gauge samples (10+20=30).
func TestMixedOrSubquerySumOverTimeWrappedInner_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := wrappedMorQuery("", "sum_over_time")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt != 5 || r.sum != 13.0 || r.bucket1 != 18 {
				t.Errorf("sum_over_time over label_replace-wrapped mixed-or subquery, histogram arm = %+v, want Count=5 Sum=13 Bucket1=18", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("sum_over_time over label_replace-wrapped mixed-or subquery: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] != 30 {
		t.Errorf("sum_over_time over label_replace-wrapped mixed-or subquery, float arm: series f = %v, want 30 (10+20)", got["f"])
	}
}

// TestMixedOrSubqueryRateWrappedInner_ChDB proves rate — the FOLD family's
// boundary-extrapolated member — composes over the label_replace-wrapped
// inner at the query root (unwrapped), mirroring
// TestMixedOrSubqueryRate_ChDB's own bare-inner assertions.
func TestMixedOrSubqueryRateWrappedInner_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := wrappedMorQuery("", "rate")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt <= 0 {
				t.Errorf("rate over label_replace-wrapped mixed-or subquery, histogram arm = %+v, want a positive Count", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("rate over label_replace-wrapped mixed-or subquery: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] <= 0 {
		t.Errorf("rate over label_replace-wrapped mixed-or subquery, float arm: series f = %v, want a positive rate", got["f"])
	}
}
