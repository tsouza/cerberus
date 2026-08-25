//go:build chdb

// chDB-backed proof that cerberus issue #2589's fix actually executes
// correctly against real ClickHouse: a subquery whose own top-level inner
// is `and`/`unless` between a histogram-valued (or mixed float/histogram)
// operand and a plain float operand no longer discards the histogram
// payload through [subqueryAnchorShape]'s lossy four-column reprojection.
//
// Before this fix, `lowerSubqueryOverBinary` unconditionally wrapped
// `lowerBinary(b, s, grid)`'s result — which genuinely resolves to a
// [chplan.HistogramRowShape] / [chplan.MixedRowShape] node for this shape,
// via the generic [lowerVectorSetOp] / [lowerVectorSetOpOperand] path —
// through [subqueryAnchorShape], silently dropping the nine
// Histogram*Column outputs (or the Mixed discriminator) and leaving any
// consumer reading the meaningless placeholder Value column instead, with
// no error at all. Every case here proves the FIXED behaviour: real,
// correctly-valued histogram data read back from a real chDB execution,
// including a genuine per-anchor semantic difference (TestSubqueryUnless
// below) that only a truly per-anchor-correct join can reproduce.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// subqSetOpGaugeDDL declares otel_metrics_gauge/otel_metrics_sum exactly
// like histogram_native_float_vector_scaling_binop_swap_chdb_test.go's own
// swapGaugeSeedDDL — duplicated under a distinct name only so this file has
// no build-order dependency on that one.
const subqSetOpGaugeDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// TestSubqueryAndHistogramFloatBare_ChDB proves `(<exp-hist> and on(series)
// <gauge>)[range:step]` — the issue's own pure-histogram-vs-float example
// (cerberus issue #2337's semi-join shape) — under a bare top-level
// subquery, with no outer range-vector function. `and` forwards the LHS
// (histogram) row verbatim whenever the RHS also has a matching series at
// the SAME anchor; the gauge series here is present at both seeded
// anchors, so both histogram rows survive with their real payload intact.
func TestSubqueryAndHistogramFloatBare_ChDB(t *testing.T) {
	const histMetric = "subq_and_exp_hist"
	const gaugeMetric = "subq_and_gauge"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + histMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + histMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n" +
		subqSetOpGaugeDDL +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		"    ('" + gaugeMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 100.0),\n" +
		"    ('" + gaugeMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 200.0);\n"
	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	query := "(" + histMetric + " and on(series) " + gaugeMetric + ")[2m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 2 {
		t.Fatalf("%s: got %d rows, want 2 (one per subquery anchor, both matched by the gauge side) — a still-corrupted lowering would report the wrong shape or placeholder values here: %+v", query, len(rows), rows)
	}
	anchor1 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC))
	if anchor1.cnt != 2 || anchor1.sum != 4.0 || anchor1.bucket1 != 6 {
		t.Errorf("%s: anchor 00:01 = %+v, want Count=2 Sum=4 Bucket1=6 (the histogram side's own real payload)", query, anchor1)
	}
	anchor2 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC))
	if anchor2.cnt != 3 || anchor2.sum != 9.0 || anchor2.bucket1 != 12 {
		t.Errorf("%s: anchor 00:02 = %+v, want Count=3 Sum=9 Bucket1=12 (the histogram side's own real payload)", query, anchor2)
	}
}

// TestSubqueryUnlessHistogramFloatPerAnchor_ChDB proves the join is
// evaluated PER SUBQUERY ANCHOR, not once over the whole window: the gauge
// side only has a sample at the SECOND anchor, so `<hist> unless on(series)
// <gauge>` must keep the histogram row at the FIRST anchor (no matching
// gauge sample there) and drop it at the SECOND (a matching gauge sample
// exists). A silently-corrupted lowering that fell through to the raw
// four-column Sample quartet could not reproduce this per-anchor split at
// all — this is the "verify the ACTUAL data" proof, not merely "no error".
func TestSubqueryUnlessHistogramFloatPerAnchor_ChDB(t *testing.T) {
	const histMetric = "subq_unless_exp_hist"
	const gaugeMetric = "subq_unless_gauge"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + histMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + histMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n" +
		subqSetOpGaugeDDL +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		"    ('" + gaugeMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 200.0);\n"
	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	query := "(" + histMetric + " unless on(series) " + gaugeMetric + ")[2m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("%s: got %d rows, want exactly 1 (only the 00:01 anchor, where the gauge side has no matching sample): %+v", query, len(rows), rows)
	}
	got := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC))
	if got.cnt != 2 || got.sum != 4.0 || got.bucket1 != 6 {
		t.Errorf("%s: surviving anchor 00:01 = %+v, want Count=2 Sum=4 Bucket1=6", query, got)
	}
	for _, r := range rows {
		if r.ts == time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC).Unix() {
			t.Errorf("%s: anchor 00:02 must be DROPPED (gauge side has a matching series there) but a row survived: %+v", query, r)
		}
	}
}

// TestSubqueryMixedOrAndBare_ChDB proves the "third wrapper family" the
// issue names: a subquery's own inner is a mixed float/histogram `or`
// itself wrapped by a further `and` — `((<hist> or <other-gauge>) and
// on(series) <filter-gauge>)[range:step]`. `filter-gauge` only has a
// sample for the histogram side's series ("a"), so the outer `and` must
// preserve the histogram-arm row (real payload intact) while dropping the
// mixed-or's own float-arm row (series "b", no matching filter sample) —
// proving a genuinely [chplan.MixedRowShape] subquery inner composes
// through this shape without collapsing to the placeholder Value column.
func TestSubqueryMixedOrAndBare_ChDB(t *testing.T) {
	const histMetric = "subq_mixedand_exp_hist"
	const orGaugeMetric = "subq_mixedand_or_gauge"
	const filterGaugeMetric = "subq_mixedand_filter_gauge"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + histMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n" +
		subqSetOpGaugeDDL +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		"    ('" + orGaugeMetric + "', map('series', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 50.0),\n" +
		"    ('" + filterGaugeMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0);\n"
	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	query := "((" + histMetric + " or " + orGaugeMetric + ") and on(series) " + filterGaugeMetric + ")[1m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "a" {
			gotHist = true
			if r.cnt != 2 || r.sum != 4.0 || r.bucket1 != 6 {
				t.Errorf("%s: histogram-arm row = %+v, want Count=2 Sum=4 Bucket1=6 (the real payload, not the placeholder Value column)", query, r)
			}
		}
		if r.series == "b" {
			t.Errorf("%s: float-arm row for series b must be DROPPED by the outer `and` (no matching filter sample) but survived: %+v", query, r)
		}
	}
	if !gotHist {
		t.Fatalf("%s: no histogram-arm row for series a; got %+v", query, histRows)
	}
}
