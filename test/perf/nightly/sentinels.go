package nightly

import (
	"net/http"
	"net/url"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
)

// Metric names as they appear in testdata/samples/*.parquet's MetricName
// column (real, scrubbed production names — see testdata/samples/README.md).
const (
	histogramMetric = "svc_http_request_duration_seconds"
	sumMetric       = "svc_http_requests_total"
	gaugeMetric     = "kube_pod_status_reason"
)

// scrubbedRareStatusClass is one real, scrubbed status_class token from the
// sample — see error_ratio_by_namespace's own comment below for why a
// literal "5xx"-style filter cannot match this data.
const scrubbedRareStatusClass = "scrubbed-138722867745"

// sampleWindowStart / sampleWindowEnd bound every sentinel's query_range
// window, inside the trimmed sample's single calendar day (2026-08-18, the
// closest-to-median-cardinality day across all three metrics in the full
// 14-day set — see testdata/samples/README.md for the selection). The
// window sits inside that day's real captured span (09:00:01 - 13:29:59
// UTC).
var (
	sampleWindowStart = time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC)
	sampleWindowEnd   = time.Date(2026, 8, 18, 13, 25, 0, 0, time.UTC)
)

const sentinelStep = time.Minute

// Per-sentinel committed-ceiling headroom multipliers, replacing a single
// package-wide constant (formerly nightlyBaselineHeadroom, 1.5x for every
// sentinel). A real noise investigation (issue #2370's headroom-tightening
// follow-up) measured each sentinel's run-to-run peak-memory variance across
// repeated, INDEPENDENT `go test -tags=integration -run TestPerfNightlyRealCH`
// invocations (not just the in-test max-of-N sampling in
// realch_perfnightly_integration_test.go, which only captures intra-process
// variance) at the unmodified calibration point, and found the four
// sentinels' noise floors differ by nearly an order of magnitude — far too
// much for one multiplier to serve every sentinel honestly: tight enough to
// catch a real ~25% regression on the quiet sentinels would flake constantly
// on the noisy ones, and loose enough to never flake the noisy ones leaves
// the quiet ones no tighter than before.
//
//   - histogramBaselineHeadroom (classic_histogram_quantile_by_route, n=5
//     independent runs): 456.3M-471.0M bytes, max/min = 1.032x (CV 1.26%) —
//     by far the quietest of the four. This is also the exact sentinel
//     issue #2408's own investigation regression-tested (a real +24.6%
//     window-widening regression that a 1.5x headroom passed cleanly), so
//     it gets the tightest headroom: 1.20x leaves ~16% cushion over the
//     observed swing while a worst-case calibration/regression draw still
//     trips at ~+24% — squarely the target range.
//   - gaugeBaselineHeadroom (pod_status_reason_gauge, n=5): 725.5M-817.7M
//     bytes, max/min = 1.127x (CV 4.48%). Real memory usage here already
//     sits at 78-88% of the 1 GiB cap — nightlyMemoryCapFraction (0.85, see
//     its own comment in realch_perfnightly_integration_test.go) caps the
//     committed ceiling at 912,680,550 bytes regardless of this multiplier
//     for ANY headroom above ~1.12x, which the observed noise itself
//     (1.127x) does not leave enough cushion below to use safely. This
//     sentinel's real regression protection is PRONG (a)'s absolute cap,
//     not this constant, so it is left unchanged at 1.5x rather than
//     pretend a tightened value here buys any real sensitivity — see
//     issue #2435 for the cap-fraction margin itself.
//   - requestRateBaselineHeadroom (request_rate_by_method, n=8): 235.1M-
//     313.4M bytes, max/min = 1.333x (CV 10.45%) — an ExpectedStatus=422
//     sentinel: "peak memory" is memory used BEFORE the rate-window fanout
//     bound rejects the query, not a completed query's memory, and that
//     early-abort point is inherently noisier (scan-ordering/scheduling
//     dependent) than the two 200-status sentinels above. Left unchanged at
//     1.5x: the observed worst-case swing alone consumes most of that
//     headroom (1.5x / 1.333x = 1.125x real cushion), and tightening
//     further risks flaking on exactly the noise this measurement found.
//   - errorRatioBaselineHeadroom (error_ratio_by_namespace, n=8): 269.9M-
//     340.5M bytes, max/min = 1.262x (CV 8.93%) — same rejection-noise
//     character as request_rate_by_method, modestly less noisy. Tightened
//     to 1.45x, keeping ~15% real cushion over the observed swing
//     (1.45x / 1.262x = 1.149x).
const (
	histogramBaselineHeadroom   = 1.20
	gaugeBaselineHeadroom       = 1.50
	requestRateBaselineHeadroom = 1.50
	errorRatioBaselineHeadroom  = 1.45
	// nativeHistogramBaselineHeadroom has no per-sentinel noise measurement
	// yet (classic_native_histogram_derived was added after the other four
	// sentinels' independent-process noise sweep) — the unmeasured default
	// mirrors gaugeBaselineHeadroom/requestRateBaselineHeadroom's own
	// conservative choice: keep the pre-tightening 1.5x until a real
	// multi-run sweep for this specific sentinel justifies going tighter.
	nativeHistogramBaselineHeadroom = 1.50
	// labelValuesBaselineHeadroom (label_values_matcher_stock_key and its
	// renamed twin, n=7 independent runs): 25.85M-25.89M bytes, max/min =
	// 1.0014x (CV 0.06%), and 25.92M-25.98M, 1.0022x (CV 0.07%). The
	// regression they exist for, the emitter's PREWHERE split on a server
	// where move_to_prewhere resolved in, measured 35.8M-40.1M and
	// 36.8M-45.3M on the same data and server — at least 1.38x. 1.20x sits
	// two orders of magnitude above the noise and well under that step.
	labelValuesBaselineHeadroom = 1.20
	// rateByRouteBaselineHeadroom (rate_by_route_matcher_stock_key): the
	// same rate query measured over 6 repeats per server when the PREWHERE
	// split was chosen (docs/clickhouse-optimizations.background.md): 54.0M-
	// 58.1M on 24.8 and 41.0M-45.2M on 26.6 under the emitter's split (1.08x
	// and 1.10x max/min), 38.7M-47.9M on 25.9 under the server's (1.24x).
	// The regressions it exists for move it by several times — the server's
	// split on 26.6 measured 231M-321M, an explicit PREWHERE holding the
	// matcher and the time bounds 679M-748M on 24.8 — so it keeps the
	// conservative 1.5x (1.5x / 1.24x = 1.21x cushion on the noisiest line).
	rateByRouteBaselineHeadroom = 1.50
	// instantSelectorBaselineHeadroom (instant_selector_in_order, n=7):
	// 49.5M-58.1M bytes, max/min = 1.175x (CV 5.44%). Its memory prong is
	// a backstop — the pipeline prong is what separates the plan it guards
	// — so it keeps the conservative 1.5x (1.5x / 1.175x = 1.28x cushion).
	instantSelectorBaselineHeadroom = 1.50
	// nativeQuantilesBaselineHeadroom (native_histogram_quantiles_rate, n=7
	// replays of its dispatched statement on a 2-CPU server): 143.3M-193.8M
	// bytes, max/min = 1.352x. Its memory prong is a backstop — the planning
	// prong is what separates the plan it guards — so it keeps the
	// conservative 1.5x (1.5x / 1.352x = 1.11x cushion).
	nativeQuantilesBaselineHeadroom = 1.50
)

// Sentinel describes one real-data query the nightly harness issues and the
// construct family it exercises. Unlike test/perf/smoke's fixed 3-sentinel
// set (each calibrated to trip one specific memory-bounding mechanism on
// synthetic data), these run the REAL captured 2026-08-18 sample and cover
// construct families the smoke tier doesn't reach at all: a CLASSIC
// histogram_quantile (the sample data is classic-bucket, not exponential —
// exercising the bucket-rate arrayJoin fan-out issue #2408 investigated,
// which the smoke tier's native-histogram sentinel never touches), a plain
// counter rate, a Gauge aggregation (no smoke-tier coverage of this signal
// type at all), and a cross-series error-ratio shape.
type Sentinel struct {
	Name   string
	Family string
	Path   string
	Params func(start, end time.Time) url.Values
	// ExpectedStatus is the HTTP status this sentinel's query is expected
	// to return at real production cardinality. http.StatusOK for every
	// sentinel except the two #2429 found: at this sample's real series
	// count and the shared anchor grid below, a plain
	// `sum by()(rate(...))`-shaped query genuinely exceeds
	// internal/chsql's rate-window fanout resource bound by design — the
	// fix's own point was turning that from a silent OOM into a clean,
	// deliberate rejection, not making the underlying cost disappear. The
	// gate (PR B) asserts on THIS field so a status-class change either
	// direction — the bound silently breaking (OOM risk returns) or
	// silently over-broadening (a legitimate query starts being rejected)
	// — is itself the regression signal, not just a missed memory ceiling.
	ExpectedStatus int
	// ExpectedErrorSubstring, populated only when ExpectedStatus != 200,
	// is the response body substring proving THIS SPECIFIC guard fired —
	// not some unrelated failure that happens to share the status code.
	ExpectedErrorSubstring string
	// WindowStart / WindowEnd / Step, when WindowStart is non-zero,
	// override the shared sampleWindowStart / sampleWindowEnd / sentinelStep
	// for THIS sentinel only — see classic_histogram_quantile_by_route's own
	// comment for why one sentinel needs a narrower window than the other
	// three's shared production-representative one.
	WindowStart time.Time
	WindowEnd   time.Time
	Step        time.Duration
	// BaselineHeadroom is this sentinel's own committed-ceiling multiplier
	// against the max-of-N calibration measurement — the NOMINAL multiple,
	// before committedCeilingBytes clamps it to nightlyCapCeilingBytes; a
	// sentinel close to the absolute ceiling ends up with less. See the
	// *BaselineHeadroom constants above for the real, independent-process
	// noise data each value is calibrated against, and why a single
	// package-wide multiplier does not fit all four sentinels.
	BaselineHeadroom float64
	// Deployment is the tables, and the strategies resolved from them, the
	// request reads. The zero value is the metrics tables cerberus
	// provisions under their default names.
	Deployment Deployment
	// QuerySettings are ClickHouse settings stamped on every request of this
	// sentinel (chclient.WithQuerySetting), on top of the ones the
	// production wiring stamps.
	QuerySettings map[string]any
	// WantPipelineStep, when set, names a processor that EXPLAIN PIPELINE of
	// the statement cerberus dispatched — its text and settings, read back
	// from system.query_log — must contain. It is the check for a mechanism
	// that changes the plan without moving peak memory beyond run-to-run
	// noise.
	WantPipelineStep string
	// PlanningPeer, when set, is the request parameters of the sentinel's
	// planning peer: a query over the same data whose statement plans in a
	// comparable time. The planning prong requires the time ClickHouse takes
	// to plan the sentinel's dispatched statement (EXPLAIN PLAN of its text
	// and settings, read back from system.query_log) to stay within a fixed
	// multiple of the time it takes to plan the peer's. It is the check for a
	// mechanism whose cost is planning time, which peak memory does not
	// measure; the ratio cancels the speed of the host running it.
	PlanningPeer func(start, end time.Time) url.Values
	// NotOn names the ClickHouse release lines ("major.minor") of
	// perf-nightly.yml's matrix this sentinel is not part of the set of, each
	// with the reason: a construct whose feature floors above that server, or
	// a server defect tracked by the issue the reason cites. The sentinel set
	// of a line is SentinelsFor(line); nightly-baseline.json carries a bound
	// for exactly that set.
	NotOn map[string]string
}

// SentinelsFor is the sentinel set of ClickHouse release line version: every
// sentinel whose NotOn does not name it.
func SentinelsFor(version string) []Sentinel {
	set := make([]Sentinel, 0, len(Sentinels))
	for _, s := range Sentinels {
		if _, out := s.NotOn[version]; !out {
			set = append(set, s)
		}
	}
	return set
}

// Deployment names the metrics tables a sentinel's request reads. Each one
// is served by its own prom handler, wired as cmd/cerberus wires one: the
// SchemaStrategies the boot requirements check resolves from those tables,
// with the server facts the resolved optimizations decide
// (choptwire.SetServerStrategies).
type Deployment int

const (
	// DeploymentProvisioned is the four metrics tables cerberus provisions,
	// in the default database under their default names.
	DeploymentProvisioned Deployment = iota
	// DeploymentStockKey is the same tables as the OpenTelemetry exporter
	// v0.157 creates them — its ServiceName-first sorting key and DateTime
	// timestamps — in database stockKeyDB under their default names.
	DeploymentStockKey
	// DeploymentStockKeyRenamed is DeploymentStockKey's tables under the
	// non-default names stockKeyRenamedTables gives them, in database
	// stockKeyRenamedDB.
	DeploymentStockKeyRenamed
)

// The databases the stock-keyed deployments live in.
const (
	stockKeyDB        = "stock_key"
	stockKeyRenamedDB = "stock_key_renamed"
)

// stockKeyRenamedTables maps each metrics table of DeploymentStockKey to its
// name in DeploymentStockKeyRenamed.
var stockKeyRenamedTables = map[string]string{
	"otel_metrics_gauge":                 "metrics_gauge_v2",
	"otel_metrics_sum":                   "metrics_sum_v2",
	"otel_metrics_histogram":             "metrics_histogram_v2",
	"otel_metrics_exponential_histogram": "metrics_exp_histogram_v2",
}

// settingUseQueryConditionCache switches ClickHouse's query condition cache
// off on a sentinel whose peak memory would otherwise depend on what earlier
// repeats left in it.
const settingUseQueryConditionCache = "use_query_condition_cache"

// The sample slice the label-matcher sentinels read: one real, scrubbed
// method value of sumMetric over the last five minutes before 12:30, a span
// the captured day covers without a gap.
const scrubbedMethod = "scrubbed-0cf008d48ef1"

var (
	matcherWindowStart = time.Date(2026, 8, 18, 12, 25, 0, 0, time.UTC)
	matcherWindowEnd   = time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)
)

// pipelineAggregatingInOrder is the processor ClickHouse runs a GROUP BY
// with when optimize_aggregation_in_order reads the table in its sort order.
const pipelineAggregatingInOrder = "AggregatingInOrderTransform"

// Sentinels is PR A's "prove the mechanism, print the numbers" corpus — no
// baseline/gate yet (see realch_perfnightly_integration_test.go's doc
// comment). PR B calibrates a committed baseline from real numbers these
// produce, the same task-4 methodology test/perf/smoke's own PR followed.
var Sentinels = []Sentinel{
	{
		// Params MUST include `le` in the by(...) clause and query the
		// `_bucket` series — histogramAggShapeLowerable (internal/promql/
		// histogram_quantile.go) only recognizes the classic-bucket
		// aggregation shape `<agg> by (le, ...) (<fn>(<bucket_selector>
		// [range]))`; a by-clause omitting `le`, or a selector missing the
		// `_bucket` suffix, resolves to an entirely different (and here,
		// unintended) ordinary-float-bucket/Gauge-Sum companion-column
		// fallback instead — silently never reaching RangeBucketFanout /
		// RangeBucketGridNative at all, which defeated this sentinel's whole
		// purpose until this fix (see the PR that added this comment).
		//
		// WindowStart/WindowEnd/Step deliberately narrow this ONE sentinel's
		// window instead of sharing the other three's ~4h20m/1m production
		// window: at that full window this query genuinely reaches
		// chplan.RangeBucketGridNative (confirmed via this harness's own
		// "ts_grid_histogram enabled=true" boot log against a real
		// ClickHouse 25.9+) but runs all the way to a genuine ClickHouse
		// MEMORY_LIMIT_EXCEEDED abort at ~99.9% of the memory cap — unlike
		// RangeBucketFanout, the native path has no resource-bound guard of
		// its own yet (internal/chsql/lwr_fanout_bound.go's guard is wired
		// only into RangeBucketFanout's collapse; tracked as issue #2486).
		// A near-cap abort is real signal but a DIFFERENT one than this
		// sentinel exists to produce: #2408's own cost story is 71% of
		// ClickHouse CPU on a query that SUCCEEDS, and #2429/error_ratio's
		// own precedent for an "expected 422" sentinel is a CHEAP guard
		// rejection (~2s at 16-22% of cap), not a near-cap abort — baking a
		// near-cap OOM in as "the expected, passing behavior" here would
		// mask #2486 rather than measure #2408.
		//
		// The window below (5 anchors) is the calibration sweep's safest
		// pick, not merely the largest that happens to succeed: memory grows
		// steeply non-linearly with anchor count for this shape (real,
		// same-series-cardinality measurements against this sample: 5
		// anchors -> 43.1% of cap, 10 -> 53.9%, 20 -> 72.4%, 60 -> genuine
		// MEMORY_LIMIT_EXCEEDED) — itself further evidence for #2486, since
		// even a modest ~20-minute panel already sits uncomfortably close to
		// the cap. 5 anchors leaves ~20 points of margin under
		// nightlyMemoryCapFraction even after the committed baseline's own
		// 1.5x headroom, which the 10- and 20-anchor points do not.
		Name:             "classic_histogram_quantile_by_route",
		Family:           "histogram_quantile over a classic (bucket/bounds) histogram — issue #2408's arrayJoin bucket-rate fan-out",
		Path:             "/api/v1/query_range",
		ExpectedStatus:   http.StatusOK,
		WindowStart:      time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC),
		WindowEnd:        time.Date(2026, 8, 18, 9, 10, 0, 0, time.UTC),
		Step:             time.Minute,
		BaselineHeadroom: histogramBaselineHeadroom,
		NotOn: map[string]string{
			"24.8": "the native classic-histogram grid this sentinel measures (chopt ts_grid_histogram) floors at 25.9; " +
				"on 24.8 the fan-out lowering's collapsed-payload resource bound rejects this window by design (" +
				"chsql.RangeBucketFanoutGroupBudgetMessage)",
		},
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantile(0.95, sum by (http_route, le) (rate(` + histogramMetric + `_bucket[5m])))`},
			}
		},
	},
	{
		// Real numbers from this exact sentinel against the committed sample
		// data: 99.8% of the 1 GiB cap at 3,741 series / 260 steps under the
		// original (unfixed) engine — despite applySpillSettings' always-on
		// spill, which should have converted this into a slower disk spill
		// rather than an OOM abort. Filed and fixed as issue #2429: at this
		// scale the query is genuinely too expensive to serve safely, so
		// internal/chsql's rate-window fanout resource bound now rejects it
		// cleanly (422) rather than letting ClickHouse OOM. That rejection
		// IS this sentinel's expected, correct outcome at real production
		// cardinality — a regression here is either the bound silently
		// breaking (status flips back toward an OOM) or the query starting
		// to succeed at a genuinely unsafe scale, not "still 422".
		Name:                   "request_rate_by_method",
		Family:                 "plain counter rate() + sum by() — baseline construct, no histogram/gauge machinery",
		Path:                   "/api/v1/query_range",
		ExpectedStatus:         http.StatusUnprocessableEntity,
		ExpectedErrorSubstring: chsql.RateWindowFanoutBudgetMessage,
		BaselineHeadroom:       requestRateBaselineHeadroom,
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`sum by (method) (rate(` + sumMetric + `[5m]))`},
			}
		},
	},
	{
		Name:             "pod_status_reason_gauge",
		Family:           "Gauge aggregation — signal type the smoke tier has zero coverage of",
		Path:             "/api/v1/query_range",
		ExpectedStatus:   http.StatusOK,
		BaselineHeadroom: gaugeBaselineHeadroom,
		NotOn: map[string]string{
			"24.8": "ClickHouse 24.8-24.12 fails reading back a spilled GROUP BY over the mapSort-built series " +
				"identity (TYPE_MISMATCH, issue #3971), which this aggregation spills into",
		},
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`sum by (reason) (` + gaugeMetric + `)`},
			}
		},
	},
	{
		// scrubbedRareStatusClass is one real status_class value from the
		// sample (the rarest of its 4 distinct values, 360 of 1,338,836
		// rows) — the scrub pipeline (#2411) pseudonymizes every Attribute
		// VALUE, not just identifier-bearing keys, so "5xx"-style literal
		// filtering is not meaningful against this data; any real distinct
		// value exercises the identical filtered-numerator-over-denominator
		// construct this sentinel targets. Also hits (and, post-#2429-fix,
		// is correctly rejected by) the same rate-window fanout bound as
		// request_rate_by_method above — see its comment.
		Name:                   "error_ratio_by_namespace",
		Family:                 "cross-series ratio (error-rate shape) — two independent rate() aggregations divided, a common real-world PromQL construct absent from the smoke tier entirely",
		Path:                   "/api/v1/query_range",
		ExpectedStatus:         http.StatusUnprocessableEntity,
		ExpectedErrorSubstring: chsql.RateWindowFanoutBudgetMessage,
		BaselineHeadroom:       errorRatioBaselineHeadroom,
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {
					`sum by (k8s_namespace_name) (rate(` + sumMetric + `{status_class="` + scrubbedRareStatusClass + `"}[5m]))` +
						` / sum by (k8s_namespace_name) (rate(` + sumMetric + `[5m]))`,
				},
			}
		},
	},
	{
		// nativeHistogramMetric (loader.go) is DERIVED, not captured: the
		// real production sample this whole package's data was scrubbed
		// from only ever emits classic-bucket histograms (see loader.go's
		// loadNativeHistogramFromClassicSample doc comment), so there is no
		// real exponential-histogram sample to load. This sentinel carries
		// the same real 3,741-series cardinality and real per-series sample
		// cadence as classic_histogram_quantile_by_route, just re-bucketed
		// into an OTel exponential layout — the point is exercising
		// internal/chsql/histogram_quantile_native.go's merge/cumsum
		// machinery at real production scale, which no other lane (smoke's
		// own native-histogram sentinel is CI-fixture scale, see
		// test/perf/smoke/seed.go) reaches today.
		//
		// Params deliberately does NOT wrap nativeHistogramMetric in a
		// `sum by(...)(...)` aggregation the way the other by-route/by-method
		// sentinels do. Measured directly against this exact derived data: a
		// plain `sum by (http_route) (nativeHistogramMetric)` — even with NO
		// rate()/window wrapper at all, at a SINGLE query anchor — genuinely
		// exceeds the 1 GiB ClickHouse memory cap
		// (MEMORY_LIMIT_EXCEEDED, not a clean chsql-level rejection): the
		// native-histogram merge path has no resource-bound guard of its own
		// yet (unlike RangeBucketFanout's #2429 fix), tracked as issue #2490.
		// The bare per-series shape here — one `histogram_quantile` per
		// original series, no cross-series merge — is a real,
		// currently-lowerable PromQL construct (see
		// test/spec/promql/histogram_quantile_native_range.txtar) that still
		// exercises the SAME native quantile machinery (cum/revcum bucket
		// walk over each series' own, now-fixed-layout — see loader.go —
		// PositiveBucketCounts) at real per-series scale, without tripping
		// the separately-tracked aggregation gap.
		//
		// WindowStart/WindowEnd/Step mirror
		// classic_histogram_quantile_by_route's own narrowed window rather
		// than the other three sentinels' shared ~4h20m/1m production
		// window, calibrated the same way — a real sweep against this exact
		// derived data at increasing anchor counts (max-of-5 measurements,
		// same nightlySentinelRepeats this harness always uses):
		// 5 anchors -> 23.8% of cap, 10 -> 45.9%, 15 -> 75.4% — steeply
		// non-linear, the same shape classic_histogram_quantile_by_route's
		// own sweep found. 15 anchors already breaches safe headroom margin
		// (75.4% * nightlyBaselineHeadroom's 1.5x = 113%, over the cap
		// outright — PRONG (b) would have to clamp to the absolute ceiling
		// and so never actually gate, the same failure mode
		// pod_status_reason_gauge's own comment in
		// realch_perfnightly_integration_test.go warns against). 10 anchors
		// is the sweep's safest pick that still meaningfully stresses
		// ClickHouse: real margin (45.9% * 1.5 = 68.85%, ~16 points under
		// nightlyMemoryCapFraction) while processing double
		// classic_histogram_quantile_by_route's own 5-anchor window. The
		// committed ceiling in nightly-baseline.json comes from its own
		// separate calibration capture (~46-47% of cap) — normal max-of-5
		// run-to-run variance from this sweep's own 45.9%, not a different
		// window or query shape.
		Name:             "classic_native_histogram_derived",
		Family:           "histogram_quantile over a DERIVED exponential histogram — same real cardinality/cadence as the classic sentinel above, re-bucketed (see loader.go)",
		Path:             "/api/v1/query_range",
		ExpectedStatus:   http.StatusOK,
		BaselineHeadroom: nativeHistogramBaselineHeadroom,
		WindowStart:      time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC),
		WindowEnd:        time.Date(2026, 8, 18, 9, 14, 0, 0, time.UTC),
		Step:             time.Minute,
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantile(0.95, ` + nativeHistogramMetric + `)`},
			}
		},
	},
	{
		// The plural native quantile over a rate()-folded native histogram,
		// the shape a Grafana panel asking for several quantiles issues. Its
		// statement must plan in about the time the single-phi quantile over
		// the same input plans in (PlanningPeer): it reaches the same
		// merge/window-fold machinery, so the engine must dispatch it with the
		// same settings. Dispatched without the native-histogram analyzer fix,
		// it planned for about 110 s on this data against about 2.5 s for the
		// single-phi peer, while its peak memory stayed within the noise of the
		// fixed plan's — so peak memory alone cannot see the regression.
		//
		// An instant query: the range form over this window is rejected by the
		// histogram window fold's collapsed-payload resource bound before it
		// plans the quantile at all.
		Name:             "native_histogram_quantiles_rate",
		Family:           "histogram_quantiles over rate() of a DERIVED exponential histogram — planning cost of the plural native quantile",
		Path:             "/api/v1/query",
		ExpectedStatus:   http.StatusOK,
		BaselineHeadroom: nativeQuantilesBaselineHeadroom,
		WindowStart:      time.Date(2026, 8, 18, 9, 14, 0, 0, time.UTC),
		WindowEnd:        time.Date(2026, 8, 18, 9, 14, 0, 0, time.UTC),
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantiles(sum by (http_route) (rate(` + nativeHistogramMetric + `[5m])), "quantile", 0.5, 0.9, 0.95)`},
			}
		},
		PlanningPeer: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantile(0.95, sum by (http_route) (rate(` + nativeHistogramMetric + `[5m])))`},
			}
		},
	},
	{
		// The label-values read of one metric under a label matcher, against
		// the exporter's own sorting key. The emitter's PREWHERE split keeps
		// only the metric-name filter in PREWHERE and switches off the
		// server's move of the matcher and the time bounds, so every row of
		// the metric's granules is read with its attribute maps; where
		// move_to_prewhere is resolved in, the server's split filters first.
		Name:             "label_values_matcher_stock_key",
		Family:           "label values under a matcher on the exporter's sorting key — the PREWHERE/WHERE split of a filtered scan",
		Path:             "/api/v1/label/http_route/values",
		ExpectedStatus:   http.StatusOK,
		WindowStart:      matcherWindowStart,
		WindowEnd:        matcherWindowEnd,
		Step:             time.Minute,
		BaselineHeadroom: labelValuesBaselineHeadroom,
		Deployment:       DeploymentStockKey,
		QuerySettings:    map[string]any{settingUseQueryConditionCache: 0},
		Params: func(start, end time.Time) url.Values {
			return url.Values{"match[]": {sumMetric + `{method="` + scrubbedMethod + `"}`}}
		},
	},
	{
		// label_values_matcher_stock_key over the same tables under
		// non-default names, whose shape the emitter reads from the deployed
		// tables rather than from the default names it knows.
		Name:             "label_values_matcher_stock_key_renamed",
		Family:           "label values under a matcher on renamed exporter-keyed tables — the PREWHERE/WHERE split of a filtered scan",
		Path:             "/api/v1/label/http_route/values",
		ExpectedStatus:   http.StatusOK,
		WindowStart:      matcherWindowStart,
		WindowEnd:        matcherWindowEnd,
		Step:             time.Minute,
		BaselineHeadroom: labelValuesBaselineHeadroom,
		Deployment:       DeploymentStockKeyRenamed,
		QuerySettings:    map[string]any{settingUseQueryConditionCache: 0},
		Params: func(start, end time.Time) url.Values {
			return url.Values{"match[]": {sumMetric + `{method="` + scrubbedMethod + `"}`}}
		},
	},
	{
		// The rate of one metric under a label matcher, grouped by a label,
		// against the exporter's own sorting key. On a server where
		// move_to_prewhere is not resolved in, the emitter's PREWHERE split
		// keeps the metric-name filter in PREWHERE and the matcher and the
		// time bounds the range window pushes down in WHERE; the server's own
		// split there orders the matcher's map read first (from 26.2), and a
		// PREWHERE holding the matcher and the time bounds evaluates them on
		// every row of the metric's granules — each several times the peak
		// memory of the emitter's split.
		Name:             "rate_by_route_matcher_stock_key",
		Family:           "rate() by a label under a matcher on the exporter's sorting key — the explicit PREWHERE/WHERE split of a range-window scan",
		Path:             "/api/v1/query_range",
		ExpectedStatus:   http.StatusOK,
		WindowStart:      matcherWindowStart,
		WindowEnd:        matcherWindowEnd,
		Step:             time.Minute,
		BaselineHeadroom: rateByRouteBaselineHeadroom,
		Deployment:       DeploymentStockKey,
		QuerySettings:    map[string]any{settingUseQueryConditionCache: 0},
		Params: func(start, end time.Time) url.Values {
			return url.Values{"query": {`sum by (http_route) (rate(` + sumMetric + `{method="` + scrubbedMethod + `"}[5m]))`}}
		},
	},
	{
		// An instant selector groups by (MetricName, Attributes), a prefix of
		// the sorting key cerberus provisions, so the engine stamps
		// optimize_aggregation_in_order and ClickHouse aggregates in the
		// key's order. Losing the stamp changes the plan but not peak memory
		// beyond run-to-run noise, so the sentinel also requires the
		// in-order processor in the dispatched statement's pipeline.
		Name:             "instant_selector_in_order",
		Family:           "instant selector on cerberus's sorting key — GROUP BY a sort-key prefix aggregated in order",
		Path:             "/api/v1/query",
		ExpectedStatus:   http.StatusOK,
		WindowStart:      matcherWindowEnd,
		WindowEnd:        matcherWindowEnd,
		BaselineHeadroom: instantSelectorBaselineHeadroom,
		QuerySettings:    map[string]any{settingUseQueryConditionCache: 0},
		WantPipelineStep: pipelineAggregatingInOrder,
		Params: func(start, end time.Time) url.Values {
			return url.Values{"query": {sumMetric}}
		},
	},
}
