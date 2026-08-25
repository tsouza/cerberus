//go:build chdb

// chDB-backed proof that cerberus issue #2563's eleven float-only
// range-vector functions — max_over_time, min_over_time, stddev_over_time,
// stdvar_over_time, quantile_over_time, mad_over_time, deriv,
// predict_linear, double_exponential_smoothing (holt_winters),
// ts_of_max_over_time, ts_of_min_over_time — over a subquery whose OWN
// inner already resolves histogram-native execute successfully against real
// ClickHouse and answer an EMPTY result, rather than the
// "unsupported"/502-shaped failure [lowerOuterRangeFnOverSubquery] used to
// raise for this shape (cerberus issue #2543's original guard). Reference
// Prometheus's own functions.go reads only `matrixVal[0].Floats` for every
// one of these, so an all-histogram window answers empty there too — this
// file is the round-trip evidence that cerberus's emitted SQL is well
// formed enough to actually reach and confirm that empty answer, not just
// that the Go plan shape looks right (TestLower_ExpHistogram_DropFamilyEmptyOverSubquery,
// histogram_native_subquery_select_test.go, pins the Go-shape half).
//
// Seeds the same two-sample histogram-only series
// subqSelectHistFixture already uses for the SELECT family
// (subquery_select_histogram_chdb_test.go) — no float samples at all — so
// the window every case below reduces is exclusively histogram-valued.
package promql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestSubqueryHistogramDropFamily_ChDB executes every one of the eleven
// float-only reducers over the shared histogram-only subquery fixture and
// asserts each answers zero rows rather than a query error.
func TestSubqueryHistogramDropFamily_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: "max_over_time((" + metric + ")[2m:1m])"},
		{name: "min_over_time", query: "min_over_time((" + metric + ")[2m:1m])"},
		{name: "stddev_over_time", query: "stddev_over_time((" + metric + ")[2m:1m])"},
		{name: "stdvar_over_time", query: "stdvar_over_time((" + metric + ")[2m:1m])"},
		{name: "mad_over_time", query: "mad_over_time((" + metric + ")[2m:1m])"},
		{name: "deriv", query: "deriv((" + metric + ")[2m:1m])"},
		{name: "ts_of_max_over_time", query: "ts_of_max_over_time((" + metric + ")[2m:1m])"},
		{name: "ts_of_min_over_time", query: "ts_of_min_over_time((" + metric + ")[2m:1m])"},
		{name: "quantile_over_time", query: "quantile_over_time(0.5, (" + metric + ")[2m:1m])"},
		{name: "predict_linear", query: "predict_linear((" + metric + ")[2m:1m], 60)"},
		{name: "holt_winters", query: "double_exponential_smoothing((" + metric + ")[2m:1m], 0.5, 0.5)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlStr, args := lowerAndEmit(t, tc.query, s, evalTS)
			got := sampleValueRows(t, fixture, sqlStr, args)
			if len(got) != 0 {
				t.Errorf("%s: got %d rows %+v, want 0 (all-histogram window has no float data for reference to answer)", tc.name, len(got), got)
			}
		})
	}
}
