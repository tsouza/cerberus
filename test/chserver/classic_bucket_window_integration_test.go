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

// classicWindowRefusedPeakBytes bounds the peak memory of a probe the
// per-series check refuses: it refuses classicWindowWideSeries once the
// series is collected and before it is folded (16-20 MiB on both builds),
// where folding that series alone takes about 63 MiB at the fold's 433 B per
// entry.
const classicWindowRefusedPeakBytes = 48 << 20

// chMemoryLimitExceededCode is ClickHouse's MEMORY_LIMIT_EXCEEDED error code,
// the one query_log records for a statement aborted at the per-query cap.
const chMemoryLimitExceededCode = 241

// classicWindowShape is one seed: series series of one shared 50-bucket
// layout, each carrying samples samples spaced spacing apart and ending at
// classicMergeEvalTime. Bucket j of a series' k-th newest sample counts
// samples - k observations, so every series is a counter whose buckets grow
// equally.
type classicWindowShape struct {
	series, samples int
	spacing         time.Duration
}

// classicWindowWidth is every seed's finite bound count.
const classicWindowWidth = 50

// seed inserts shape under its own metric name on s and returns that name.
func (sh classicWindowShape) seed(ctx context.Context, t *testing.T, s *server) string {
	t.Helper()
	metric := fmt.Sprintf("classic_window_%d_%d_%d", sh.series, sh.samples, sh.spacing.Milliseconds())
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalMillisecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(%d - (number %% %d))),
  arrayMap(j -> toFloat64(j), range(1, %d))
FROM numbers(%d)`,
		metric, sh.samples,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), sh.samples, sh.spacing.Milliseconds(),
		classicWindowWidth+1, sh.samples, sh.samples,
		classicWindowWidth+1,
		sh.series*sh.samples))
	return metric
}

// The seeds. classicWindowShared runs the window stage out of memory under
// the 1 GiB cap on both builds; the wide and narrow ones sit on either side
// of the per-series ceiling.
var (
	classicWindowShared = classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second}
	// classicWindowWideSeries carries 3,000 x 51 = 153,000 entries in one
	// series, past the 152,520 per-series ceiling the 1 GiB cap derives.
	classicWindowWideSeries = classicWindowShape{series: 1, samples: 3_000, spacing: 100 * time.Millisecond}
	// classicWindowNarrowSeries carries 2,950 x 51 = 150,450 entries in
	// each of two series, within that ceiling.
	classicWindowNarrowSeries = classicWindowShape{series: 2, samples: 2_950, spacing: 100 * time.Millisecond}
)

// TestClassicBucketWindow_CeilingsAcrossBuilds serves the instant
// classic-histogram quantile through the production handler under the 1 GiB
// query memory cap and the default resource bounds, on each build. The
// 58,000 x 50 x 112 shape runs the window stage into ClickHouse's per-query
// cap (code 241), and the handler answers it with HTTP 422 and the
// classic-histogram memory-limit message. One series past the per-series
// ceiling is refused by the per-series check with its own message, at a peak
// far below the cap, and two series just within it answer with the quantile
// their seed implies.
func TestClassicBucketWindow_CeilingsAcrossBuilds(t *testing.T) {
	seriesCeiling := promql.ClassicBucketWindowSeriesEntriesForMemory(classicMergeQueryMemoryBytes)
	if got := classicWindowWideSeries.samples * (classicWindowWidth + 1); int64(got) <= seriesCeiling {
		t.Fatalf("wide series carries %d entries; want past the per-series ceiling %d", got, seriesCeiling)
	}
	if got := classicWindowNarrowSeries.samples * (classicWindowWidth + 1); int64(got) > seriesCeiling {
		t.Fatalf("narrow series carry %d entries; want within the per-series ceiling %d", got, seriesCeiling)
	}
	memoryLimitMessage := fmt.Sprintf("%s (per-query cap %d bytes)", chplan.ClassicHistogramMemoryLimitMessage, classicMergeQueryMemoryBytes)
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			mux := classicMergeMux(ctx, t, s, "auto")
			narrow := classicWindowNarrowSeries.seed(ctx, t, s)
			t.Run("admitted/per-series", func(t *testing.T) {
				code, body := serveClassicWindow(ctx, mux, "rate", narrow, "5m")
				assertClassicWindowAnswer(t, s, code, body)
			})
			wide := classicWindowWideSeries.seed(ctx, t, s)
			t.Run("refused/wide-series", func(t *testing.T) {
				qid := fmt.Sprintf("classic-window-%s-wide-series-%d", s.version, time.Now().UnixNano())
				code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, "rate", wide, "5m")
				if code != http.StatusUnprocessableEntity || !strings.Contains(body, chplan.ClassicBucketWindowSeriesBudgetMessage) {
					t.Fatalf("%s: HTTP %d: %s; want 422 carrying %q", s.version, code, body, chplan.ClassicBucketWindowSeriesBudgetMessage)
				}
				if peak := s.exceptionPeakMemory(ctx, t, qid); peak >= classicWindowRefusedPeakBytes {
					t.Fatalf("%s: refused at peak memory_usage %d bytes; want the guard to refuse before the stage folds, below %d bytes",
						s.version, peak, classicWindowRefusedPeakBytes)
				}
			})
			shared := classicWindowShared.seed(ctx, t, s)
			t.Run("out-of-memory/shared-rate", func(t *testing.T) {
				qid := fmt.Sprintf("classic-window-%s-out-of-memory-%d", s.version, time.Now().UnixNano())
				code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, "rate", shared, "5m")
				var resp struct {
					ErrorType string `json:"errorType"`
					Error     string `json:"error"`
				}
				decodeJSON(t, body, &resp)
				if code != http.StatusUnprocessableEntity || resp.ErrorType != "execution" || resp.Error != memoryLimitMessage {
					t.Fatalf("%s: HTTP %d: %s; want 422 execution %q", s.version, code, body, memoryLimitMessage)
				}
				if got := s.exceptionCode(ctx, t, qid); got != chMemoryLimitExceededCode {
					t.Fatalf("%s: query_log records exception code %d; want ClickHouse's %d at the per-query cap",
						s.version, got, chMemoryLimitExceededCode)
				}
			})
		})
	}
}

// exceptionCode is the exception code query_log records for the failed
// query qid.
func (s *server) exceptionCode(ctx context.Context, t *testing.T, qid string) int32 {
	t.Helper()
	s.flushLogs(ctx, t)
	var code int32
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(exception_code) FROM system.query_log WHERE type = 'ExceptionWhileProcessing' AND query_id = ?", qid).Scan(&code); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return code
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
