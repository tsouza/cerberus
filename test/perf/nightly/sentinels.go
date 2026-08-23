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
}

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
		Name:           "classic_histogram_quantile_by_route",
		Family:         "histogram_quantile over a classic (bucket/bounds) histogram — issue #2408's arrayJoin bucket-rate fan-out",
		Path:           "/api/v1/query_range",
		ExpectedStatus: http.StatusOK,
		WindowStart:    time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 8, 18, 9, 10, 0, 0, time.UTC),
		Step:           time.Minute,
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
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`sum by (method) (rate(` + sumMetric + `[5m]))`},
			}
		},
	},
	{
		Name:           "pod_status_reason_gauge",
		Family:         "Gauge aggregation — signal type the smoke tier has zero coverage of",
		Path:           "/api/v1/query_range",
		ExpectedStatus: http.StatusOK,
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
		Name:           "classic_native_histogram_derived",
		Family:         "histogram_quantile over a DERIVED exponential histogram — same real cardinality/cadence as the classic sentinel above, re-bucketed (see loader.go)",
		Path:           "/api/v1/query_range",
		ExpectedStatus: http.StatusOK,
		WindowStart:    time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 8, 18, 9, 14, 0, 0, time.UTC),
		Step:           time.Minute,
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantile(0.95, ` + nativeHistogramMetric + `)`},
			}
		},
	},
}
