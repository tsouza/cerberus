package nightly

import (
	"net/url"
	"time"
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
}

// Sentinels is PR A's "prove the mechanism, print the numbers" corpus — no
// baseline/gate yet (see realch_perfnightly_integration_test.go's doc
// comment). PR B calibrates a committed baseline from real numbers these
// produce, the same task-4 methodology test/perf/smoke's own PR followed.
var Sentinels = []Sentinel{
	{
		Name:   "classic_histogram_quantile_by_route",
		Family: "histogram_quantile over a classic (bucket/bounds) histogram — issue #2408's arrayJoin bucket-rate fan-out",
		Path:   "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`histogram_quantile(0.95, sum by (http_route) (rate(` + histogramMetric + `[5m])))`},
			}
		},
	},
	{
		// Real numbers from this exact sentinel against the committed sample
		// data: 99.8% of the 1 GiB cap at 3,741 series / 260 steps — despite
		// applySpillSettings' always-on spill, which should have converted
		// this into a slower disk spill rather than an OOM abort. Filed as
		// issue #2429 (needs investigation, not a quick fix from this one
		// data point) rather than silently accepted.
		Name:   "request_rate_by_method",
		Family: "plain counter rate() + sum by() — baseline construct, no histogram/gauge machinery",
		Path:   "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {`sum by (method) (rate(` + sumMetric + `[5m]))`},
			}
		},
	},
	{
		Name:   "pod_status_reason_gauge",
		Family: "Gauge aggregation — signal type the smoke tier has zero coverage of",
		Path:   "/api/v1/query_range",
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
		// construct this sentinel targets. Also hits issue #2429: 100.0% of
		// the 1 GiB cap at this cardinality.
		Name:   "error_ratio_by_namespace",
		Family: "cross-series ratio (error-rate shape) — two independent rate() aggregations divided, a common real-world PromQL construct absent from the smoke tier entirely",
		Path:   "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {
					`sum by (k8s_namespace_name) (rate(` + sumMetric + `{status_class="` + scrubbedRareStatusClass + `"}[5m]))` +
						` / sum by (k8s_namespace_name) (rate(` + sumMetric + `[5m]))`,
				},
			}
		},
	},
}
