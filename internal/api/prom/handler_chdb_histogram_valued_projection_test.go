//go:build chdb

package prom_test

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestHistogramProjection_RuntimeColumnTypes_ChDB pins the half of the
// decode contract that neither the emitter's own tests nor the
// scan-contract test can see: what ClickHouse actually REPORTS for the
// nine output columns.
//
// The emitter's casts are pinned as SQL TEXT (chsql's shape test and the
// `-- sql --` cells of test/spec/promql/exp_histogram_*.txtar), and
// internal/chclient's scan-contract test pins that each named type scans
// into its HistogramValue field. Neither executes the expression. That
// matters because the chDB-backed handler tests CANNOT catch a reverted
// cast on their own: chdb-go's driver coerces an int64 or a text-rendered
// array into the destination, so a UInt64 HistogramCount would still
// decode here while failing on a real ClickHouse — the exact
// chDB-coerces / prod-is-strict split that produced #1967.
//
// Asserting toTypeName over the emitted SQL closes that: chDB is
// ClickHouse's own engine, so its type inference IS ClickHouse's.
func TestHistogramProjection_RuntimeColumnTypes_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	// promql.Lower anchors an instant query to now64(), so this fixture
	// is seeded RELATIVE TO NOW rather than reusing histValuedSeed's
	// fixed 2026-01-01 rows — those fall outside the lookback and the
	// projection would report types over an empty result. Two scrapes
	// 60s apart keep rate()'s two-sample floor satisfied.
	c.Seed(t, histValuedDDL+`
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('typecheck_exp_hist', map('service', 'api'), now64(9) - toIntervalSecond(60),  6,  3.0, 0, 1, 0, [ 2,  3], 0, []),
    ('typecheck_exp_hist', map('service', 'api'), now64(9),                        30, 15.0, 0, 1, 0, [14, 15], 0, []);`)

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// The pinned type of each of the nine outputs, in projection order.
	wantTypes := []string{
		"Float64", // HistogramCount
		"Float64", // HistogramSum
		"Int32",   // HistogramScale
		"Float64", // HistogramZeroThreshold
		"Float64", // HistogramZeroCount
		"Int32",   // HistogramPositiveOffset
		"Array(Float64)",
		"Int32", // HistogramNegativeOffset
		"Array(Float64)",
	}

	// Both shapes matter: a bare selector forwards the physical OTel-CH
	// columns (UInt64 / Array(UInt64)) and rate() derives Float64 ones,
	// so before the pin these two disagreed. They must now report the
	// SAME nine types.
	for _, tc := range []struct{ name, query string }{
		{"bare selector", "typecheck_exp_hist"},
		{"rate", "rate(typecheck_exp_hist[5m])"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			plan, err := promql.Lower(t.Context(), expr, schema.DefaultOTelMetrics())
			if err != nil {
				t.Fatalf("lower %q: %v", tc.query, err)
			}
			sql, args, err := chsql.Emit(t.Context(), plan)
			if err != nil {
				t.Fatalf("emit %q: %v", tc.query, err)
			}

			// Wrap the emitted SQL without touching its parameter
			// order, so the recorded args still bind positionally.
			var sb strings.Builder
			sb.WriteString("SELECT arrayStringConcat([")
			for i, col := range histogramOutputColumns {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("toTypeName(`" + col + "`)")
			}
			sb.WriteString("], '|') FROM (")
			sb.WriteString(sql)
			sb.WriteString(") LIMIT 1")

			got, err := c.QueryStrings(t.Context(), sb.String(), args...)
			if err != nil {
				t.Fatalf("query column types: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d type rows, want 1 (is the seeded row in the query window?)", len(got))
			}
			gotTypes := strings.Split(got[0], "|")
			if len(gotTypes) != len(wantTypes) {
				t.Fatalf("got %d types %v, want %d", len(gotTypes), gotTypes, len(wantTypes))
			}
			for i := range wantTypes {
				if gotTypes[i] != wantTypes[i] {
					t.Errorf("%s reports %s, want %s", histogramOutputColumns[i], gotTypes[i], wantTypes[i])
				}
			}
		})
	}
}

// histogramOutputColumns is the projection order chplan.HistogramProjection
// publishes its nine outputs in.
var histogramOutputColumns = []string{
	"HistogramCount",
	"HistogramSum",
	"HistogramScale",
	"HistogramZeroThreshold",
	"HistogramZeroCount",
	"HistogramPositiveOffset",
	"HistogramPositiveBucketCounts",
	"HistogramNegativeOffset",
	"HistogramNegativeBucketCounts",
}
