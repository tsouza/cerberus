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

	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicServerMaxThreads is the max_threads classicServerThreadsConfig gives
// a server's default profile: the value ClickHouse sizes from a 16-core host.
const classicServerMaxThreads = 16

// classicServerThreadsConfig sets the default profile's max_threads to
// classicServerMaxThreads.
func classicServerThreadsConfig() testcontainers.ContainerCustomizer {
	config := fmt.Sprintf("<clickhouse><profiles><default><max_threads>%d</max_threads></default></profiles></clickhouse>",
		classicServerMaxThreads)
	return testcontainers.WithFiles(testcontainers.ContainerFile{
		Reader:            strings.NewReader(config),
		ContainerFilePath: "/etc/clickhouse-server/users.d/max-threads.xml",
		FileMode:          configFileMode,
	})
}

// The range every probe evaluates: one hour ending at classicMergeEvalTime
// at a 15-second step, the scrape interval of the seeds.
const (
	classicRangeSpan = time.Hour
	classicRangeStep = 15 * time.Second
)

// classicRangeSteps is the number of evaluation steps across the range, both
// ends included.
const classicRangeSteps = int(classicRangeSpan/classicRangeStep) + 1

// classicRangeSamples is each seeded series' sample count: one per step
// interval of classicRangeSpan, the newest at classicMergeEvalTime and the
// oldest one step after the range's start.
const classicRangeSamples = int(classicRangeSpan / classicRangeStep)

// classicRangePoints is the number of steps whose window holds the two
// samples rate needs: every step but the first two, whose windows hold no
// sample and one sample.
const classicRangePoints = classicRangeSteps - 2

// The seeds, series of one shared 50-bucket layout at a 15-second scrape
// over the hour. On a server running a statement on sixteen threads,
// 250 series peak at 436-438 MiB on 26.8 and 572 MiB on 26.6, and 16,000
// series run out of the 1 GiB cap on every run on both builds. Between them,
// 1,000 to 4,000 series answer on some runs and run out of memory on others,
// and which differs by build and by thread count.
var (
	classicRangeAdmitted = classicWindowShape{series: 250, samples: classicRangeSamples, spacing: classicRangeStep}
	classicRangeOverCap  = classicWindowShape{series: 16_000, samples: classicRangeSamples, spacing: classicRangeStep}
)

// classicRangeQuantile is the quantile every probe asks for, and
// classicRangeQuantileValue the value the seeds imply: every bucket of a
// series grows equally, so the 50 finite buckets and +Inf split the rate
// evenly and the 0.9 quantile falls nine tenths of the way through the
// bucket bounded by 45 and 46.
const (
	classicRangeQuantile      = 0.9
	classicRangeQuantileValue = classicRangeQuantile * (classicWindowWidth + 1)
)

// The statement of every probe must be the ts_grid_range lowering: it carries
// classicRangeGridFn, the function that lowering computes each per-bucket rate
// with, and not classicRangeLadderFn, which only the classic bucket-ladder
// lowering (chplan.RangeBucketGridNative) emits.
const (
	classicRangeGridFn   = "timeSeriesRateToGrid"
	classicRangeLadderFn = "timeSeriesResetsToGrid"
)

// classicCalibratedThreads is the thread count the classic-histogram
// calibration ran at; the admitted probe must have run on more.
const classicCalibratedThreads = 2

// TestClassicBucketRange_TsGridOverCapAcrossBuilds serves the range
// classic-histogram quantile `histogram_quantile(0.9, sum by (le)
// (rate(<bucket>[classicWindowRange])))` over one hour at a 15-second step through the
// production handler under the 1 GiB query memory cap, on each build, from a
// server whose default profile runs a statement on sixteen threads. The
// handler lowers it through ts_grid_range. A shape under the cap answers
// with the quantile its seed implies at every step; a shape past the cap
// fails at ClickHouse's per-query cap and is answered with HTTP 422 and the
// classic-histogram memory-limit message, never ClickHouse's own error.
func TestClassicBucketRange_TsGridOverCapAcrossBuilds(t *testing.T) {
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image, classicServerThreadsConfig())
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			var threads string
			if err := s.admin.Conn().QueryRow(ctx, "SELECT value FROM system.settings WHERE name = 'max_threads'").Scan(&threads); err != nil {
				t.Fatalf("%s: read max_threads: %v", s.version, err)
			}
			if threads != strconv.Itoa(classicServerMaxThreads) {
				t.Fatalf("%s: default max_threads %s; want %d", s.version, threads, classicServerMaxThreads)
			}
			mux := classicMergeMux(ctx, t, s, "auto")
			t.Run("admitted", func(t *testing.T) {
				metric := classicRangeAdmitted.seed(ctx, t, s)
				qid := fmt.Sprintf("classic-range-%s-admitted-%d", s.version, time.Now().UnixNano())
				code, body := serveClassicRange(chclient.WithQueryID(ctx, qid), mux, metric)
				assertClassicRangeAnswer(t, s, code, body)
				if threads := assertClassicRangeGridStatement(ctx, t, s, qid); threads <= classicCalibratedThreads {
					t.Fatalf("%s: the statement peaked at %d threads; want more than %d", s.version, threads, classicCalibratedThreads)
				}
			})
			t.Run("out-of-memory", func(t *testing.T) {
				metric := classicRangeOverCap.seed(ctx, t, s)
				qid := fmt.Sprintf("classic-range-%s-out-of-memory-%d", s.version, time.Now().UnixNano())
				code, body := serveClassicRange(chclient.WithQueryID(ctx, qid), mux, metric)
				assertClassicWindowRefusal(ctx, t, s, qid, classicWindowOutOfMemory, code, body)
				assertClassicRangeGridStatement(ctx, t, s, qid)
			})
		})
	}
}

// serveClassicRange issues the range quantile over metric across
// classicRangeSpan ending at classicMergeEvalTime and returns the status and
// body.
func serveClassicRange(ctx context.Context, mux http.Handler, metric string) (int, string) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("histogram_quantile(%v, sum by (le) (rate(%s_bucket[%ds])))", classicRangeQuantile, metric, int(classicWindowRange/time.Second)))
	params.Set("start", strconv.FormatInt(classicMergeEvalTime.Add(-classicRangeSpan).Unix(), 10))
	params.Set("end", strconv.FormatInt(classicMergeEvalTime.Unix(), 10))
	params.Set("step", strconv.Itoa(int(classicRangeStep/time.Second)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// assertClassicRangeAnswer checks an admitted probe answered one series of
// classicRangePoints points, each at classicRangeQuantileValue.
func assertClassicRangeAnswer(t *testing.T, s *server, code int, body string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s; want the admitted shape answered", s.version, code, body)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	decodeJSON(t, body, &resp)
	if len(resp.Data.Result) != 1 {
		t.Fatalf("%s: %d series in the answer; want one: %s", s.version, len(resp.Data.Result), body)
	}
	values := resp.Data.Result[0].Values
	if len(values) != classicRangePoints {
		t.Fatalf("%s: %d points; want %d", s.version, len(values), classicRangePoints)
	}
	for _, v := range values {
		raw, _ := v[1].(string)
		got, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.Abs(got-classicRangeQuantileValue) > classicWindowMedianTolerance {
			t.Fatalf("%s: quantile %q at %v; want %v", s.version, raw, v[0], classicRangeQuantileValue)
		}
	}
}

// assertClassicRangeGridStatement checks the statement query_log records
// for qid is the ts_grid_range lowering, and returns the peak number of
// threads it ran on.
func assertClassicRangeGridStatement(ctx context.Context, t *testing.T, s *server, qid string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var query string
	var threads uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT any(query), max(peak_threads_usage) FROM system.query_log WHERE type != 'QueryStart' AND query_id = ?", qid).Scan(&query, &threads); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if !strings.Contains(query, classicRangeGridFn) || strings.Contains(query, classicRangeLadderFn) {
		t.Fatalf("%s: statement %q; want the ts_grid_range lowering, carrying %s and not %s",
			s.version, query, classicRangeGridFn, classicRangeLadderFn)
	}
	return threads
}
