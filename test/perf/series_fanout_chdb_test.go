//go:build chdb

// Perf guard (task #70 diagnosis → task #71 fan-in): the Drilldown-Metrics
// /api/v1/series +600ms latency was a fan-in problem — handleSeries ran a
// triple-nested fan-out, one sequential ClickHouse round-trip per matcher
// variant:
//
//	for each match[] selector m:
//	  for each nameVariant in expandUnderscoredMetricNameMatcher(m):   // V
//	    for each variant in expandBareHistogramMatcher(nameVariant):   // H
//	      fetchSeries(variant)  ->  executeInstant  ->  Engine.Query   // 1 CH round-trip
//
// So a single histogram-shaped match[] request issued N×V×H *sequential*
// round-trips, each a full parse→lower→optimize→emit→execute instant query
// — the demo's +600ms was fan-in (many fast round-trips serialised), not
// one slow query.
//
// Task #71 fixed it: the V×H×matcher variant set now collapses into ONE
// combined UNION-ALL query (chunked to ⌈N/K⌉ only past the K=128 arm cap,
// with a rendered-size guard keeping every query under CH's max_query_size).
// This harness drives the real in-process Prom handler against a chDB
// session via a round-trip-counting Querier decorator and now asserts the
// post-fan-in invariant: a typical histogram-shaped request issues EXACTLY
// ONE round-trip. It also runs the equivalent single combined query
// directly to size the wall-clock win the fan-in delivers.
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// countingQuerier wraps the chDB-backed Client and records every CH
// round-trip the handler issues — both the direct Client.QueryStrings /
// QueryLabelSets catalog paths and the Engine.Query / QueryCursor instant
// paths that /series fans out over. Each method also records its own wall
// time so the harness can report a per-trip breakdown.
type countingQuerier struct {
	// Embedded so the Querier methods the /series path does NOT exercise
	// (QueryMetricMeta / QueryExemplars) are forwarded unmodified; the four
	// methods below are overridden to count + time their round-trips.
	*chclienttest.Client

	inner *chclienttest.Client

	mu     sync.Mutex
	calls  int
	perOp  []time.Duration
	method []string
}

func (c *countingQuerier) record(method string, d time.Duration) {
	c.mu.Lock()
	c.calls++
	c.perOp = append(c.perOp, d)
	c.method = append(c.method, method)
	c.mu.Unlock()
}

func (c *countingQuerier) Query(ctx context.Context, q string, a ...any) ([]chclient.Sample, error) {
	t := time.Now()
	r, err := c.inner.Query(ctx, q, a...)
	c.record("Query", time.Since(t))
	return r, err
}

func (c *countingQuerier) QueryCursor(ctx context.Context, q string, a ...any) (chclient.Cursor, error) {
	t := time.Now()
	r, err := c.inner.QueryCursor(ctx, q, a...)
	c.record("QueryCursor", time.Since(t))
	return r, err
}

func (c *countingQuerier) QueryStrings(ctx context.Context, q string, a ...any) ([]string, error) {
	t := time.Now()
	r, err := c.inner.QueryStrings(ctx, q, a...)
	c.record("QueryStrings", time.Since(t))
	return r, err
}

func (c *countingQuerier) QueryLabelSets(ctx context.Context, q string, a ...any) ([]map[string]string, error) {
	t := time.Now()
	r, err := c.inner.QueryLabelSets(ctx, q, a...)
	c.record("QueryLabelSets", time.Since(t))
	return r, err
}

func (c *countingQuerier) reset() {
	c.mu.Lock()
	c.calls = 0
	c.perOp = nil
	c.method = nil
	c.mu.Unlock()
}

func (c *countingQuerier) snapshot() (int, []time.Duration, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := append([]time.Duration(nil), c.perOp...)
	m := append([]string(nil), c.method...)
	return c.calls, d, m
}

// seriesSeed builds the gauge / sum / histogram tables and a representative
// Drilldown corpus. The histogram base name `http_server_request_duration`
// triggers the H fan-out; the underscored form triggers the V fan-out
// (dotted candidate `http.server.request.duration`).
func seriesSeed() string {
	// Rows are anchored at now64(9) below (inside the staleness window the
	// /series bare-selector instant pipeline applies), so no fixed-timestamp
	// literal is needed here.
	gaugeDDL := `CREATE OR REPLACE TABLE otel_metrics_gauge (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
	sumDDL := `CREATE OR REPLACE TABLE otel_metrics_sum (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
	histDDL := `CREATE OR REPLACE TABLE otel_metrics_histogram (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9), Count UInt64, Sum Float64,
	  BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`

	// Populate each table with several services × attribute combos so the
	// instant queries do real granule work, not a single-row scan. now()
	// anchors the rows inside the bare-selector staleness window the
	// /series instant pipeline applies (Timestamp <= now, 5m lookback).
	gaugeIns := `INSERT INTO otel_metrics_gauge SELECT
	  concat('svc', toString(number % 8)),
	  arrayElement(['http_server_active_requests','system_cpu_utilization','process_runtime_memory'], (number % 3) + 1),
	  map('http.request.method', arrayElement(['GET','POST','PUT'], (number % 3)+1), 'host', concat('h', toString(number % 20))),
	  now64(9) - INTERVAL (number % 60) SECOND,
	  toFloat64(number)
	FROM numbers(20000);`
	sumIns := `INSERT INTO otel_metrics_sum SELECT
	  concat('svc', toString(number % 8)),
	  'http_server_request_duration_count',
	  map('http.request.method', arrayElement(['GET','POST','PUT'], (number % 3)+1), 'host', concat('h', toString(number % 20))),
	  now64(9) - INTERVAL (number % 60) SECOND,
	  toFloat64(number)
	FROM numbers(20000);`
	histIns := `INSERT INTO otel_metrics_histogram SELECT
	  concat('svc', toString(number % 8)),
	  'http_server_request_duration',
	  map('http.request.method', arrayElement(['GET','POST','PUT'], (number % 3)+1), 'host', concat('h', toString(number % 20))),
	  now64(9) - INTERVAL (number % 60) SECOND,
	  toUInt64(100 + number), toFloat64(number) / 2,
	  [10,20,30,40], [0.1,0.5,1.0]
	FROM numbers(20000);`

	return gaugeDDL + sumDDL + histDDL + gaugeIns + sumIns + histIns
}

func TestSeriesFanout_ChDB(t *testing.T) {
	inner := chclienttest.NewChDB(t)
	inner.Seed(t, seriesSeed())
	counter := &countingQuerier{Client: inner, inner: inner}

	// Real prom handler over the counting client (Client == Engine.Client,
	// so every round-trip — catalog + instant fan-out — flows through the
	// counter).
	h := prom.New(counter, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Representative Drilldown /series request: a histogram base name in
	// Prom underscore grammar. This fires BOTH fan-out layers — V (dotted
	// candidate) and H (the three classic-histogram companions).
	matcher := `{__name__="http_server_request_duration"}`
	reqURL := fmt.Sprintf("%s/api/v1/series?match[]=%s&start=%d&end=%d",
		srv.URL, url.QueryEscape(matcher),
		time.Now().Add(-1*time.Hour).Unix(), time.Now().Unix())

	counter.reset()
	start := time.Now()
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET /series: %v", err)
	}
	totalWall := time.Since(start)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	n, perOp, methods := counter.snapshot()
	var chSum time.Duration
	for _, d := range perOp {
		chSum += d
	}

	t.Logf("=== /api/v1/series fan-out diagnosis ===")
	t.Logf("request matcher: %s", matcher)
	t.Logf("CH round-trips issued (N): %d", n)
	t.Logf("per-trip CH wall times:")
	for i, d := range perOp {
		t.Logf("    trip %2d  %-12s  %v", i+1, methods[i], d.Round(time.Microsecond))
	}
	t.Logf("sum of CH round-trip wall:   %v", chSum.Round(time.Microsecond))
	t.Logf("end-to-end handler wall:     %v", totalWall.Round(time.Microsecond))
	t.Logf("mean per-trip:               %v", (chSum / time.Duration(max1(n))).Round(time.Microsecond))

	// --- Projected batched cost: one OR-joined combined query ---------
	//
	// Model the task #71 refactor: instead of N sequential instant queries,
	// issue ONE query per table that ORs every variant's MetricName
	// predicate together and projects distinct label sets. This is the
	// shape internal/api/loki/series.go:buildSeriesSQL already uses for the
	// Loki head. We run the equivalent combined SQL directly to size the
	// floor cost of the batched path.
	//
	// The variant set for this request resolves to:
	//   gauge/sum  candidates: http_server_request_duration{,_count,_sum,_bucket}
	//   histogram  base:       http_server_request_duration
	// A single UNION-ALL over the three tables, each filtering MetricName
	// IN (...the request's variant names...), captures every row the
	// fan-out would have touched in one round-trip.
	db := openChDB(t)
	combined := `
SELECT DISTINCT MetricName, Attributes FROM (
  SELECT MetricName, Attributes FROM otel_metrics_gauge
    WHERE MetricName IN ('http_server_request_duration','http_server_request_duration_count','http_server_request_duration_sum','http_server_request_duration_bucket')
  UNION ALL
  SELECT MetricName, Attributes FROM otel_metrics_sum
    WHERE MetricName IN ('http_server_request_duration','http_server_request_duration_count','http_server_request_duration_sum','http_server_request_duration_bucket')
  UNION ALL
  SELECT MetricName, Attributes FROM otel_metrics_histogram
    WHERE MetricName = 'http_server_request_duration'
)`
	// chDB-go's parquet driver panics on large multi-column projections;
	// wrap in count() so the result is one row. The WHERE / scan / DISTINCT
	// work — what we're timing — is identical.
	combinedCount := "SELECT count() FROM (" + combined + ")"
	const iters = 7
	best := time.Hour
	for i := 0; i < iters; i++ {
		s := time.Now()
		var c int64
		if err := db.QueryRow(combinedCount).Scan(&c); err != nil {
			t.Fatalf("combined query: %v", err)
		}
		if d := time.Since(s); d < best {
			best = d
		}
	}

	t.Logf("--- projected batched (single combined query) ---")
	t.Logf("combined-query round-trips:  1  (was %d)", n)
	t.Logf("combined-query best wall:    %v", best.Round(time.Microsecond))
	if chSum > 0 {
		t.Logf("CH-time delta:               %v (fan-out)  ->  %v (combined)  =  %.1fx",
			chSum.Round(time.Microsecond), best.Round(time.Microsecond),
			float64(chSum)/float64(best))
	}

	// --- ASSERTION: fan-in shipped — typical request is ONE round-trip ---
	//
	// The /series fan-in batching (task #71) landed on main: the
	// triple-nested V×H×matcher fan-out now collapses into ONE combined
	// UNION-ALL query (chunked to ⌈N/K⌉ only past the K=128 arm cap). The
	// matcher above (`{__name__="http_server_request_duration"}`) fans out
	// to V (dotted-candidate) × H (classic-histogram companion) variants —
	// far below the cap — so the batched handler issues EXACTLY ONE CH
	// round-trip. This is the deterministic post-fan-in guard: a regression
	// that re-introduced the per-variant loop (or otherwise un-batched the
	// fan-out) would inflate n back above 1 and trip here.
	if n != 1 {
		t.Fatalf("/series fan-in regression: a typical histogram-shaped match[] request "+
			"issued %d ClickHouse round-trips, want exactly 1. The fan-in batching (task "+
			"#71) collapses the V×H variant fan-out into one combined UNION-ALL query for "+
			"any request below the K=128 arm cap; an n>1 here means the per-variant loop "+
			"regressed or the combined query was split when it should not have been.", n)
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}
