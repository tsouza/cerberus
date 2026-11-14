//go:build integration

package chserver

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicWindowRefusedPeakBytes bounds the peak memory of a probe the window
// guards refuse. The query-wide check refuses before the stage collects a
// sample (18-31 MiB on both builds), and the per-series check refuses a
// series of a few thousand samples once it is collected and before it is
// folded (26-31 MiB); an unguarded shape of these sizes peaks above 600 MiB
// or runs out of memory.
const classicWindowRefusedPeakBytes = 128 << 20

// classicWindowShape is one seed: series series of one shared 50-bucket
// layout, or of disjoint per-series layouts, each carrying samples samples
// spaced spacing apart and ending at classicMergeEvalTime. Bucket j of a
// series' k-th newest sample counts samples - k observations, so every
// series is a counter whose buckets grow equally.
type classicWindowShape struct {
	series, samples int
	spacing         time.Duration
	disjoint        bool
}

// classicWindowWidth is every seed's finite bound count.
const classicWindowWidth = 50

// seed inserts shape under its own metric name on s and returns that name.
func (sh classicWindowShape) seed(ctx context.Context, t *testing.T, s *server) string {
	t.Helper()
	layout, bound := "shared", "toFloat64(j)"
	if sh.disjoint {
		layout = "disjoint"
		bound = fmt.Sprintf("toFloat64(intDiv(number, %d) * %d + j)", sh.samples, classicWindowWidth)
	}
	metric := fmt.Sprintf("classic_window_%d_%d_%d_%s", sh.series, sh.samples, sh.spacing.Milliseconds(), layout)
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalMillisecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(%d - (number %% %d))),
  arrayMap(j -> %s, range(1, %d))
FROM numbers(%d)`,
		metric, sh.samples,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), sh.samples, sh.spacing.Milliseconds(),
		classicWindowWidth+1, sh.samples, sh.samples,
		bound, classicWindowWidth+1,
		sh.series*sh.samples))
	return metric
}

// The seeds. The 112-sample shapes are the reproduction of the window
// stage running ClickHouse out of memory; the admitted query-wide probe
// reads the newest 20 samples of the shared one through a [40s] range.
var (
	classicWindowShared   = classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second}
	classicWindowDisjoint = classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second, disjoint: true}
	// classicWindowWideSeries carries 6,000 x 51 = 306,000 entries in one
	// series, past the 305,040 per-series ceiling the 1 GiB cap derives.
	classicWindowWideSeries = classicWindowShape{series: 1, samples: 6_000, spacing: 45 * time.Millisecond}
	// classicWindowNarrowSeries carries 5,900 x 51 = 300,900 entries in
	// each of two series, within that ceiling.
	classicWindowNarrowSeries = classicWindowShape{series: 2, samples: 5_900, spacing: 50 * time.Millisecond}
)

// classicWindowAdmittedRange selects the newest 20 samples of
// classicWindowShared's 2-second spacing: 58,000 x 20 x 51 entries, an
// estimate of 802,921,600 against the 805,306,368 the 1 GiB cap derives.
// classicWindowRefusedRange selects 21, an estimate of 838,417,600.
const (
	classicWindowAdmittedRange = "40s"
	classicWindowRefusedRange  = "42s"
)

// TestClassicBucketWindow_CeilingsAcrossBuilds serves the instant
// classic-histogram quantile through the production handler under the 1 GiB
// query memory cap and the default resource bounds, on each build. The
// 58,000 x 50 x 112 shapes, shared and disjoint, are refused by the
// query-wide check, and one series past the per-series ceiling by the
// per-series check, each with its own message and at a peak far below the
// cap. A shape just within each ceiling answers with the quantile its seed
// implies, and the same shape one sample per series wider is refused.
func TestClassicBucketWindow_CeilingsAcrossBuilds(t *testing.T) {
	seriesCeiling := promql.ClassicBucketWindowSeriesEntriesForMemory(classicMergeQueryMemoryBytes)
	if got := classicWindowWideSeries.samples * (classicWindowWidth + 1); int64(got) <= seriesCeiling {
		t.Fatalf("wide series carries %d entries; want past the per-series ceiling %d", got, seriesCeiling)
	}
	if got := classicWindowNarrowSeries.samples * (classicWindowWidth + 1); int64(got) > seriesCeiling {
		t.Fatalf("narrow series carry %d entries; want within the per-series ceiling %d", got, seriesCeiling)
	}
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			mux := classicMergeMux(ctx, t, s, "auto")
			shared := classicWindowShared.seed(ctx, t, s)
			narrow := classicWindowNarrowSeries.seed(ctx, t, s)
			t.Run("admitted/query-wide", func(t *testing.T) {
				code, body := serveClassicWindow(ctx, mux, "rate", shared, classicWindowAdmittedRange)
				assertClassicWindowAnswer(t, s, code, body)
			})
			t.Run("admitted/per-series", func(t *testing.T) {
				code, body := serveClassicWindow(ctx, mux, "rate", narrow, "5m")
				assertClassicWindowAnswer(t, s, code, body)
			})
			disjoint := classicWindowDisjoint.seed(ctx, t, s)
			wide := classicWindowWideSeries.seed(ctx, t, s)
			for _, tc := range []struct {
				name, fn, metric, rng, message string
			}{
				{"refused/shared-rate-21-samples", "rate", shared, classicWindowRefusedRange, chplan.ClassicBucketWindowBudgetMessage},
				{"refused/shared-rate", "rate", shared, "5m", chplan.ClassicBucketWindowBudgetMessage},
				{"refused/shared-increase", "increase", shared, "5m", chplan.ClassicBucketWindowBudgetMessage},
				{"refused/shared-sum_over_time", "sum_over_time", shared, "5m", chplan.ClassicBucketWindowBudgetMessage},
				{"refused/disjoint-rate", "rate", disjoint, "5m", chplan.ClassicBucketWindowBudgetMessage},
				{"refused/wide-series", "rate", wide, "5m", chplan.ClassicBucketWindowSeriesBudgetMessage},
			} {
				t.Run(tc.name, func(t *testing.T) {
					qid := fmt.Sprintf("classic-window-%s-%s-%d", s.version, strings.ReplaceAll(tc.name, "/", "-"), time.Now().UnixNano())
					code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, tc.fn, tc.metric, tc.rng)
					if code != http.StatusUnprocessableEntity || !strings.Contains(body, tc.message) {
						t.Fatalf("%s %s: HTTP %d: %s; want 422 carrying %q", s.version, tc.name, code, body, tc.message)
					}
					if peak := s.exceptionPeakMemory(ctx, t, qid); peak >= classicWindowRefusedPeakBytes {
						t.Fatalf("%s %s: refused at peak memory_usage %d bytes; want the guard to refuse before the stage folds, below %d bytes",
							s.version, tc.name, peak, classicWindowRefusedPeakBytes)
					}
				})
			}
		})
	}
}

// serveClassicWindow issues `histogram_quantile(0.5, sum by (le) (fn(metric[rng])))`
// at classicMergeEvalTime and returns the status and body.
func serveClassicWindow(ctx context.Context, mux http.Handler, fn, metric, rng string) (int, string) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("histogram_quantile(0.5, sum by (le) (%s(%s_bucket[%s])))", fn, metric, rng))
	params.Set("time", strconv.FormatInt(classicMergeEvalTime.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// classicWindowMedian is the median the seeds imply: every bucket of a
// series grows equally, so the 50 finite buckets and +Inf split the rate
// evenly, and the median falls halfway through the bucket bounded by 25 and
// 26 for a shared layout.
const classicWindowMedian = 25.5

// classicWindowMedianTolerance absorbs the float rounding of the rate
// arithmetic.
const classicWindowMedianTolerance = 1e-6

// assertClassicWindowAnswer checks an admitted probe answered with one
// sample at classicWindowMedian.
func assertClassicWindowAnswer(t *testing.T, s *server, code int, body string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s; want the admitted shape answered", s.version, code, body)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	decodeJSON(t, body, &resp)
	if len(resp.Data.Result) != 1 {
		t.Fatalf("%s: %d series in the answer; want one: %s", s.version, len(resp.Data.Result), body)
	}
	raw, _ := resp.Data.Result[0].Value[1].(string)
	got, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.Abs(got-classicWindowMedian) > classicWindowMedianTolerance {
		t.Fatalf("%s: quantile %q; want %v", s.version, raw, classicWindowMedian)
	}
}
