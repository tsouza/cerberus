package gen

import (
	"strings"

	"github.com/tsouza/cerberus/test/property"
)

// This file builds the DETERMINISTIC duplicate-timestamp counter dataset
// that the cross-path rate() correctness proof
// (test/property/rate_dup_timestamp_test.go) runs against. Unlike the
// random MetricsDataset generator, this shape is fixed: it reproduces the
// exact bug class that PR fix(chsql) "dedup duplicate-timestamp samples in
// row-path extrapolated rate-family" closed, so the proof is reproducible
// and demonstrably RED on the unfixed emitter.

// CounterSumTableName is the OTel-CH default delta/cumulative-sum table.
// A metric name with the `_total` suffix routes here (not to the gauge
// table), which is what rate()/increase() over a counter must read.
const CounterSumTableName = "otel_metrics_sum"

// CounterDupMetricName is the counter the proof queries. The `_total`
// suffix routes it to CounterSumTableName via the schema heuristic.
const CounterDupMetricName = "http_requests_total"

// CounterDupEvalTsSec is the grid anchor (unix seconds) the proof
// evaluates at. The query_range grid is the single point [300, 300]; the
// [5m] matrix window is therefore the half-open interval (0, 300].
const CounterDupEvalTsSec = 300

// CounterDupRangeSelector is the [range] the proof's rate()/increase()
// query carries. 5m over the (0, 300] window.
const CounterDupRangeSelector = "5m"

// counterDupRangeSeconds is CounterDupRangeSelector ("5m") expressed in
// seconds — the divisor that turns the extrapolated increase into a
// per-second rate.
const counterDupRangeSeconds = 300

// Expected Prometheus-correct results for the (0, 300] window over the
// counter sampled at 60/10, 120/20, 180/30, 240/40 with 240/40
// DUPLICATED (the exact shape of test/spec/promql/
// increase_duplicate_timestamp_dedup.txtar):
//
//	distinct timestamps -> 4 samples, avg interval 60s, 1.1x = 66s.
//	  duration_to_start = 60s < 66s -> NOT capped; duration_to_end = 60s.
//	  increase = 30 * (180 + 60 + 60) / 180 = 50          (deduped, correct)
//	  rate     = 50 / 300                    = 0.16666…   (increase / window_s)
//
//	dup-inflated -> 5 samples, avg interval 45s, 1.1x = 49.5s.
//	  duration_to_start = 60s >= 49.5s -> capped to 22.5s (same for end).
//	  increase = 30 * (180 + 22.5 + 22.5) / 180 = 37.5    (WRONG)
//
// The proof asserts the deduped values; on the unfixed row-path emitter
// cerberus returns the dup-inflated ones, so the row-path subtests fail.
const (
	CounterDupExpectedIncrease = 50.0
	CounterDupExpectedRate     = CounterDupExpectedIncrease / counterDupRangeSeconds
)

// counterDupLabels is the single series identity the dataset carries.
var counterDupLabels = map[string]string{"job": "api"}

// CounterDupTimestampDataset returns the deterministic duplicate-timestamp
// counter dataset. The Metrics mirror is the single source of truth: it
// carries the duplicate (Attributes, TimeUnix) sample (240s/40 twice), and
// the DDL renders that mirror faithfully so chDB sees the duplicate row.
// The oracle's FromDataset collapses the duplicate per Prometheus's
// one-sample-per-timestamp invariant, so the oracle holds the correct
// 4-sample series while cerberus must derive the same answer from the
// 5-row table.
func CounterDupTimestampDataset() property.Dataset {
	// (timestampSeconds, value) — a monotonic counter, last point dup'd.
	raw := []struct {
		tsSec int64
		value float64
	}{
		{60, 10},
		{120, 20},
		{180, 30},
		{240, 40},
		{240, 40}, // duplicate (Attributes, TimeUnix): the bug trigger.
	}
	points := make([]property.Point, 0, len(raw))
	for _, r := range raw {
		points = append(points, property.Point{TimestampMs: r.tsSec * 1000, Value: r.value})
	}
	series := []property.SeriesData{{
		MetricName: CounterDupMetricName,
		Labels:     counterDupLabels,
		Points:     points,
	}}
	return property.Dataset{
		DDL:     renderCounterDupDDL(series),
		Metrics: &property.MetricsModel{Series: series},
	}
}

// renderCounterDupDDL renders the sum-table seed script for the counter
// series. It reuses renderRow / renderMap / formatFloat (the gauge
// renderer's row helpers — the four-column positional VALUES shape is
// identical). The CREATE omits the ResourceAttributes column and uses a
// positional VALUES list; the chDB seed harness's backfillResourceAttributes
// injects `ResourceAttributes Map DEFAULT map()` and the bare CREATE TABLE
// is promoted to CREATE OR REPLACE, exactly as the spec roundtrip harness
// does — so the read path's resource-attribute merge arm resolves and the
// merged label set collapses to Attributes alone (matching the oracle).
func renderCounterDupDDL(series []property.SeriesData) string {
	var b strings.Builder
	b.WriteString(`CREATE TABLE `)
	b.WriteString(CounterSumTableName)
	b.WriteString(` (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`)
	for _, s := range series {
		b.WriteString(`INSERT INTO `)
		b.WriteString(CounterSumTableName)
		b.WriteString(` (MetricName, Attributes, TimeUnix, Value) VALUES `)
		for i, p := range s.Points {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(renderRow(s.MetricName, s.Labels, p))
		}
		b.WriteString(";\n")
	}
	return b.String()
}
