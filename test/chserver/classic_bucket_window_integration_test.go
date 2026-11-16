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
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicWindowRefusedPeakBytes bounds the peak memory of a probe the
// per-series check refuses: it refuses the series once the stage has
// collected it and before the fold runs, while the fold of that series alone
// runs past the 1 GiB cap. Half the cap separates the two: 1 x 75,000 over 16
// parts is refused at 208-260 MiB on both builds at two and sixteen threads,
// and 1 x 74,900 fails at 1,013-1,024 MiB.
const classicWindowRefusedPeakBytes = classicMergeQueryMemoryBytes / 2

// chMemoryLimitExceededCode is ClickHouse's MEMORY_LIMIT_EXCEEDED error code,
// the one query_log records for a statement aborted at the per-query cap.
const chMemoryLimitExceededCode = 241

// classicWindowShape is one seed: series series of 50-bucket layouts, each
// carrying samples samples spaced spacing apart and ending at
// classicMergeEvalTime. Bucket j of a series' k-th newest sample counts
// samples - k observations, so every series is a counter whose buckets grow
// equally. The layout is shared, bounds 1 to 50 on every series, unless
// disjoint gives series i its own bounds i*50+1 to i*50+50. With parts above
// one the rows are inserted in that many INSERTs of consecutive rows, each
// its own data part, so ClickHouse reads a single series on several threads.
type classicWindowShape struct {
	series, samples int
	spacing         time.Duration
	disjoint        bool
	parts           int
}

// classicWindowSeriesParts is the number of data parts a single-series probe
// is spread over.
const classicWindowSeriesParts = 16

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
	rows := sh.series * sh.samples
	parts := max(sh.parts, 1)
	for part := range parts {
		start, end := rows*part/parts, rows*(part+1)/parts
		s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalMillisecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(%d - (number %% %d))),
  arrayMap(j -> toFloat64(%s + j), range(1, %d))
FROM numbers(%d, %d)`,
			metric, sh.samples,
			classicMergeEvalTime.Format("2006-01-02 15:04:05"), sh.samples, sh.spacing.Milliseconds(),
			classicWindowWidth+1, sh.samples, sh.samples,
			boundOffset, classicWindowWidth+1,
			start, end-start))
	}
	return metric
}

// classicWindowRange is the range every probe's range function reads.
const classicWindowRange = 5 * time.Minute

// classicWindowShortSeries are many short series that answer under the
// 1 GiB cap on both builds: 58,000 series of 28 and 30 samples at a 10-second
// scrape, and of 56 samples at a 5-second scrape. Their peaks are in the
// many-short-series table of internal/promql/classic_bucket_window_bound.go.
var classicWindowShortSeries = []classicWindowShape{
	{series: 58_000, samples: 28, spacing: 10 * time.Second},
	{series: 58_000, samples: 30, spacing: 10 * time.Second},
	{series: 58_000, samples: 56, spacing: 5 * time.Second},
}

// classicWindowRefusal is how the handler answers a shape it cannot answer
// under the 1 GiB cap.
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

// chQueryMemoryLimitText opens the exception ClickHouse records when a query
// reaches its own per-query cap, as opposed to the server-wide limit.
const chQueryMemoryLimitText = "Query memory limit exceeded"

// classicWindowOutcome is how the handler answers one probe on one build.
// Its String is the subtest prefix.
type classicWindowOutcome int

const (
	// classicWindowAnswered: HTTP 200 with the quantile the seed implies.
	classicWindowAnswered classicWindowOutcome = iota
	// classicWindowFailsAtCap: classicWindowOutOfMemory's HTTP 422, on
	// every run.
	classicWindowFailsAtCap
	// classicWindowRefusedPerSeries: classicWindowSeriesBudget's HTTP 422.
	classicWindowRefusedPerSeries
	// classicWindowInBand: the shape answers on some runs and runs out of
	// memory on others, so either classicWindowAnswered or
	// classicWindowFailsAtCap, and never the per-series refusal.
	classicWindowInBand
)

func (o classicWindowOutcome) String() string {
	switch o {
	case classicWindowAnswered:
		return "answered"
	case classicWindowFailsAtCap:
		return "out-of-memory"
	case classicWindowRefusedPerSeries:
		return "refused"
	default:
		return "in-band"
	}
}

// classicWindowOnBoth is outcome on every build of classicMergeBuilds.
func classicWindowOnBoth(outcome classicWindowOutcome) map[string]classicWindowOutcome {
	return map[string]classicWindowOutcome{classicBuild268: outcome, classicBuild266: outcome}
}

// classicWindowProbe is one shape, the range function it is queried through
// and how the handler answers it on each build under the 1 GiB cap.
type classicWindowProbe struct {
	name  string
	shape classicWindowShape
	fn    string
	want  map[string]classicWindowOutcome
}

// classicWindowSeriesProbes are single series on either side of the
// per-series ceiling the 1 GiB cap derives, 3,821,145 entries (74,924
// samples of 51 entries), each spread over classicWindowSeriesParts data
// parts so the server reads it on several threads. Their outcomes are the
// per-series tables of
// internal/promql/classic_bucket_window_bound.go, and they are the same at
// two and at sixteen threads. Here and in classicMergeShapes a shape is
// pinned answered only where every measured run answered below 90% of the
// cap; one that failed on some run, or peaked above that, is pinned in the
// band, where answering and the cap's 422 both pass and the per-series
// refusal fails.
var classicWindowSeriesProbes = []classicWindowProbe{
	// 30,000 x 51 = 1,530,000 entries.
	{
		name: "series-30000", shape: classicWindowShape{series: 1, samples: 30_000, spacing: 8 * time.Millisecond, parts: classicWindowSeriesParts}, fn: "rate",
		want: classicWindowOnBoth(classicWindowAnswered),
	},
	// 45,000 x 51 = 2,295,000 entries: out of memory on 26.8, answered on 26.6
	// at up to 95% of the cap.
	{
		name: "series-45000", shape: classicWindowShape{series: 1, samples: 45_000, spacing: 4 * time.Millisecond, parts: classicWindowSeriesParts}, fn: "rate",
		want: map[string]classicWindowOutcome{classicBuild268: classicWindowFailsAtCap, classicBuild266: classicWindowInBand},
	},
	// 74,900 x 51 = 3,819,900 entries, just within the ceiling.
	{
		name: "series-74900", shape: classicWindowShape{series: 1, samples: 74_900, spacing: 3 * time.Millisecond, parts: classicWindowSeriesParts}, fn: "rate",
		want: classicWindowOnBoth(classicWindowFailsAtCap),
	},
	// 75,000 x 51 = 3,825,000 entries, the smallest series past it.
	{
		name: "series-75000", shape: classicWindowShape{series: 1, samples: 75_000, spacing: 3 * time.Millisecond, parts: classicWindowSeriesParts}, fn: "rate",
		want: classicWindowOnBoth(classicWindowRefusedPerSeries),
	},
}

// classicWindowManySeriesProbes are the shapes that run the window stage out
// of memory under the 1 GiB cap on at least one build when nothing refuses
// them (the tables of internal/promql/classic_bucket_window_bound.go). No
// series of theirs is past the per-series ceiling, so the cap is what
// refuses them.
var classicWindowManySeriesProbes = []classicWindowProbe{
	{
		name: "shared-rate", shape: classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second}, fn: "rate",
		want: classicWindowOnBoth(classicWindowFailsAtCap),
	},
	{
		name: "disjoint-rate", shape: classicWindowShape{series: 58_000, samples: 112, spacing: 2 * time.Second, disjoint: true}, fn: "rate",
		want: classicWindowOnBoth(classicWindowFailsAtCap),
	},
	{
		name: "short-series-rate", shape: classicWindowNearestRefusedShortSeries, fn: "rate",
		want: classicWindowOnBoth(classicWindowFailsAtCap),
	},
	// One sample per series: the spacing only names the metric.
	{
		name: "million-series-sum-over-time", shape: classicWindowShape{series: 1_000_000, samples: 1, spacing: time.Second, disjoint: true}, fn: "sum_over_time",
		want: classicWindowOnBoth(classicWindowFailsAtCap),
	},
	// 6,200 x 51 = 316,200 entries per series.
	{
		name: "long-series-rate", shape: classicWindowShape{series: 200, samples: 6_200, spacing: 40 * time.Millisecond}, fn: "rate",
		want: map[string]classicWindowOutcome{classicBuild268: classicWindowInBand, classicBuild266: classicWindowAnswered},
	},
	// 60,000 x 51 = 3,060,000 entries in one series.
	{
		name: "one-series-rate", shape: classicWindowShape{series: 1, samples: 60_000, spacing: 4 * time.Millisecond}, fn: "rate",
		want: map[string]classicWindowOutcome{classicBuild268: classicWindowFailsAtCap, classicBuild266: classicWindowInBand},
	},
}

// classicWindowNearestRefusedShortSeries is the smallest many-short-series
// shape measured to run out of memory on every run on both builds: 58,000
// series of 96 samples, 3.1 seconds apart so all 96 fall in the range. Of the
// smaller shapes measured, 58,000 x 72 answers on both builds and
// 58,000 x 84 answers on 26.6 (the many-short-series table of
// internal/promql/classic_bucket_window_bound.go); nothing between 84 and 96
// samples was measured.
var classicWindowNearestRefusedShortSeries = classicWindowShape{series: 58_000, samples: 96, spacing: 3100 * time.Millisecond}

// checkClassicWindowProbes fails t unless every probe's shape fits its range
// and is refused by the per-series check on a build exactly when one of its
// series is past the per-series ceiling.
func checkClassicWindowProbes(t *testing.T, probes []classicWindowProbe) {
	t.Helper()
	seriesCeiling := promql.ClassicBucketWindowSeriesEntriesForMemory(classicMergeQueryMemoryBytes)
	for _, p := range probes {
		if oldest := time.Duration(p.shape.samples-1) * p.shape.spacing; oldest >= classicWindowRange {
			t.Fatalf("%s: oldest sample %v before the evaluation instant; want every sample inside the %v range", p.name, oldest, classicWindowRange)
		}
		entries := int64(p.shape.samples * (classicWindowWidth + 1))
		for _, image := range classicMergeBuilds {
			want, ok := p.want[image]
			if !ok {
				t.Fatalf("%s: no outcome for %s", p.name, image)
			}
			if (want == classicWindowRefusedPerSeries) != (entries > seriesCeiling) {
				t.Fatalf("%s on %s: %v with %d entries per series against the per-series ceiling %d; want refused exactly past it",
					p.name, image, want, entries, seriesCeiling)
			}
		}
	}
}

// runClassicWindowProbe serves p through mux on s, built from image, and
// checks the outcome p expects there. A probe seeded over several parts is
// seeded and served with merges stopped, so its parts stay apart while it is
// read, and threads checks the threads its statement used once the stage
// read the whole series: every outcome but the per-series refusal.
func runClassicWindowProbe(ctx context.Context, t *testing.T, s *server, image string, mux http.Handler, p classicWindowProbe, threads classicWindowThreads) {
	t.Helper()
	want := p.want[image]
	t.Run(want.String()+"/"+p.name, func(t *testing.T) {
		if p.shape.parts > 1 {
			s.exec(ctx, t, "SYSTEM STOP MERGES otel_metrics_histogram")
			defer s.exec(ctx, t, "SYSTEM START MERGES otel_metrics_histogram")
		}
		metric := p.shape.seed(ctx, t, s)
		qid := fmt.Sprintf("classic-window-%s-%s-%d", s.version, p.name, time.Now().UnixNano())
		code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, p.fn, metric)
		assertClassicWindowOutcome(ctx, t, s, qid, want, code, body)
		if p.shape.parts > 1 && want != classicWindowRefusedPerSeries {
			threads.check(ctx, t, s, qid)
		}
	})
}

// classicWindowThreads is the range of threads a probe statement may use:
// more than above and at most atMost, zero meaning unbounded.
type classicWindowThreads struct{ above, atMost uint64 }

// classicTwoThreadPeakThreads is the most threads one single-series probe's
// statement used on a server of max_threads auto(2), on both builds, over the
// probes of classicWindowSeriesProbes; at max_threads 16 the same statements
// used 7.
const classicTwoThreadPeakThreads = 4

// classicWindowAtTwoThreads and classicWindowAtSixteenThreads are the thread
// ranges of the two servers the per-series probes run on.
var (
	classicWindowAtTwoThreads     = classicWindowThreads{atMost: classicTwoThreadPeakThreads}
	classicWindowAtSixteenThreads = classicWindowThreads{above: classicTwoThreadPeakThreads}
)

// check fails the test unless the statement qid used a thread count in r.
func (r classicWindowThreads) check(ctx context.Context, t *testing.T, s *server, qid string) {
	t.Helper()
	s.flushLogs(ctx, t)
	var peak uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(peak_threads_usage) FROM system.query_log WHERE type != 'QueryStart' AND query_id = ?", qid).Scan(&peak); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	t.Logf("%s: statement used %d threads", s.version, peak)
	if peak <= r.above || (r.atMost > 0 && peak > r.atMost) {
		t.Fatalf("%s: statement used %d threads; want more than %d and at most %d (0: unbounded)", s.version, peak, r.above, r.atMost)
	}
}

// assertClassicWindowOutcome checks the probe qid was answered as want
// describes.
func assertClassicWindowOutcome(ctx context.Context, t *testing.T, s *server, qid string, want classicWindowOutcome, code int, body string) {
	t.Helper()
	switch want {
	case classicWindowAnswered:
		assertClassicWindowAnswer(t, s, code, body)
	case classicWindowFailsAtCap:
		assertClassicWindowRefusal(ctx, t, s, qid, classicWindowOutOfMemory, code, body)
	case classicWindowRefusedPerSeries:
		assertClassicWindowRefusal(ctx, t, s, qid, classicWindowSeriesBudget, code, body)
		peak := s.exceptionPeakMemory(ctx, t, qid)
		t.Logf("%s: refused by the per-series check at peak memory_usage %d bytes", s.version, peak)
		if peak >= classicWindowRefusedPeakBytes {
			t.Fatalf("%s: refused at peak memory_usage %d bytes; want the per-series check to refuse before the fold, below %d bytes",
				s.version, peak, classicWindowRefusedPeakBytes)
		}
	case classicWindowInBand:
		if code == http.StatusOK {
			assertClassicWindowAnswer(t, s, code, body)
			return
		}
		assertClassicWindowRefusal(ctx, t, s, qid, classicWindowOutOfMemory, code, body)
	default:
		t.Fatalf("unknown outcome %d", want)
	}
}

// assertServerMaxThreads checks the max_threads s's default profile runs a
// statement on. 26.6 quotes an `auto(N)` value in system.settings and 26.8
// does not, so the quotes are not part of the comparison.
func assertServerMaxThreads(ctx context.Context, t *testing.T, s *server, want string) {
	t.Helper()
	var threads string
	if err := s.admin.Conn().QueryRow(ctx, "SELECT value FROM system.settings WHERE name = 'max_threads'").Scan(&threads); err != nil {
		t.Fatalf("%s: read max_threads: %v", s.version, err)
	}
	if threads = strings.Trim(threads, "'"); threads != want {
		t.Fatalf("%s: default max_threads %s; want %s", s.version, threads, want)
	}
}

// classicServerTwoThreads is the max_threads a server of serverNanoCPUs
// runs a statement on by default.
const classicServerTwoThreads = "auto(2)"

// TestClassicWindow_OutcomesAcrossBuilds serves the instant
// classic-histogram quantile through the production handler under the 1 GiB
// query memory cap and the default resource bounds, on each build, on a
// server running a statement on two threads. Many short series answer with
// the quantile their seed implies. Each probe of classicWindowSeriesProbes
// and classicWindowManySeriesProbes gets the outcome it pins for the build:
// the answer, HTTP 422 with the classic-histogram memory-limit message and
// ClickHouse's code 241 in query_log, or, for one series past the per-series
// ceiling alone, HTTP 422 with the per-series check's message below half the
// cap. Each single-series statement that reads its whole series uses at most
// classicTwoThreadPeakThreads threads.
func TestClassicWindow_OutcomesAcrossBuilds(t *testing.T) {
	probes := append(append([]classicWindowProbe{}, classicWindowSeriesProbes...), classicWindowManySeriesProbes...)
	for _, sh := range classicWindowShortSeries {
		probes = append(probes, classicWindowProbe{
			name: fmt.Sprintf("short-series-%d", sh.samples), shape: sh, fn: "rate",
			want: classicWindowOnBoth(classicWindowAnswered),
		})
	}
	checkClassicWindowProbes(t, probes)
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			assertServerMaxThreads(ctx, t, s, classicServerTwoThreads)
			mux := classicMergeMux(ctx, t, s, "auto")
			for _, p := range probes {
				runClassicWindowProbe(ctx, t, s, image, mux, p, classicWindowAtTwoThreads)
			}
		})
	}
}

// TestClassicWindow_SeriesCeilingAt16Threads serves
// classicWindowSeriesProbes as TestClassicWindow_OutcomesAcrossBuilds does,
// on a server whose default profile runs a statement on sixteen threads, and
// requires the outcome each probe pins at two threads while its statement
// uses more than classicTwoThreadPeakThreads threads: the series is read in
// parallel, yet folds on one thread, so the per-series ceiling does not
// depend on the thread count.
func TestClassicWindow_SeriesCeilingAt16Threads(t *testing.T) {
	checkClassicWindowProbes(t, classicWindowSeriesProbes)
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image, classicServerThreadsConfig())
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			assertServerMaxThreads(ctx, t, s, strconv.Itoa(classicServerMaxThreads))
			mux := classicMergeMux(ctx, t, s, "auto")
			for _, p := range classicWindowSeriesProbes {
				runClassicWindowProbe(ctx, t, s, image, mux, p, classicWindowAtSixteenThreads)
			}
		})
	}
}

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
