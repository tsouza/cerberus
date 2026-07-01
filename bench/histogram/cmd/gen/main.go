// Command gen seeds an identical synthetic classic-histogram dataset into
// ClickHouse (cerberus's read path) and into one or more Prometheus
// remote-write endpoints (reference Prometheus + Grafana Mimir), so the three
// backends answer `histogram_quantile(...)` over byte-identical series.
//
// Storage shapes (the whole point of the harness — they must agree):
//
//   - ClickHouse `otel_metrics_histogram`: ONE row per (series, timestamp) with
//     parallel BucketCounts × ExplicitBounds arrays under the BARE metric name
//     (no `_bucket` suffix). BucketCounts[i] is the per-bucket count (NOT
//     cumulative across buckets); len(BucketCounts) == len(ExplicitBounds)+1,
//     the last element being the +Inf overflow bucket. Cerberus derives the
//     classic `le` series at query time via arraySum(arraySlice(BucketCounts,
//     1, le_idx)) — i.e. it cumulates across buckets itself.
//
//   - Prometheus / Mimir: the classic exploded form — one `<metric>_bucket`
//     series per `le` (CUMULATIVE across buckets), plus `<metric>_sum` and
//     `<metric>_count`.
//
// Both are generated from the same per-series bucket weights, so a
// `histogram_quantile` over either representation yields the same numbers.
//
// Values are cumulative over time (monotonic counters) so rate()/increase()
// behave, and the per-step increment is a fixed integer weight vector per
// series — deterministic, exactly reproducible, and distinct per `route` label
// so high-cardinality `by (le, route)` queries return meaningfully different
// quantiles per route.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

const histTable = "otel_metrics_histogram"

// aggregationTemporalityCumulative is the OTel enum value for cumulative
// (delta=1, cumulative=2) metrics; classic histograms are cumulative counters.
const aggregationTemporalityCumulative = int32(2)

// promDefaultBounds is the Prometheus client default histogram bucket layout.
// The generator uses the first -bounds of these as ExplicitBounds; the +Inf
// overflow bucket is implicit (BucketCounts has one extra trailing element).
var promDefaultBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type config struct {
	chAddr, chDB, chUser, chPass string
	promRW, mimirRW              string
	mimirOrgID                   string
	routes, instances, bounds    int
	steps, intervalSec           int
	peakWidth                    int
	blockSteps                   int
	manifestPath                 string
}

func main() {
	log.SetFlags(log.Ltime)
	cfg := parseFlags()

	ctx := context.Background()
	if err := run(ctx, cfg); err != nil {
		log.Fatalf("FATAL: %v", err)
	}
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.chAddr, "ch-addr", envOr("BENCH_CH_ADDR", "localhost:49000"), "ClickHouse native addr")
	flag.StringVar(&c.chDB, "ch-db", envOr("BENCH_CH_DATABASE", "otel"), "ClickHouse database")
	flag.StringVar(&c.chUser, "ch-user", envOr("BENCH_CH_USERNAME", "cerberus"), "ClickHouse user")
	flag.StringVar(&c.chPass, "ch-pass", envOr("BENCH_CH_PASSWORD", ""), "ClickHouse password (empty for the throwaway local bench stack)")
	flag.StringVar(&c.promRW, "prom-remote-write", envOr("BENCH_PROM_RW", "http://localhost:49090/api/v1/write"), "Prometheus remote-write URL (empty to skip)")
	flag.StringVar(&c.mimirRW, "mimir-remote-write", envOr("BENCH_MIMIR_RW", "http://localhost:49009/api/v1/push"), "Mimir remote-write URL (empty to skip)")
	flag.StringVar(&c.mimirOrgID, "mimir-org-id", envOr("BENCH_MIMIR_ORGID", ""), "Mimir X-Scope-OrgID header (empty when multitenancy disabled)")
	flag.IntVar(&c.routes, "routes", envIntOr("BENCH_ROUTES", 5), "route label cardinality")
	flag.IntVar(&c.instances, "instances", envIntOr("BENCH_INSTANCES", 2), "instance label cardinality")
	flag.IntVar(&c.bounds, "bounds", envIntOr("BENCH_BOUNDS", 11), "number of explicit le bounds (buckets = bounds+1)")
	flag.IntVar(&c.steps, "steps", envIntOr("BENCH_STEPS", 240), "number of timestamps (samples per series)")
	flag.IntVar(&c.intervalSec, "interval", envIntOr("BENCH_INTERVAL_SEC", 15), "seconds between samples (scrape interval)")
	flag.IntVar(&c.peakWidth, "peak-width", envIntOr("BENCH_PEAK_WIDTH", 5), "triangular width of per-step bucket weights")
	flag.IntVar(&c.blockSteps, "block-steps", 120, "steps per remote-write/insert flush block")
	flag.StringVar(&c.manifestPath, "manifest", envOr("BENCH_MANIFEST", "data-window.json"), "path to write the data-window manifest")
	flag.Parse()
	if c.bounds < 1 || c.bounds > len(promDefaultBounds) {
		log.Fatalf("-bounds must be 1..%d", len(promDefaultBounds))
	}
	return c
}

// series is one labelled histogram stream with its fixed per-step bucket
// weights and per-step sum contribution.
type series struct {
	labels     map[string]string // job/instance/route (no le)
	weights    []uint64          // length bounds+1; weights[i] added to BucketCounts[i] each step
	perStepSum float64           // Σ weights[i]*rep[i] added to Sum each step
}

func run(ctx context.Context, cfg config) error {
	bounds := promDefaultBounds[:cfg.bounds]
	nb := len(bounds) + 1 // total buckets incl +Inf
	reps := bucketReps(bounds)

	// End the data window at "now" (aligned to the interval) so the samples are
	// fresh — reference Prometheus rejects remote-writes far in the past, and
	// cerberus's instant-query staleness lookback wants a recent last sample.
	interval := time.Duration(cfg.intervalSec) * time.Second
	end := time.Now().UTC().Truncate(interval)
	start := end.Add(-time.Duration(cfg.steps-1) * interval)

	all := buildSeries(cfg, nb, reps)
	log.Printf("generating: %d series (%d routes × %d instances), %d buckets, %d steps @ %ds → window [%s, %s]",
		len(all), cfg.routes, cfg.instances, nb, cfg.steps, cfg.intervalSec,
		start.Format(time.RFC3339), end.Format(time.RFC3339))

	conn, err := dialCH(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := ensureTable(ctx, conn, cfg.chDB); err != nil {
		return err
	}

	metric := "http_request_duration_seconds"
	rwTargets := remoteWriteTargets(cfg)

	total := 0
	for s0 := 0; s0 < cfg.steps; s0 += cfg.blockSteps {
		s1 := s0 + cfg.blockSteps
		if s1 > cfg.steps {
			s1 = cfg.steps
		}
		promByKey := map[string]*promSeries{}
		orderedKeys := []string{}

		batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+histTable+
			" (MetricName, Attributes, TimeUnix, StartTimeUnix, Count, Sum, BucketCounts, ExplicitBounds, AggregationTemporality, Flags)")
		if err != nil {
			return fmt.Errorf("prepare batch: %w", err)
		}

		for s := s0; s < s1; s++ {
			ts := start.Add(time.Duration(s) * interval)
			tsMs := ts.UnixMilli()
			mult := uint64(s + 1) // cumulative-over-time multiplier

			for _, sr := range all {
				bc := make([]uint64, nb)
				var count uint64
				for i := 0; i < nb; i++ {
					bc[i] = sr.weights[i] * mult
					count += bc[i]
				}
				sumVal := sr.perStepSum * float64(mult)

				if err := batch.Append(metric, sr.labels, ts, start, count, sumVal, bc, bounds, aggregationTemporalityCumulative, uint32(0)); err != nil {
					return fmt.Errorf("append row: %w", err)
				}

				// Prometheus/Mimir exploded form: cumulative-across-buckets.
				var across uint64
				for i := 0; i < len(bounds); i++ {
					across += sr.weights[i] * mult
					addSample(promByKey, &orderedKeys, bucketLabels(metric, sr.labels, leString(bounds[i])), tsMs, float64(across))
				}
				addSample(promByKey, &orderedKeys, bucketLabels(metric, sr.labels, "+Inf"), tsMs, float64(count))
				addSample(promByKey, &orderedKeys, nameLabels(metric+"_count", sr.labels), tsMs, float64(count))
				addSample(promByKey, &orderedKeys, nameLabels(metric+"_sum", sr.labels), tsMs, sumVal)
				total++
			}
		}

		if err := batch.Send(); err != nil {
			return fmt.Errorf("ch batch send: %w", err)
		}

		if len(rwTargets) > 0 {
			seriesOut := make([]promSeries, 0, len(orderedKeys))
			for _, k := range orderedKeys {
				seriesOut = append(seriesOut, *promByKey[k])
			}
			body := encodeWriteRequest(seriesOut)
			for _, tgt := range rwTargets {
				if err := postRemoteWrite(ctx, tgt.url, body, tgt.headers); err != nil {
					return fmt.Errorf("remote_write %s: %w", tgt.name, err)
				}
			}
		}
		log.Printf("  block steps [%d,%d) flushed (%d otel rows total)", s0, s1, total)
	}

	if err := writeManifest(cfg, metric, bounds, start, end, len(all)); err != nil {
		return err
	}
	log.Printf("done: %d otel rows inserted; manifest → %s", total, cfg.manifestPath)
	return nil
}

// buildSeries enumerates the label cross-product and assigns each series a
// deterministic triangular bucket-weight vector peaked at a route/instance
// dependent bucket, so quantiles differ across routes.
func buildSeries(cfg config, nb int, reps []float64) []series {
	out := make([]series, 0, cfg.routes*cfg.instances)
	for inst := 0; inst < cfg.instances; inst++ {
		for r := 0; r < cfg.routes; r++ {
			peak := (r*3 + inst) % nb
			w := make([]uint64, nb)
			var perStepSum float64
			for i := 0; i < nb; i++ {
				d := i - peak
				if d < 0 {
					d = -d
				}
				v := cfg.peakWidth - d
				if v < 1 {
					v = 1 // keep every bucket non-empty so cumulative counters are strictly monotone
				}
				w[i] = uint64(v)
				perStepSum += float64(v) * reps[i]
			}
			out = append(out, series{
				labels:     map[string]string{"job": "api", "instance": fmt.Sprintf("inst-%d", inst), "route": fmt.Sprintf("route-%d", r)},
				weights:    w,
				perStepSum: perStepSum,
			})
		}
	}
	return out
}

// bucketReps returns a representative observation value per bucket (midpoints;
// the +Inf bucket uses 1.5× the top finite bound) used only to synthesize a
// plausible Sum. Sum is identical across backends, so it never affects parity.
func bucketReps(bounds []float64) []float64 {
	reps := make([]float64, len(bounds)+1)
	for i := range bounds {
		if i == 0 {
			reps[i] = bounds[0] / 2
		} else {
			reps[i] = (bounds[i-1] + bounds[i]) / 2
		}
	}
	reps[len(bounds)] = bounds[len(bounds)-1] * 1.5
	return reps
}

func leString(b float64) string {
	// Match Prometheus le-label formatting for the default buckets.
	return formatFloat(b)
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}

// addSample appends a sample to the series identified by its sorted label set,
// creating the series (and recording key order) on first use within a block.
func addSample(byKey map[string]*promSeries, order *[]string, labels [][2]string, tsMs int64, v float64) {
	key := labelKey(labels)
	ps, ok := byKey[key]
	if !ok {
		ps = &promSeries{labels: labels}
		byKey[key] = ps
		*order = append(*order, key)
	}
	ps.samples = append(ps.samples, promSample{valueField: v, timestampMsFld: tsMs})
}

func labelKey(labels [][2]string) string {
	s := ""
	for _, l := range labels {
		s += l[0] + "=" + l[1] + ";"
	}
	return s
}

// nameLabels builds a sorted prom label slice from a metric name + attrs.
func nameLabels(name string, attrs map[string]string) [][2]string {
	out := make([][2]string, 0, len(attrs)+1)
	out = append(out, [2]string{"__name__", name})
	for k, v := range attrs {
		out = append(out, [2]string{k, v})
	}
	sortLabels(out)
	return out
}

// bucketLabels is nameLabels for `<metric>_bucket` plus an le label.
func bucketLabels(metric string, attrs map[string]string, le string) [][2]string {
	out := make([][2]string, 0, len(attrs)+2)
	out = append(out, [2]string{"__name__", metric + "_bucket"})
	out = append(out, [2]string{"le", le})
	for k, v := range attrs {
		out = append(out, [2]string{k, v})
	}
	sortLabels(out)
	return out
}

func sortLabels(l [][2]string) {
	sort.Slice(l, func(i, j int) bool { return l[i][0] < l[j][0] })
}

type rwTarget struct {
	name    string
	url     string
	headers map[string]string
}

func remoteWriteTargets(cfg config) []rwTarget {
	var t []rwTarget
	if cfg.promRW != "" {
		t = append(t, rwTarget{name: "prometheus", url: cfg.promRW})
	}
	if cfg.mimirRW != "" {
		h := map[string]string{}
		if cfg.mimirOrgID != "" {
			h["X-Scope-OrgID"] = cfg.mimirOrgID
		}
		t = append(t, rwTarget{name: "mimir", url: cfg.mimirRW, headers: h})
	}
	return t
}

func dialCH(ctx context.Context, cfg config) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{cfg.chAddr},
		Auth:        clickhouse.Auth{Database: cfg.chDB, Username: cfg.chUser, Password: cfg.chPass},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("ch open: %w", err)
	}
	// Wait for CH to answer — the generator may race a just-started container.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err = conn.Ping(ctx); err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ch ping: %w", err)
		}
		time.Sleep(time.Second)
	}
}

// ensureTable waits for cerberus's own auto-created histogram table (so the
// benchmark measures cerberus's real production DDL), falling back to creating
// a byte-compatible table itself if cerberus hasn't provisioned it in time.
func ensureTable(ctx context.Context, conn driver.Conn, db string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n uint64
		if err := conn.QueryRow(ctx, "SELECT count() FROM system.tables WHERE database = ? AND name = ?", db, histTable).Scan(&n); err == nil && n == 1 {
			log.Printf("histogram table present (cerberus-provisioned)")
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("histogram table absent after wait; creating fallback DDL")
	if err := conn.Exec(ctx, fmt.Sprintf(fallbackHistDDL, db, histTable)); err != nil {
		return fmt.Errorf("fallback create: %w", err)
	}
	return nil
}

func writeManifest(cfg config, metric string, bounds []float64, start, end time.Time, nSeries int) error {
	m := map[string]any{
		"metric":                metric,
		"metric_bucket":         metric + "_bucket",
		"start_unix":            start.Unix(),
		"end_unix":              end.Unix(),
		"step_interval_seconds": cfg.intervalSec,
		"steps":                 cfg.steps,
		"routes":                cfg.routes,
		"instances":             cfg.instances,
		"bounds":                bounds,
		"otel_series":           nSeries,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.manifestPath, b, 0o644)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envIntOr(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return d
}

// fallbackHistDDL matches the columns cerberus reads (upstream OTel-CH exporter
// histogram schema). Only used if cerberus hasn't auto-created the table first.
const fallbackHistDDL = `CREATE TABLE IF NOT EXISTS "%s"."%s" (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl String CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32 CODEC(ZSTD(1)),
    ScopeSchemaUrl String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName String CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix DateTime64(9) CODEC(Delta, ZSTD(1)),
    TimeUnix DateTime64(9) CODEC(Delta, ZSTD(1)),
    Count UInt64 CODEC(Delta, ZSTD(1)),
    Sum Float64 CODEC(ZSTD(1)),
    BucketCounts Array(UInt64) CODEC(ZSTD(1)),
    ExplicitBounds Array(Float64) CODEC(ZSTD(1)),
    Exemplars Nested (
        FilteredAttributes Map(LowCardinality(String), String),
        TimeUnix DateTime64(9),
        Value Float64,
        SpanId String,
        TraceId String
    ) CODEC(ZSTD(1)),
    Flags UInt32 CODEC(ZSTD(1)),
    Min Float64 CODEC(ZSTD(1)),
    Max Float64 CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1)),
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_key mapKeys(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_value mapValues(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
SETTINGS index_granularity=8192, ttl_only_drop_parts=1`
