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
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicBuild263 is the 26.3 build the classic-quantile kernel (#4518) and
// the classic-bucket layout (#4519) measurements ran on.
const classicBuild263 = "clickhouse/clickhouse-server:26.3.17.56-alpine"

// classicKernelBlockBuilds are the builds the kernel measurement covers.
var classicKernelBlockBuilds = []string{classicBuild263, classicBuild268}

// The seed: classicKernelBlockSeries series of classicKernelBlockBounds finite
// bounds (1 to classicKernelBlockBounds) plus +Inf, one sample every
// classicRangeStep over the hour before classicMergeEvalTime. Bucket i of
// series s grows by classicKernelBlockIncrement(s, i) per sample, so the
// buckets of one series grow unequally.
const (
	classicKernelBlockSeries = 500
	classicKernelBlockBounds = 10
	classicKernelBlockSpread = 5
)

// classicKernelBlockIncrement is bucket i's growth per sample in series s.
func classicKernelBlockIncrement(s, i int) float64 {
	return float64(1 + (s+i)%classicKernelBlockSpread)
}

// classicKernelBlockStep is the range's evaluation step.
const classicKernelBlockStep = time.Minute

// classicKernelBlockPoints is the number of points of each series' answer:
// every step of the hour but the first, whose window holds no sample.
const classicKernelBlockPoints = int(classicRangeSpan / classicKernelBlockStep)

// The setting that decides which blocks the quantile kernel runs over. At
// the server's default (1) ClickHouse runs the expressions the range result's
// series-order sort does not need after that sort, over its output blocks;
// at classicKernelBelowSort it runs them before the sort, over the blocks
// the aggregation beneath hands up.
const (
	classicKernelSortSetting = "query_plan_execute_functions_after_sorting"
	classicKernelBelowSort   = 0
)

// classicKernelBlockMaxRatio bounds the served statement's peak against the
// same statement with the kernel run before the sort. A kernel whose peak
// grows with the block it runs over fails it: over this shape the kernel
// that tested length(BucketCounts) first peaked at 800 MiB served against
// 100 MiB before the sort on 26.3.17.56 (8.0x) and 803 against 76 MiB on
// 26.8.36.3 (10.6x); the kernel without that test at 100 against 100 MiB
// (1.0x) and 102 against 77 MiB (1.3x) (#4518).
const classicKernelBlockMaxRatio = 2

// TestClassicBucketRange_KernelPeakAcrossBlocks serves the per-series range
// quantile `histogram_quantile(0.9, rate(<bucket>[5m]))` through the
// production handler on each build, checks every point against the quantile
// the seed implies, then runs the dispatched statement again with the kernel
// moved before the series-order sort. The served statement, whose kernel runs
// after that sort at the server default
// (query_plan_execute_functions_after_sorting=1), must peak at no more than
// classicKernelBlockMaxRatio times the second run (#4518).
func TestClassicBucketRange_KernelPeakAcrossBlocks(t *testing.T) {
	for _, image := range classicKernelBlockBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			metric := seedClassicKernelBlock(ctx, t, s)
			mux := classicMergeMux(ctx, t, s, "auto")

			qid := fmt.Sprintf("classic-kernel-block-%s-%d", s.version, time.Now().UnixNano())
			code, body := serveClassicKernelBlock(chclient.WithQueryID(ctx, qid), mux, metric)
			assertClassicKernelBlockAnswer(t, s, code, body)
			served := s.classicKernelBlockRecord(ctx, t, qid)

			replayed, belowSort := s.classicReplay(ctx, t, served, qid+"-below-sort", classicKernelSortSetting, classicKernelBelowSort)
			if replayed != classicKernelBlockSeries*classicKernelBlockPoints {
				t.Fatalf("%s: the statement with the kernel before the sort returned %d rows; want %d",
					s.version, replayed, classicKernelBlockSeries*classicKernelBlockPoints)
			}

			t.Logf("%s: peak %d bytes served, %d with the kernel before the sort", s.version, served.peak, belowSort.peak)
			if served.peak > classicKernelBlockMaxRatio*belowSort.peak {
				t.Fatalf("%s: the served statement peaked at %d bytes against %d with the kernel before the sort; want at most %dx",
					s.version, served.peak, belowSort.peak, classicKernelBlockMaxRatio)
			}
		})
	}
}

// seedClassicKernelBlock inserts the seed under its own metric name and
// returns that name.
func seedClassicKernelBlock(ctx context.Context, t *testing.T, s *server) string {
	t.Helper()
	metric := fmt.Sprintf("classic_kernel_block_%d_%d", classicKernelBlockSeries, classicKernelBlockBounds)
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalMillisecond((number %% %d) * %d),
  arrayMap(i -> toUInt64((%d - (number %% %d)) * (1 + (intDiv(number, %d) + i) %% %d)), range(%d)),
  arrayMap(j -> toFloat64(j), range(1, %d))
FROM numbers(%d)`,
		metric, classicRangeSamples,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), classicRangeSamples, classicRangeStep.Milliseconds(),
		classicRangeSamples, classicRangeSamples, classicRangeSamples, classicKernelBlockSpread, classicKernelBlockBounds+1,
		classicKernelBlockBounds+1,
		classicKernelBlockSeries*classicRangeSamples))
	return metric
}

// serveClassicKernelBlock issues the per-series range quantile over metric
// across classicRangeSpan ending at classicMergeEvalTime and returns the
// status and body.
func serveClassicKernelBlock(ctx context.Context, mux http.Handler, metric string) (int, string) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("histogram_quantile(%v, rate(%s_bucket[%ds]))", classicRangeQuantile, metric, int(classicWindowRange/time.Second)))
	params.Set("start", strconv.FormatInt(classicMergeEvalTime.Add(-classicRangeSpan).Unix(), 10))
	params.Set("end", strconv.FormatInt(classicMergeEvalTime.Unix(), 10))
	params.Set("step", strconv.Itoa(int(classicKernelBlockStep/time.Second)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// classicKernelBlockQuantile is the classicRangeQuantile quantile series s's
// rates imply, by Prometheus's bucketQuantile over the cumulative ladder of
// classicKernelBlockIncrement: every bucket's rate is its increment over the
// same window, so the ladder's shape, and the quantile, are the increments'.
func classicKernelBlockQuantile(s int) float64 {
	cum := make([]float64, classicKernelBlockBounds+1)
	total := 0.0
	for i := range cum {
		total += classicKernelBlockIncrement(s, i)
		cum[i] = total
	}
	rank := classicRangeQuantile * total
	idx := 0
	for cum[idx] < rank {
		idx++
	}
	if idx == classicKernelBlockBounds {
		return float64(classicKernelBlockBounds)
	}
	lowerBound, lowerCum := 0.0, 0.0
	if idx > 0 {
		lowerBound, lowerCum = float64(idx), cum[idx-1]
	}
	upperBound := float64(idx + 1)
	return lowerBound + (upperBound-lowerBound)*((rank-lowerCum)/(cum[idx]-lowerCum))
}

// assertClassicKernelBlockAnswer checks the probe answered
// classicKernelBlockSeries series of classicKernelBlockPoints points, each at
// the quantile classicKernelBlockQuantile gives its series.
func assertClassicKernelBlockAnswer(t *testing.T, s *server, code int, body string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s; want the quantile answered", s.version, code, body)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	decodeJSON(t, body, &resp)
	if len(resp.Data.Result) != classicKernelBlockSeries {
		t.Fatalf("%s: %d series in the answer; want %d", s.version, len(resp.Data.Result), classicKernelBlockSeries)
	}
	for _, series := range resp.Data.Result {
		id, err := strconv.Atoi(series.Metric["svc"])
		if err != nil {
			t.Fatalf("%s: series %v carries no numeric svc label", s.version, series.Metric)
		}
		want := classicKernelBlockQuantile(id)
		if len(series.Values) != classicKernelBlockPoints {
			t.Fatalf("%s: series %d answered %d points; want %d", s.version, id, len(series.Values), classicKernelBlockPoints)
		}
		for _, v := range series.Values {
			raw, _ := v[1].(string)
			got, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.Abs(got-want) > classicWindowMedianTolerance {
				t.Fatalf("%s: series %d answered %q at %v; want %v", s.version, id, raw, v[0], want)
			}
		}
	}
}

// classicKernelBlockRun is what system.query_log records for one finished
// statement.
type classicKernelBlockRun struct {
	statement string
	settings  map[string]string
	peak      uint64
}

// classicKernelBlockRecord reads the finished query qid's statement, settings
// and peak memory from system.query_log.
func (s *server) classicKernelBlockRecord(ctx context.Context, t *testing.T, qid string) classicKernelBlockRun {
	t.Helper()
	s.flushLogs(ctx, t)
	var r classicKernelBlockRun
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT query, Settings, memory_usage FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ? ORDER BY memory_usage DESC LIMIT 1",
		qid).Scan(&r.statement, &r.settings, &r.peak); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return r
}

// classicReplay reruns run's statement under its own settings with setting
// overridden to value, as the query replayID, and returns the rows it
// answered and what system.query_log recorded for it.
func (s *server) classicReplay(ctx context.Context, t *testing.T, run classicKernelBlockRun, replayID, setting string, value any) (int, classicKernelBlockRun) {
	t.Helper()
	settings := clickhouse.Settings{}
	for name, v := range run.settings {
		settings[name] = v
	}
	settings[setting] = value
	rows, err := s.admin.Conn().Query(clickhouse.Context(ctx, clickhouse.WithQueryID(replayID), clickhouse.WithSettings(settings)), run.statement)
	if err != nil {
		t.Fatalf("%s: replay with %s=%v: %v", s.version, setting, value, err)
	}
	replayed := 0
	for rows.Next() {
		replayed++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: replay with %s=%v: %v", s.version, setting, value, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("%s: close the replay with %s=%v: %v", s.version, setting, value, err)
	}
	return replayed, s.classicKernelBlockRecord(ctx, t, replayID)
}
