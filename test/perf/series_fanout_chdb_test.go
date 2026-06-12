//go:build chdb

// Deliverable 2 (task #70): diagnose the Drilldown-Metrics /api/v1/series
// +600ms latency.
//
// handleSeries (internal/api/prom/metadata.go:327-363) runs a triple-nested
// fan-out per request:
//
//	for each match[] selector m:
//	  for each nameVariant in expandUnderscoredMetricNameMatcher(m):   // V
//	    for each variant in expandBareHistogramMatcher(nameVariant):   // H
//	      fetchSeries(variant)  ->  executeInstant  ->  Engine.Query   // 1 CH round-trip
//
// So a single histogram-shaped match[] request issues N×V×H *sequential*
// ClickHouse round-trips, each a full parse→lower→optimize→emit→execute
// instant query. The hypothesis: the demo's +600ms is fan-in (many fast
// round-trips serialised) — not one slow query.
//
// This harness drives the real in-process Prom handler against a chDB
// session via a round-trip-counting Querier decorator, measures N + the
// per-trip / total wall time, then runs the equivalent single OR-joined
// combined query (modelled on internal/api/loki/series.go:buildSeriesSQL)
// to project the batched-into-one cost. It does NOT refactor the handler
// (that's task #71) — it only proves the win is real and sizes it.
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

	// --- ASSERTION: don't-regress-upward round-trip baseline -------------
	//
	// IMPORTANT SCOPE NOTE: the /series fan-in batching (#790) that would
	// collapse this triple-nested fan-out into a SINGLE combined query is
	// HELD — it is NOT on main. So this guard deliberately does NOT assert
	// `n == 1` (that lands as part of the #790 follow-up, at which point
	// flip the ceiling below to `if n != 1` and delete this note). What it
	// CAN do today is pin the CURRENT round-trip count as a regression
	// ceiling: the matcher above (`{__name__="http_server_request_duration"}`)
	// fans out over the V (dotted-candidate) × H (classic-histogram
	// companion) variant cross-product, and a bug that added a new
	// expansion layer — or made an existing one multiply instead of union —
	// would inflate N. The ceiling bites that explosion without pretending
	// the batching shipped.
	//
	// The ceiling is set generously above the observed count (32 at the
	// time of writing) so normal variant-set drift (a companion suffix
	// added/removed) doesn't flake it, while a multiplicative blow-up
	// (e.g. fanning the H layer over every gauge row, or a doubled V pass)
	// trips it. When #790 lands and N drops to 1, replace this block with
	// the exact `n == 1` assertion.
	const maxRoundTrips = 64
	if n > maxRoundTrips {
		t.Fatalf("/series fan-out round-trip regression: a single histogram-shaped "+
			"match[] request issued %d sequential ClickHouse round-trips, exceeding the "+
			"%d ceiling. The V×H variant expansion has blown up (a new expansion layer, "+
			"or an existing one multiplying instead of unioning). NOTE: the #790 fan-in "+
			"batching that collapses this to 1 round-trip is held off main; when it lands, "+
			"swap this ceiling for `n == 1`.", n, maxRoundTrips)
	}
	if n < 1 {
		t.Fatalf("/series request issued %d round-trips — the counting Querier saw no "+
			"CH traffic, so the harness isn't exercising the fan-out path it claims to "+
			"measure", n)
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
