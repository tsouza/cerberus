//go:build integration

package regexjit

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// benchBuilds are the builds BenchmarkRegexJIT measures.
var benchBuilds = []string{
	"clickhouse/clickhouse-server:26.7.13.12-alpine",
	"clickhouse/clickhouse-server:26.8.10.6-alpine",
}

// Benchmark data volume: one hour of metrics and logs ending at benchEnd.
const (
	benchPods          = 20_000
	benchScrapeStep    = 10 * time.Second
	benchLogLines      = 10_000_000
	benchSpan          = time.Hour
	benchQueryStep     = time.Minute
	benchMetric        = "rjit_bench"
	benchService       = "rjitbench"
	benchParallelism   = 4 // concurrent clients per GOMAXPROCS unit
	benchSeedTimeout   = 20 * time.Minute
	benchPodGroups     = 4
	benchLatencyBucket = 997
)

var benchEnd = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

// benchQuery is one emitted shape the benchmark measures. churn renders a
// variant of the query per request whose pattern text differs but whose
// answer on the seeded data does not, so each request meets a pattern the
// server has not compiled yet.
type benchQuery struct {
	name  string
	logql bool
	query string
	churn func(i int) string
}

// churnDigits is the smallest bound churned {1,n} digit runs start from: it
// covers every digit run the seed writes, so each variant selects the same
// rows.
const churnDigits = 6

var benchQueries = []benchQuery{
	{
		name:  "promql-matcher-compiled",
		query: `sum(rate(` + benchMetric + `{pod=~"api-.*"}[5m]))`,
		churn: func(i int) string {
			return fmt.Sprintf(`sum(rate(%s{pod=~"api-[0-9]{1,%d}-.*"}[5m]))`, benchMetric, churnDigits+i)
		},
	},
	{
		name:  "promql-matcher-alternation-re2",
		query: `sum(rate(` + benchMetric + `{pod=~"api-.*|web-.*"}[5m]))`,
	},
	{
		name:  "promql-label_replace",
		query: `sum by (svc) (label_replace(rate(` + benchMetric + `[5m]), "svc", "$1", "pod", "(.*)-[0-9]+-.*"))`,
		churn: func(i int) string {
			return fmt.Sprintf(`sum by (svc) (label_replace(rate(%s[5m]), "svc", "$1", "pod", "(.*)-[0-9]{1,%d}-.*"))`, benchMetric, churnDigits+i)
		},
	},
	{
		name:  "logql-line-filter-compiled",
		logql: true,
		query: `sum(count_over_time({service_name="` + benchService + `"} |~ "timeout after [0-9]+ms" [5m]))`,
		churn: func(i int) string {
			return fmt.Sprintf(`sum(count_over_time({service_name=%q} |~ "timeout after [0-9]{1,%d}ms" [5m]))`, benchService, churnDigits+i)
		},
	},
	{
		name:  "logql-line-filter-dot",
		logql: true,
		query: `sum(count_over_time({service_name="` + benchService + `"} |~ "user=.*admin" [5m]))`,
	},
	{
		name:  "logql-line-filter-alternation-re2",
		logql: true,
		query: `sum(count_over_time({service_name="` + benchService + `"} |~ "timeout|refused" [5m]))`,
	},
	{
		name:  "logql-regexp-parser",
		logql: true,
		query: `sum by (code) (count_over_time({service_name="` + benchService + `"} | regexp "status=(?P<code>[0-9]+)" [5m]))`,
	},
	{
		name:  "logql-unwrap-duration",
		logql: true,
		query: `avg(avg_over_time({service_name="` + benchService + `"} | logfmt | unwrap duration(latency) | __error__="" [5m]))`,
	},
}

// benchScenario is a request lifecycle.
type benchScenario string

const (
	// scenarioWarm repeats one query, as a refreshing dashboard does.
	scenarioWarm benchScenario = "warm"
	// scenarioCold empties the compiled-code cache before every request.
	scenarioCold benchScenario = "cold"
	// scenarioChurn sends a pattern the server has not seen every request.
	scenarioChurn benchScenario = "churn"
	// scenarioConcurrent runs the warm query from concurrent clients.
	scenarioConcurrent benchScenario = "concurrent"
)

// BenchmarkRegexJIT measures cerberus-emitted regular-expression shapes on
// each benchmark build with compilation off and at the server's defaults,
// per scenario. Besides wall time per request it reports, from
// system.query_log, the server's elapsed and CPU milliseconds per request.
//
//	go test -tags=integration -run '^$' -bench RegexJIT -benchtime 10x ./test/regexjit/
func BenchmarkRegexJIT(b *testing.B) {
	for _, image := range benchBuilds {
		b.Run(image, func(b *testing.B) {
			ctx := context.Background()
			s := startServer(ctx, b, image)
			if !s.regexpJIT {
				b.Fatalf("%s has no %s", s.version, settingCompileRegexp)
			}
			seedBench(ctx, b, s)
			h := newHandlers(s.client(b))
			b.Logf("%s: server %s, defaults %s=1 %s=%d, container %d CPUs / %d GiB",
				image, s.version, settingCompileRegexp, settingMinCountRegexp, s.defaultMinCount(ctx, b),
				serverNanoCPUs/1_000_000_000, serverMemoryBytes>>30)
			for _, q := range benchQueries {
				for _, sc := range []benchScenario{scenarioWarm, scenarioCold, scenarioChurn, scenarioConcurrent} {
					if sc == scenarioChurn && q.churn == nil {
						continue
					}
					for _, mode := range []jitMode{jitOff, jitDefault} {
						b.Run(fmt.Sprintf("%s/%s/jit=%s", q.name, sc, mode), func(b *testing.B) {
							runBench(ctx, b, s, h, q, sc, mode)
						})
					}
				}
			}
		})
	}
}

var benchTagSeq atomic.Int64

func runBench(ctx context.Context, b *testing.B, s *server, h handlers, q benchQuery, sc benchScenario, mode jitMode) {
	tag := fmt.Sprintf("regexjit-bench-%d", benchTagSeq.Add(1))
	reqCtx := chclient.WithQuerySetting(s.modeCtx(ctx, mode), settingLogComment, tag)
	issue := func(query string) {
		start := benchEnd.Add(-benchSpan)
		if q.logql {
			h.lokiRange(reqCtx, b, query, start, benchEnd, benchQueryStep, probeLineLimit)
			return
		}
		h.promRange(reqCtx, b, query, start, benchEnd, benchQueryStep)
	}
	// Warm and concurrent runs start from a server that has seen the query:
	// one untimed request, untagged, compiles what the defaults compile.
	if sc == scenarioWarm || sc == scenarioConcurrent {
		s.dropCompiled(ctx, b)
		issue(q.query)
		reqCtx = chclient.WithQuerySetting(s.modeCtx(ctx, mode), settingLogComment, tag)
	}
	b.ResetTimer()
	switch sc {
	case scenarioConcurrent:
		b.SetParallelism(benchParallelism)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				issue(q.query)
			}
		})
	default:
		for i := 0; i < b.N; i++ {
			query := q.query
			switch sc {
			case scenarioCold:
				b.StopTimer()
				s.dropCompiled(ctx, b)
				b.StartTimer()
			case scenarioChurn:
				query = q.churn(int(benchTagSeq.Add(1)))
			}
			issue(query)
		}
	}
	b.StopTimer()
	elapsed, cpu := s.serverCost(ctx, b, tag)
	b.ReportMetric(elapsed/float64(b.N), "server-ms/op")
	b.ReportMetric(cpu/float64(b.N), "server-cpu-ms/op")
}

// serverCost sums the elapsed and CPU (user + system) milliseconds of the
// finished queries tagged tag.
func (s *server) serverCost(ctx context.Context, t testing.TB, tag string) (elapsedMs, cpuMs float64) {
	t.Helper()
	s.exec(ctx, t, "SYSTEM FLUSH LOGS")
	var elapsed, cpuMicros uint64
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT sum(query_duration_ms), sum(ProfileEvents['UserTimeMicroseconds'] + ProfileEvents['SystemTimeMicroseconds']) "+
			"FROM system.query_log WHERE type = 'QueryFinish' AND log_comment = ?", tag).Scan(&elapsed, &cpuMicros)
	if err != nil {
		t.Fatalf("read query_log for %s: %v", tag, err)
	}
	return float64(elapsed), float64(cpuMicros) / float64(time.Millisecond/time.Microsecond)
}

// seedBench writes an hour of gauge samples for benchPods pods and
// benchLogLines log lines. Pod names are `<group>-<n>-<hex>`, so
// label_replace's `(.*)-[0-9]+-.*` backtracks over each; log lines are
// logfmt, one in eleven a timeout, one in seven an admin user.
func seedBench(ctx context.Context, t testing.TB, s *server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, benchSeedTimeout)
	defer cancel()
	start := benchEnd.Add(-benchSpan)
	samplesPerPod := int(benchSpan / benchScrapeStep)
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT 'bench', '%s',
    map('pod', concat(arrayElement(['api', 'web', 'worker', 'db'], (number %% %d) %% %d + 1), '-',
        toString(number %% %d), '-', lower(hex(cityHash64(number %% %d) %% 65536)))),
    toDateTime64(%d, 9) + toIntervalSecond(%d * intDiv(number, %d) + 1),
    toFloat64(number)
FROM numbers(%d)`,
		benchMetric, benchPods, benchPodGroups, benchPods, benchPods,
		start.Unix(), int(benchScrapeStep/time.Second), benchPods, benchPods*samplesPerPod))

	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_logs (Timestamp, ServiceName, Body, ResourceAttributes)
SELECT
    toDateTime64(%d, 9) + toIntervalMillisecond(1 + number %% %d),
    '%s',
    concat('level=', if(number %% 11 = 0, 'error', 'info'),
           ' status=', toString(200 + (number %% 5) * 100 + number %% 3),
           ' user=', if(number %% 7 = 0, 'admin', concat('u', toString(number %% 1000))),
           ' latency=', toString(number %% %d), 'ms',
           ' msg="', if(number %% 11 = 0, concat('timeout after ', toString(number %% %d), 'ms'), 'request served'), '"'),
    map('service.name', '%s')
FROM numbers(%d)`,
		start.Unix(), benchSpan.Milliseconds()-1, benchService, benchLatencyBucket, benchLatencyBucket, benchService, benchLogLines))
	t.Logf("%s: seeded %s gauge samples over %s pods and %s log lines",
		s.version, strconv.Itoa(benchPods*samplesPerPod), strconv.Itoa(benchPods), strconv.Itoa(benchLogLines))
}
