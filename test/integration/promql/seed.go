//go:build chdb

// Package promql holds the chDB-backed exotic-PromQL integration suite.
//
// The suite reuses the property-test machinery verbatim: it seeds ONE
// rich, deterministic OTel fixture into an ephemeral chDB session, mounts
// the real prom.Handler behind an httptest server, and for every query in
// the hand-curated exotic matrix asserts that cerberus's lowered+executed
// SQL agrees with the from-scratch oracle (test/property/oracle/promql).
//
// Unlike the random property generator, the dataset here is FIXED — a
// mirror of the prometheus/compliance reference fixture
// (juliusv/prometheus_demo_service, namespace `demo`) that the official
// PromQL conformance suite was written against. The fixed fixture lets the
// matrix exercise exotic constructs (binary-op-over-rate, histogram_quantile,
// on()/group_left, subqueries, @ modifier, ...) against data purpose-built
// for them.
//
// # Seed <-> Model lockstep
//
// RichSeed returns BOTH a chDB DDL+INSERT script AND a parallel in-memory
// *property.MetricsModel. The DDL is what cerberus reads (via chDB); the
// model is what the oracle reads. The two MUST encode identical (name,
// labels, points) series or the comparison is meaningless — RichSeed owns
// that invariant by deriving both from the same generators.
//
// The trickiest part is the classic histogram. cerberus stores per-bucket
// DELTA counts in otel_metrics_histogram.BucketCounts with finite
// ExplicitBounds, then synthesizes cumulative `<base>_bucket{le=...}`
// series at query time via arraySum(arraySlice(BucketCounts, 1, i)) (see
// internal/promql/histogram_bucket.go::wrapHistogramBucketFanout). The
// oracle, by contrast, reads plain series out of the model. So RichSeed
// emits the histogram into chDB as delta BucketCounts, and into the model
// as the equivalent CUMULATIVE `_bucket{le}` / `_count` / `_sum` series —
// computed by prefixSum below. exotic_test.go validates this lockstep with
// a histogram_quantile self-check before trusting the rest of the matrix.
package promql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

// nowAnchor is the wall-clock baseline every series is anchored to. It
// matches runner_chdb.go's defaultNowAnchor (2026-01-01T00:00:01Z) so any
// now()/now64() substitution in the lowering aligns with the oracle's eval
// grid. Series run forward from here at stepInterval spacing.
var nowAnchor = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// stepInterval is the sample spacing. 15s matches the reference fixture's
// scrape resolution; with seedSteps samples a series spans seedSteps*15s.
const stepInterval = 15 * time.Second

// seedSteps is the number of samples per (series, step) pair. 40 * 15s =
// 10 minutes of data — wide enough for 5m rate windows, [30m:1m]-style
// subqueries clamp to the data window, and the 5m lookback to always find
// a fresh sample near the end of the window.
const seedSteps = 40

// instances are the three scrape targets. Mirroring the reference fixture's
// three service.instance.id values gives a real cross-target dimension for
// on(instance)/group_left() joins.
var instances = []string{
	"demo.promlabs.com:10000",
	"demo.promlabs.com:10001",
	"demo.promlabs.com:10002",
}

// histoBounds are the finite explicit bucket bounds (le) for the classic
// histogram. The trailing +Inf overflow bucket is implicit (BucketCounts
// has one more entry than ExplicitBounds). A small, hand-picked set keeps
// the cumulative arithmetic auditable; the design's 25-bucket exponential
// ladder is unnecessary for catching lowering bugs and would make the
// model harder to verify by eye.
var histoBounds = []float64{0.1, 0.5, 1, 2.5}

// EvalTs is the canonical instant every matrix query evaluates at unless it
// overrides. It sits near the end of the data window so rate/increase have
// a full 5m window of samples and the instant selector finds a fresh point.
var EvalTs = nowAnchor.Add(time.Duration(seedSteps-1) * stepInterval).Unix()

// RichSeed builds the fixed fixture. It returns the multi-statement chDB
// DDL+INSERT script and the parallel in-memory model the oracle reads. The
// two are derived from the same per-series generators so they stay in
// lockstep by construction.
func RichSeed() (ddl string, model *property.MetricsModel) {
	var b strings.Builder
	b.WriteString(gaugeDDL())
	b.WriteString(sumDDL())
	b.WriteString(histogramDDL())

	var series []property.SeriesData

	// --- Counters (otel_metrics_sum), with a reset partway through. ---
	// demo_cpu_usage_seconds_total{instance,mode}: monotonic per (instance,
	// mode), distinct slopes so rate()/by() roll-ups differ across series.
	for i, inst := range instances {
		for mi, mode := range []string{"user", "system", "idle"} {
			lbls := map[string]string{"instance": inst, "job": "demo", "mode": mode}
			slope := float64(1+mi) + float64(i)*0.5
			vals := counterValues(slope, false)
			b.WriteString(sumInsert("demo_cpu_usage_seconds_total", lbls, vals))
			series = append(series, mkSeries("demo_cpu_usage_seconds_total", lbls, vals))
		}
	}
	// demo_items_shipped_total{instance}: a monotonic counter, one series
	// per instance (so on(instance)/group_left joins resolve one-to-one).
	// It is kept monotonic so rate() agrees byte-for-byte between cerberus
	// and the oracle; counter-RESET behaviour (rate across a restart +
	// resets()) is exercised separately by the deterministic promqltest
	// tail (testdata/resets.test) where the expected values are pinned.
	for i, inst := range instances {
		lbls := map[string]string{"instance": inst, "job": "demo"}
		slope := 5 + float64(i)
		vals := counterValues(slope, false)
		b.WriteString(sumInsert("demo_items_shipped_total", lbls, vals))
		series = append(series, mkSeries("demo_items_shipped_total", lbls, vals))
	}

	// --- Gauges (otel_metrics_gauge). ---
	// demo_memory_usage_bytes{instance,type}: the group_left pivot.
	for i, inst := range instances {
		for ti, typ := range []string{"used", "cached", "buffers", "free"} {
			lbls := map[string]string{"instance": inst, "job": "demo", "type": typ}
			base := 2e9 + float64(i)*1e7 + float64(ti)*1e6
			vals := gaugeRamp(base, 1024)
			b.WriteString(gaugeInsert("demo_memory_usage_bytes", lbls, vals))
			series = append(series, mkSeries("demo_memory_usage_bytes", lbls, vals))
		}
	}
	// demo_disk_usage_bytes / demo_disk_total_bytes{instance,device}.
	for i, inst := range instances[:2] {
		for di, dev := range []string{"/dev/sda1", "/dev/sda2"} {
			lbls := map[string]string{"instance": inst, "job": "demo", "device": dev}
			usage := gaugeRamp(10e9+float64(i)*4096, float64(di+1)*1024)
			b.WriteString(gaugeInsert("demo_disk_usage_bytes", lbls, usage))
			series = append(series, mkSeries("demo_disk_usage_bytes", lbls, usage))

			total := gaugeConst(100e9)
			b.WriteString(gaugeInsert("demo_disk_total_bytes", lbls, total))
			series = append(series, mkSeries("demo_disk_total_bytes", lbls, total))
		}
	}
	// demo_num_cpus{instance}: constant gauge (4 cores).
	for _, inst := range instances {
		lbls := map[string]string{"instance": inst, "job": "demo"}
		vals := gaugeConst(4)
		b.WriteString(gaugeInsert("demo_num_cpus", lbls, vals))
		series = append(series, mkSeries("demo_num_cpus", lbls, vals))
	}
	// demo_intermittent_metric{instance}: SPARSE — only every 3rd step
	// carries a value, so the 5m lookback turns gaps into "no value" and
	// count_over_time/absent_over_time see fewer samples than a dense
	// series would.
	for i, inst := range instances {
		lbls := map[string]string{"instance": inst, "job": "demo"}
		vals := sparseValues(float64(i + 1))
		b.WriteString(gaugeInsert("demo_intermittent_metric", lbls, vals))
		series = append(series, mkSeries("demo_intermittent_metric", lbls, vals))
	}

	// --- Classic histogram (otel_metrics_histogram). ---
	// demo_api_request_duration_seconds{method,path,status}. Each series
	// gets a monotonically growing per-bucket delta vector; the model
	// mirrors the cumulative _bucket{le}/_count/_sum companions cerberus
	// synthesizes at query time.
	histLabelSets := []map[string]string{
		{"method": "GET", "path": "/api", "status": "200"},
		{"method": "POST", "path": "/api", "status": "500"},
		{"method": "GET", "path": "/web", "status": "200"},
	}
	for si, base := range histLabelSets {
		lbls := map[string]string{"job": "demo"}
		for k, v := range base {
			lbls[k] = v
		}
		b.WriteString(histogramInsert("demo_api_request_duration_seconds", lbls, si))
		series = append(series, histogramModelSeries("demo_api_request_duration_seconds", lbls, si)...)
	}

	return b.String(), &property.MetricsModel{Series: series}
}

// ---------------------------------------------------------------------
// Value generators. Each returns one Point per step so the chDB INSERT and
// the model series consume the same slice.
// ---------------------------------------------------------------------

// counterValues produces a monotonically increasing counter at the given
// per-step slope. When reset is true the counter drops back to a small
// value at the midpoint, modeling a process restart that rate()/increase()
// must bridge and resets() must count.
func counterValues(slope float64, reset bool) []property.Point {
	out := make([]property.Point, 0, seedSteps)
	for i := 0; i < seedSteps; i++ {
		v := slope * float64(i)
		if reset && i >= seedSteps/2 {
			v = slope * float64(i-seedSteps/2)
		}
		out = append(out, property.Point{TimestampMs: stepTsMs(i), Value: v})
	}
	return out
}

// gaugeRamp produces a gauge that rises linearly from base by inc per step.
func gaugeRamp(base, inc float64) []property.Point {
	out := make([]property.Point, 0, seedSteps)
	for i := 0; i < seedSteps; i++ {
		out = append(out, property.Point{TimestampMs: stepTsMs(i), Value: base + float64(i)*inc})
	}
	return out
}

// gaugeConst produces a constant gauge.
func gaugeConst(v float64) []property.Point {
	out := make([]property.Point, 0, seedSteps)
	for i := 0; i < seedSteps; i++ {
		out = append(out, property.Point{TimestampMs: stepTsMs(i), Value: v})
	}
	return out
}

// sparseValues emits a value only on every 3rd step — the gaps model an
// intermittent scrape target. The non-emitted steps simply produce no
// Point (and no chDB row), so the 5m lookback resolves them to "stale".
func sparseValues(scale float64) []property.Point {
	out := make([]property.Point, 0, seedSteps/3+1)
	for i := 0; i < seedSteps; i += 3 {
		out = append(out, property.Point{TimestampMs: stepTsMs(i), Value: scale * float64(i)})
	}
	return out
}

// bucketDeltas returns the per-step, per-bucket DELTA counts for histogram
// series si. There are len(histoBounds)+1 buckets (the trailing one is the
// +Inf overflow). Values grow with the step index so per-anchor rate
// windows differ across the grid.
func bucketDeltas(si, step int) []uint64 {
	out := make([]uint64, len(histoBounds)+1)
	for b := range out {
		// All inputs are small non-negative ints (bucket index, step,
		// series index), so the sum is always a small positive value; the
		// max() floor makes the non-negativity explicit for gosec.
		out[b] = uint64(max((b+1)*2+step+si, 0))
	}
	return out
}

// ---------------------------------------------------------------------
// chDB DDL + INSERT rendering.
// ---------------------------------------------------------------------

func gaugeDDL() string {
	return `CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);
`
}

func sumDDL() string {
	return `CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32 DEFAULT 2,
    IsMonotonic Bool DEFAULT true
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);
`
}

func histogramDDL() string {
	return `CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);
`
}

func gaugeInsert(name string, lbls map[string]string, points []property.Point) string {
	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ")
	for i, p := range points {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "('%s', %s, %s, %s)",
			name, renderMap(lbls), tsLiteral(p.TimestampMs), formatFloat(p.Value))
	}
	b.WriteString(";\n")
	return b.String()
}

func sumInsert(name string, lbls map[string]string, points []property.Point) string {
	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES ")
	for i, p := range points {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "('%s', %s, %s, %s)",
			name, renderMap(lbls), tsLiteral(p.TimestampMs), formatFloat(p.Value))
	}
	b.WriteString(";\n")
	return b.String()
}

func histogramInsert(name string, lbls map[string]string, si int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES ")
	for step := 0; step < seedSteps; step++ {
		if step > 0 {
			b.WriteString(", ")
		}
		deltas := bucketDeltas(si, step)
		var count uint64
		for _, d := range deltas {
			count += d
		}
		sum := histogramSum(deltas)
		fmt.Fprintf(&b, "('%s', %s, %s, %d, %s, %s, %s)",
			name, renderMap(lbls), tsLiteral(stepTsMs(step)),
			count, formatFloat(sum), renderUintArray(deltas), renderFloatArray(histoBounds))
	}
	b.WriteString(";\n")
	return b.String()
}

// histogramModelSeries returns the cumulative companion series the oracle
// must see for histogram series si: one `<name>_bucket{le=...}` series per
// finite bound plus the +Inf bucket, a `<name>_count`, and a `<name>_sum`.
// These mirror exactly what cerberus's bucket fan-out + companion-column
// lowering produces, so a histogram_quantile over either side agrees.
func histogramModelSeries(name string, lbls map[string]string, si int) []property.SeriesData {
	var out []property.SeriesData

	// One _bucket series per le (finite bounds + +Inf). The value at each
	// step is the CUMULATIVE delta count up to and including that bucket.
	leLabels := make([]string, 0, len(histoBounds)+1)
	for _, b := range histoBounds {
		leLabels = append(leLabels, formatFloat(b))
	}
	leLabels = append(leLabels, "+Inf")

	for bi, le := range leLabels {
		bl := copyLabels(lbls)
		bl["le"] = le
		points := make([]property.Point, 0, seedSteps)
		for step := 0; step < seedSteps; step++ {
			deltas := bucketDeltas(si, step)
			var cum uint64
			for k := 0; k <= bi; k++ {
				cum += deltas[k]
			}
			points = append(points, property.Point{TimestampMs: stepTsMs(step), Value: float64(cum)})
		}
		out = append(out, property.SeriesData{
			MetricName: name + "_bucket",
			Labels:     bl,
			Points:     points,
		})
	}

	// _count and _sum companions.
	countPts := make([]property.Point, 0, seedSteps)
	sumPts := make([]property.Point, 0, seedSteps)
	for step := 0; step < seedSteps; step++ {
		deltas := bucketDeltas(si, step)
		var count uint64
		for _, d := range deltas {
			count += d
		}
		countPts = append(countPts, property.Point{TimestampMs: stepTsMs(step), Value: float64(count)})
		sumPts = append(sumPts, property.Point{TimestampMs: stepTsMs(step), Value: histogramSum(deltas)})
	}
	out = append(out,
		property.SeriesData{MetricName: name + "_count", Labels: copyLabels(lbls), Points: countPts},
		property.SeriesData{MetricName: name + "_sum", Labels: copyLabels(lbls), Points: sumPts},
	)
	return out
}

// histogramSum approximates Sum as the count-weighted midpoint of each
// bucket (finite buckets use the midpoint of their (lower, upper] edge; the
// +Inf bucket uses the last finite bound). The exact value is irrelevant to
// the comparison — what matters is that the chDB Sum column and the model
// `_sum` series compute it IDENTICALLY, which they do via this one helper.
func histogramSum(deltas []uint64) float64 {
	var sum float64
	lower := 0.0
	for i, bound := range histoBounds {
		mid := (lower + bound) / 2
		sum += mid * float64(deltas[i])
		lower = bound
	}
	// +Inf overflow bucket: charge it at the last finite bound.
	sum += lower * float64(deltas[len(deltas)-1])
	return sum
}

// ---------------------------------------------------------------------
// Small shared helpers.
// ---------------------------------------------------------------------

func mkSeries(name string, lbls map[string]string, points []property.Point) property.SeriesData {
	return property.SeriesData{MetricName: name, Labels: copyLabels(lbls), Points: points}
}

func stepTsMs(i int) int64 {
	return nowAnchor.Add(time.Duration(i) * stepInterval).UnixMilli()
}

func tsLiteral(ms int64) string {
	t := time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000")
	return "toDateTime64('" + t + "', 9)"
}

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
		fmt.Fprintf(&b, "'%s', '%s'", k, labels[k])
	}
	b.WriteByte(')')
	return b.String()
}

func renderUintArray(vals []uint64) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.FormatUint(v, 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func renderFloatArray(vals []float64) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = formatFloat(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
