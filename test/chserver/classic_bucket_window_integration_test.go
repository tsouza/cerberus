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

// classicWindowShape is one seed: series series of 50-bucket layouts, each
// carrying samples samples spaced spacing apart and ending at
// classicMergeEvalTime. Bucket j of a series' k-th newest sample counts
// samples - k observations, so every series is a counter whose buckets grow
// equally. The layout is shared, bounds 1 to 50 on every series, unless
// disjoint gives series i its own bounds i*50+1 to i*50+50.
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
	metric := fmt.Sprintf("classic_window_%d_%d_%d", sh.series, sh.samples, sh.spacing.Milliseconds())
	boundOffset := "0"
	if sh.disjoint {
		metric += "_disjoint"
		boundOffset = fmt.Sprintf("intDiv(number, %d) * %d", sh.samples, classicWindowWidth)
	}
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalMillisecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(%d - (number %% %d))),
  arrayMap(j -> toFloat64(%s + j), range(1, %d))
FROM numbers(%d)`,
		metric, sh.samples,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), sh.samples, sh.spacing.Milliseconds(),
		classicWindowWidth+1, sh.samples, sh.samples,
		boundOffset, classicWindowWidth+1,
		sh.series*sh.samples))
	return metric
}

// The seeds. The wide and narrow ones sit on either side of the per-series
// ceiling.
var (
	// classicWindowWideSeries carries 3,000 x 51 = 153,000 entries in one
	// series, past the 152,520 per-series ceiling the 1 GiB cap derives.
	classicWindowWideSeries = classicWindowShape{series: 1, samples: 3_000, spacing: 100 * time.Millisecond}
	// classicWindowNarrowSeries carries 2,950 x 51 = 150,450 entries in
	// each of two series, within that ceiling.
	classicWindowNarrowSeries = classicWindowShape{series: 2, samples: 2_950, spacing: 100 * time.Millisecond}
)

// classicWindowRange is the range every probe's range function reads.
const classicWindowRange = 5 * time.Minute

// classicWindowShortSeries are many short series that answer under the
// 1 GiB cap on both builds: 58,000 series at a 10-second scrape, 28 and 30
// samples in the range. Their peaks are in the many-short-series table of
// internal/promql/classic_bucket_window_bound.go.
var classicWindowShortSeries = []classicWindowShape{
	{series: 58_000, samples: 28, spacing: 10 * time.Second},
	{series: 58_000, samples: 30, spacing: 10 * time.Second},
}

// classicWindowRefusal is how the handler answers a shape it cannot answer
// under the 1 GiB cap. Its String is the subtest prefix.
type classicWindowRefusal int

const (
	// classicWindowOutOfMemory: the statement runs into ClickHouse's
	// per-query cap (code 241) and the handler answers HTTP 422 with the
	// classic-histogram memory-limit message.
	classicWindowOutOfMemory classicWindowRefusal = iota
	// classicWindowSeriesBudget: the per-series check refuses one series
	// past the per-series ceiling with HTTP 422 and its own message.
	classicWindowSeriesBudget
)

func (r classicWindowRefusal) String() string {
	if r == classicWindowOutOfMemory {
		return "out-of-memory"
	}
	return "refused"
}

// chQueryMemoryLimitText opens the exception ClickHouse records when a query
// reaches its own per-query cap, as opposed to the server-wide limit.
const chQueryMemoryLimitText = "Query memory limit exceeded"

// classicWindowRefused are the shapes that run the window stage out of
// memory under the 1 GiB cap on at least one build when nothing refuses
// them first (the tables of internal/promql/classic_bucket_window_bound.go),
// each with the range function it is queried through and the refusal the
// handler answers it with.
var classicWindowRefused = []struct {
	name  string
	shape classicWindowShape
	fn    string
	want  classicWindowRefusal
}{
	{name: "shared-rate", shape: classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second}, fn: "rate", want: classicWindowOutOfMemory},
	{name: "disjoint-rate", shape: classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second, disjoint: true}, fn: "rate", want: classicWindowOutOfMemory},
	{name: "short-series-rate", shape: classicWindowNearestRefusedShortSeries, fn: "rate", want: classicWindowOutOfMemory},
	// One sample per series: the spacing only names the metric.
	{name: "million-series-sum-over-time", shape: classicWindowShape{series: 1_000_000, samples: 1, spacing: time.Second, disjoint: true}, fn: "sum_over_time", want: classicWindowOutOfMemory},
	// 6,200 x 51 = 316,200 and 60,000 x 51 = 3,060,000 entries per series.
	{name: "long-series-rate", shape: classicWindowShape{series: 200, samples: 6_200, spacing: 40 * time.Millisecond}, fn: "rate", want: classicWindowSeriesBudget},
	{name: "one-series-rate", shape: classicWindowShape{series: 1, samples: 60_000, spacing: 4 * time.Millisecond}, fn: "rate", want: classicWindowSeriesBudget},
}

// classicWindowNearestRefusedShortSeries is the smallest many-short-series
// shape measured to run out of memory on every run on both builds: 58,000
// series of 96 samples, 3.1 seconds apart so all 96 fall in the range. Of the
// smaller shapes measured, 58,000 x 72 answers on both builds and
// 58,000 x 84 answers on 26.6 (the many-short-series table of
// internal/promql/classic_bucket_window_bound.go); nothing between 84 and 96
// samples was measured.
var classicWindowNearestRefusedShortSeries = classicWindowShape{series: 58_000, samples: 96, spacing: 3100 * time.Millisecond}

// TestClassicBucketWindow_CeilingsAcrossBuilds serves the instant
// classic-histogram quantile through the production handler under the 1 GiB
// query memory cap and the default resource bounds, on each build. Two
// series just within the per-series ceiling and 58,000 short series at a
// 10-second scrape answer with the quantile their seed implies. One series
// past the per-series ceiling is refused by the per-series check with its
// own message, at a peak far below the cap. Every shape that runs out of
// memory when nothing refuses it first is answered with HTTP 422: the
// per-series check's message where that check refuses it, and otherwise the
// classic-histogram memory-limit message, with query_log recording
// ClickHouse's code 241.
func TestClassicBucketWindow_CeilingsAcrossBuilds(t *testing.T) {
	seriesCeiling := promql.ClassicBucketWindowSeriesEntriesForMemory(classicMergeQueryMemoryBytes)
	if got := classicWindowWideSeries.samples * (classicWindowWidth + 1); int64(got) <= seriesCeiling {
		t.Fatalf("wide series carries %d entries; want past the per-series ceiling %d", got, seriesCeiling)
	}
	if got := classicWindowNarrowSeries.samples * (classicWindowWidth + 1); int64(got) > seriesCeiling {
		t.Fatalf("narrow series carry %d entries; want within the per-series ceiling %d", got, seriesCeiling)
	}
	shapes := append([]classicWindowShape{classicWindowWideSeries, classicWindowNarrowSeries}, classicWindowShortSeries...)
	for _, r := range classicWindowRefused {
		shapes = append(shapes, r.shape)
		if got := int64(r.shape.samples * (classicWindowWidth + 1)); (r.want == classicWindowSeriesBudget) != (got > seriesCeiling) {
			t.Fatalf("%s carries %d entries per series against the per-series ceiling %d; want past it exactly when the per-series check refuses it",
				r.name, got, seriesCeiling)
		}
	}
	for _, sh := range shapes {
		if oldest := time.Duration(sh.samples-1) * sh.spacing; oldest >= classicWindowRange {
			t.Fatalf("%+v: oldest sample %v before the evaluation instant; want every sample inside the %v range", sh, oldest, classicWindowRange)
		}
	}
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
				code, body := serveClassicWindow(ctx, mux, "rate", narrow)
				assertClassicWindowAnswer(t, s, code, body)
			})
			for _, sh := range classicWindowShortSeries {
				t.Run(fmt.Sprintf("admitted/short-series-%d", sh.samples), func(t *testing.T) {
					metric := sh.seed(ctx, t, s)
					code, body := serveClassicWindow(ctx, mux, "rate", metric)
					assertClassicWindowAnswer(t, s, code, body)
				})
			}
			wide := classicWindowWideSeries.seed(ctx, t, s)
			t.Run("refused/wide-series", func(t *testing.T) {
				qid := fmt.Sprintf("classic-window-%s-wide-series-%d", s.version, time.Now().UnixNano())
				code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, "rate", wide)
				if code != http.StatusUnprocessableEntity || !strings.Contains(body, chplan.ClassicBucketWindowSeriesBudgetMessage) {
					t.Fatalf("%s: HTTP %d: %s; want 422 carrying %q", s.version, code, body, chplan.ClassicBucketWindowSeriesBudgetMessage)
				}
				if peak := s.exceptionPeakMemory(ctx, t, qid); peak >= classicWindowRefusedPeakBytes {
					t.Fatalf("%s: refused at peak memory_usage %d bytes; want the guard to refuse before the stage folds, below %d bytes",
						s.version, peak, classicWindowRefusedPeakBytes)
				}
			})
			for _, r := range classicWindowRefused {
				t.Run(r.want.String()+"/"+r.name, func(t *testing.T) {
					metric := r.shape.seed(ctx, t, s)
					qid := fmt.Sprintf("classic-window-%s-%s-%d", s.version, r.name, time.Now().UnixNano())
					code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, r.fn, metric)
					assertClassicWindowRefusal(ctx, t, s, qid, r.want, code, body)
				})
			}
		})
	}
}

// assertClassicWindowRefusal checks the probe qid was answered with want's
// HTTP 422.
func assertClassicWindowRefusal(ctx context.Context, t *testing.T, s *server, qid string, want classicWindowRefusal, code int, body string) {
	t.Helper()
	switch want {
	case classicWindowSeriesBudget:
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, chplan.ClassicBucketWindowSeriesBudgetMessage) {
			t.Fatalf("%s: HTTP %d: %s; want 422 carrying %q", s.version, code, body, chplan.ClassicBucketWindowSeriesBudgetMessage)
		}
	case classicWindowOutOfMemory:
		message := fmt.Sprintf("%s (per-query cap %d bytes)", chplan.ClassicHistogramMemoryLimitMessage, classicMergeQueryMemoryBytes)
		var resp struct {
			ErrorType string `json:"errorType"`
			Error     string `json:"error"`
		}
		decodeJSON(t, body, &resp)
		if code != http.StatusUnprocessableEntity || resp.ErrorType != "execution" || resp.Error != message {
			t.Fatalf("%s: HTTP %d: %s; want 422 execution %q", s.version, code, body, message)
		}
		if got := s.exceptionCode(ctx, t, qid); got != chMemoryLimitExceededCode {
			t.Fatalf("%s: query_log records exception code %d; want ClickHouse's %d at the per-query cap",
				s.version, got, chMemoryLimitExceededCode)
		}
		if got := s.exceptionMessage(ctx, t, qid); !strings.HasPrefix(got, fmt.Sprintf("Code: %d. DB::Exception: %s", chMemoryLimitExceededCode, chQueryMemoryLimitText)) {
			t.Fatalf("%s: query_log records %q; want the per-query cap's %q", s.version, got, chQueryMemoryLimitText)
		}
	default:
		t.Fatalf("unknown refusal %d", want)
	}
}

// exceptionMessage is the exception text query_log records for the failed
// query qid.
func (s *server) exceptionMessage(ctx context.Context, t *testing.T, qid string) string {
	t.Helper()
	s.flushLogs(ctx, t)
	var msg string
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT any(exception) FROM system.query_log WHERE type = 'ExceptionWhileProcessing' AND query_id = ?", qid).Scan(&msg); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return msg
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

// serveClassicWindow issues
// `histogram_quantile(0.5, sum by (le) (fn(metric[classicWindowRange])))` at
// classicMergeEvalTime and returns the status and body.
func serveClassicWindow(ctx context.Context, mux http.Handler, fn, metric string) (int, string) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("histogram_quantile(0.5, sum by (le) (%s(%s_bucket[%ds])))", fn, metric, int(classicWindowRange/time.Second)))
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
