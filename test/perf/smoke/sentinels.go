package smoke

import (
	"net/url"
	"time"
)

// Metric names the seed builders write and the sentinel queries read. Kept
// here (not in seed.go) so this file is the single place that names what a
// sentinel targets.
const (
	// NativeHistogramMetric is Sentinel 1's exponential-histogram metric.
	NativeHistogramMetric = "cerberus_smoke_latency_exp_hist"
	// WideCounterMetric is Sentinel 2's high-cardinality counter metric.
	WideCounterMetric = "cerberus_smoke_wide_requests_total"
)

// sentinelWindow / sentinelStep size every sentinel's query_range grid: 1h at
// 15s step (241 anchors), matching the exact scale
// applyNativeHistogramAnalyzerFix's doc comment cites as the validation
// methodology (500 series under a one-label GROUP BY at this grid measured
// 12.2-14.5s with the analyzer vs 2.3-2.6s without it).
const (
	sentinelWindow = time.Hour
	sentinelStep   = 15 * time.Second
)

// NativeHistogramSeriesCount is Sentinel 1's series cardinality. Fixed
// (not calibrated) at the exact count applyNativeHistogramAnalyzerFix's own
// doc comment names as the methodology that measured the #2355/#2358
// regression: "500 distinct series under a one-label GROUP BY".
const NativeHistogramSeriesCount = 500

// Sentinel describes one real-ClickHouse memory-bounding differential
// target: an HTTP request against the mounted production handlers, built to
// reach a specific plan shape and memory-bounding mechanism the #2364
// incident (and #2358/#2355/#2366) turned on, at a scale realistic enough
// that the mechanism is what keeps the query under the memory cap rather
// than the query being incidentally too small to need it.
type Sentinel struct {
	// Name identifies the sentinel in the baseline JSON and test output.
	Name string
	// Mechanism names the applyXxx settings rule
	// (internal/engine/query_settings_rules.go / spill.go) this sentinel
	// targets, for logging/doc purposes only.
	Mechanism string
	// Path is the HTTP path to issue the request against, on the combined
	// prom+tempo mux.
	Path string
	// Params returns the query-string params (sans start/end/step, which the
	// caller fills from Window/Step) for the request over [start, end).
	Params func(start, end time.Time) url.Values
	// Window / Step size the query_range grid this sentinel issues.
	Window time.Duration
	Step   time.Duration
}

// Sentinels is the single source of truth for the perf-smoke real-ClickHouse
// differential: every sentinel query the integration harness issues, and the
// specific #2364-class memory-bounding mechanism each one targets.
var Sentinels = []Sentinel{
	{
		// PRIMARY sentinel — this is what actually broke in #2364. Hits
		// chplan.HistogramQuantileNative / HistogramProjection ->
		// applyNativeHistogramAnalyzerFix directly.
		Name:      "native_histogram_quantile",
		Mechanism: "applyNativeHistogramAnalyzerFix (internal/engine/query_settings_rules.go)",
		Path:      "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {"histogram_quantile(0.9, sum by (pod) (rate(" + NativeHistogramMetric + "[5m])))"},
			}
		},
		Window: sentinelWindow,
		Step:   sentinelStep,
	},
	{
		// Spill, unconditional: applySpillSettings fires on EVERY data-plane
		// query, but only a high-cardinality GROUP BY actually crosses
		// spillThreshold(cap) and exercises the spill-to-disk path rather
		// than fitting comfortably in RAM regardless.
		Name:      "spill_high_cardinality_groupby",
		Mechanism: "applySpillSettings (internal/engine/spill.go)",
		Path:      "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {"sum by (session_id) (rate(" + WideCounterMetric + "[5m]))"},
			}
		},
		Window: sentinelWindow,
		Step:   sentinelStep,
	},
	{
		// compare(), gated on chplan.MetricsCompare via
		// applyCompareMemoryBound. Shape mirrors
		// test/spec/traceql/metrics_compare.txtar. Tempo's
		// /api/metrics/query_range defaults `step` (traceql.DefaultQueryRangeStep,
		// ~240 points) when omitted, matching Grafana Traces Drilldown's own
		// issue-detector query — Step is left zero here and the caller omits
		// the param.
		Name:      "compare_memory_bound",
		Mechanism: "applyCompareMemoryBound (internal/engine/query_settings_rules.go)",
		Path:      "/api/metrics/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{"q": {`{ } | compare({ status = error })`}}
		},
		Window: sentinelWindow,
		Step:   0,
	},
}
