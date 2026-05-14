// Package gen holds the rapid generators for the property-testing
// framework. Each generator is split into its own file: metrics.go
// produces the random dataset, promql.go produces a query targeted at
// that dataset.
package gen

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// MetricNamePool is the fixed metric-name pool for PR 1. Both names
// avoid the `_count` / `_total` / `_sum` / `_bucket` suffixes the OTel
// schema heuristic in [schema.Metrics.TableFor] routes to the
// otel_metrics_sum table — that keeps every generated series in the
// gauge table and matches the DDL the dataset generator emits.
//
// `http_requests_total` is intentionally NOT in the pool for PR 1 even
// though the original investigation plan listed it: with the gauge-only
// DDL the generator emits, that name would route to the (empty) sum
// table and every query against it would return zero series — a
// near-100% trivial-accept rate that wastes iterations.
var MetricNamePool = []string{"up", "temperature"}

// LabelNamePool is the fixed label-name pool. Two pure label names
// (no instrumentation-side semantics) keep series counts predictable.
var LabelNamePool = []string{"job", "instance"}

// LabelValuesByName is a name → values map for the label pool. The
// per-name value pools are intentionally small so the rapid generator
// produces overlap (same label values across series), giving aggregate
// queries something interesting to roll up.
var LabelValuesByName = map[string][]string{
	"job":      {"api", "web", "batch"},
	"instance": {"a", "b", "c"},
}

// anchorTime is the timestamp the generator anchors all series to.
// Fixed so each rapid iteration produces the same wall-clock baseline
// and the failure log's `evalTs=` value is comparable across runs.
//
// 2026-05-13T12:00:00Z is a round timestamp far enough in the future
// to avoid colliding with any wall-clock assertion in the chDB seeds.
var anchorTime = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// GaugeTableName is the OTel-CH default gauge table. The DDL the
// generator emits targets this name; the chDB session and the SQL
// cerberus emits both expect it.
const GaugeTableName = "otel_metrics_gauge"

// MetricsDataset returns a rapid generator that draws a random
// property.Dataset. The generator's accept-set is intentionally narrow
// for PR 1:
//
//   - 1–5 series.
//   - Each series carries 1–3 labels from {job, instance}.
//   - Per series, 1–10 points spaced at 15-second intervals from a
//     fixed anchor.
//   - Values are uniform-like floats in [0, 100], drawn from a small
//     pool so rate / sum aggregations produce comparable numbers.
//   - Gauge only — no histograms, summaries, or sums. The bridge
//     oracle and cerberus's gauge path are both well-trodden; the
//     wider shapes land in PR 2 along with the from-scratch oracle.
//
// The generator returns a Dataset whose DDL is a multi-statement
// script (CREATE OR REPLACE TABLE + per-series INSERTs) and whose
// Metrics mirror is in lock-step.
func MetricsDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numSeries := rapid.IntRange(1, 5).Draw(t, "numSeries")
		seen := map[string]struct{}{}

		series := make([]property.SeriesData, 0, numSeries)
		for i := 0; i < numSeries; i++ {
			name := rapid.SampledFrom(MetricNamePool).Draw(t, fmt.Sprintf("name_%d", i))
			lset := drawLabelSet(t, fmt.Sprintf("labels_%d", i))
			key := name + labelKey(lset)
			if _, dup := seen[key]; dup {
				// Skip duplicates so the generator's label-set
				// uniqueness invariant holds — promql parsers and
				// SampleStore both key by labelset hash, and two
				// series with the same hash would collapse.
				continue
			}
			seen[key] = struct{}{}

			points := drawPoints(t, fmt.Sprintf("points_%d", i))
			series = append(series, property.SeriesData{
				MetricName: name,
				Labels:     lset,
				Points:     points,
			})
		}

		return property.Dataset{
			DDL:     renderDDL(series),
			Metrics: &property.MetricsModel{Series: series},
		}
	})
}

// drawLabelSet picks a 1–3 label subset from LabelNamePool and assigns
// each a random value from LabelValuesByName. The label-name draw is
// stable (sorted) so shrinking has fewer redundant attempts.
func drawLabelSet(t *rapid.T, id string) map[string]string {
	count := rapid.IntRange(1, len(LabelNamePool)).Draw(t, id+"_count")
	// Pick the first `count` label names in sorted order. Choosing
	// the order rather than the subset keeps the shrinker focused
	// (count goes down, names don't reshuffle).
	names := append([]string(nil), LabelNamePool...)
	sort.Strings(names)
	picked := names[:count]
	out := make(map[string]string, len(picked))
	for _, name := range picked {
		values := LabelValuesByName[name]
		v := rapid.SampledFrom(values).Draw(t, id+"_"+name)
		out[name] = v
	}
	return out
}

// drawPoints emits 1–10 monotonically-timestamped samples per series.
// Timestamps are anchorTime + 15s * i so the bridge oracle's
// LookbackDelta (5m) sees every recent point during instant
// evaluation. Values are drawn from a fixed float pool — keeping the
// pool small means the comparator never has to deal with rounding
// noise from rapid's float generators.
func drawPoints(t *rapid.T, id string) []property.Point {
	count := rapid.IntRange(1, 10).Draw(t, id+"_count")
	step := 15 * time.Second
	out := make([]property.Point, 0, count)
	for i := 0; i < count; i++ {
		ts := anchorTime.Add(time.Duration(i) * step).UnixMilli()
		v := rapid.SampledFrom([]float64{0, 0.5, 1, 2.5, 10, 23, 60, 99}).Draw(t, fmt.Sprintf("%s_v_%d", id, i))
		out = append(out, property.Point{TimestampMs: ts, Value: v})
	}
	return out
}

// renderDDL produces the multi-statement seed script for series.
//
// Statements:
//
//   - One `CREATE OR REPLACE TABLE otel_metrics_gauge (...) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);`
//   - One `INSERT INTO otel_metrics_gauge VALUES (...), (...), ...;`
//     per series, with each row encoded as
//     `(name, map(...), toDateTime64('...'), value)`.
//
// `CREATE OR REPLACE TABLE` keeps re-runs inside the same chDB process
// idempotent (chdb-go shares one catalog across sessions in v1.11.0).
//
// MergeTree (not Memory) is the chosen engine because cerberus's
// optimizer emits PREWHERE clauses on aggregate-over-filter shapes
// (the promotion rule fires unconditionally), and chDB's Memory
// engine returns ILLEGAL_PREWHERE on those queries. MergeTree
// supports PREWHERE the way every real cerberus deployment does, so
// the property test matches what production CH does. ORDER BY
// (MetricName, TimeUnix) gives the table a sort key without forcing
// a primary key (which would constrain INSERT order).
func renderDDL(series []property.SeriesData) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(GaugeTableName)
	b.WriteString(` (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);
`)
	for _, s := range series {
		b.WriteString(`INSERT INTO `)
		b.WriteString(GaugeTableName)
		b.WriteString(` VALUES `)
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

// renderRow renders one `(name, map(...), toDateTime64('...', 9),
// value)` literal row.
func renderRow(name string, labels map[string]string, p property.Point) string {
	var b strings.Builder
	b.WriteByte('(')
	b.WriteByte('\'')
	b.WriteString(name)
	b.WriteByte('\'')
	b.WriteString(", ")
	b.WriteString(renderMap(labels))
	b.WriteString(", toDateTime64('")
	// chdb-go accepts 'YYYY-MM-DD HH:MM:SS.nnn' wall-clock literals
	// with the toDateTime64(..., 9) cast. Use millisecond precision —
	// the generator's 15s spacing is well above the resolution chDB
	// stores at, so trailing-zero noise doesn't move the comparator.
	ts := time.UnixMilli(p.TimestampMs).UTC().Format("2006-01-02 15:04:05.000")
	b.WriteString(ts)
	b.WriteString("', 9), ")
	b.WriteString(formatFloat(p.Value))
	b.WriteByte(')')
	return b.String()
}

// renderMap renders a label set as a CH `map('k1','v1', 'k2','v2')`
// expression. Sorted keys for determinism.
func renderMap(labels map[string]string) string {
	if len(labels) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('\'')
		b.WriteString(k)
		b.WriteString("', '")
		b.WriteString(labels[k])
		b.WriteByte('\'')
	}
	b.WriteByte(')')
	return b.String()
}

// formatFloat renders a Go float64 in a CH-friendly textual form. The
// generator's value pool is small and integer-valued, so %g produces
// short, unambiguous literals; CH parses both `1.0` and `1` as
// Float64 in this context.
func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}

// labelKey produces a stable string key from a label set for the
// dedup pass in MetricsDataset. Not exported; the framework's
// labelKey serves the comparator side.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// AnchorTime is the fixed wall-clock baseline the dataset generator
// anchors series timestamps to. Exported so the PromQL generator can
// choose an EvalTs from inside the active window.
func AnchorTime() time.Time { return anchorTime }
