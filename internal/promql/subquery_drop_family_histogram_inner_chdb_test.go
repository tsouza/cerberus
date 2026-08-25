//go:build chdb

// chDB-backed proof that cerberus issue #2590's eleven float-only
// range-vector functions — max_over_time, min_over_time, stddev_over_time,
// stdvar_over_time, quantile_over_time, mad_over_time, deriv,
// predict_linear, double_exponential_smoothing (holt_winters),
// ts_of_max_over_time, ts_of_min_over_time — execute successfully against
// real ClickHouse and answer an EMPTY result, rather than
// expHistogramSelectorRouting's hard rejection, when the function call
// itself is a bare SUBQUERY's own inner expression
// (`max_over_time(m[2m])[2m:1m]`) rather than the OUTER wrapper around an
// already-histogram-native subquery
// (`max_over_time((m)[2m:1m])`, subquery_drop_family_histogram_chdb_test.go,
// cerberus issue #2563).
//
// Reuses the identical two-sample, histogram-only fixture
// subqSelectHistFixture already seeds for the SELECT family
// (subquery_select_histogram_chdb_test.go) and the sibling OUTER-wraps
// test above — no float samples at all — so every anchor the outer `[2m:1m]`
// subquery grid evaluates the inner function's own 2m lookback window
// against is exclusively histogram-valued.
package promql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestSubqueryHistogramDropFamilyInner_ChDB executes every one of the
// eleven float-only reducers as a bare subquery's own inner expression
// over the shared histogram-only fixture and asserts each answers zero
// rows rather than a query error.
func TestSubqueryHistogramDropFamilyInner_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: "max_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "min_over_time", query: "min_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "stddev_over_time", query: "stddev_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "stdvar_over_time", query: "stdvar_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "mad_over_time", query: "mad_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "deriv", query: "deriv(" + metric + "[2m])[2m:1m]"},
		{name: "ts_of_max_over_time", query: "ts_of_max_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "ts_of_min_over_time", query: "ts_of_min_over_time(" + metric + "[2m])[2m:1m]"},
		{name: "quantile_over_time", query: "quantile_over_time(0.5, " + metric + "[2m])[2m:1m]"},
		{name: "predict_linear", query: "predict_linear(" + metric + "[2m], 60)[2m:1m]"},
		{name: "holt_winters", query: "double_exponential_smoothing(" + metric + "[2m], 0.5, 0.5)[2m:1m]"},
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
