//go:build integration

// handler_route_b_realch_integration_test.go — proves route B (the
// sharded-pushdown solver) returns the SAME answer as route A against a REAL
// ClickHouse server, through the REAL production emitter.
//
// # WHY THIS TEST EXISTS
//
// Every other route-B test in the repo either runs entirely on chDB
// (compatibility/prometheus's differential corpus, forced onto route B via
// CERBERUS_EVAL_ROUTE=sharded in compatibility.yml's
// compatibility/prometheus-forced-route job) or runs against a real
// ClickHouse with a FAKE emitter that returns the identical, hand-written SQL
// for every shard regardless of the input plan
// (internal/solver/executor_realch_integration_test.go's memProbeEmitter —
// that test proves the per-shard max_memory_usage SETTING is honoured by a
// real server, not that the per-shard SQL a real query LOWERS to is correct
// against one).
//
// That leaves a gap: chDB (libchdb) leniently coerces result-column types a
// real clickhouse-go/v2 Scan strict-scans and rejects (the same class of
// blind spot test/spec/strictscan_integration_test.go documents for route A).
// A route-B-specific emit-type bug — e.g. a per-shard time-bound predicate
// that projects a column shape only route B's slicing produces — would pass
// every existing gate: the chDB-only differential can't see the strict-scan
// divergence, and the real-CH memory-apportion test never asks the real
// emitter to lower a genuine query at all. Route B is also the OOM-relief
// path (docs/solver.md), so it is exactly the path a deployment leans on
// hardest when already under memory pressure — the worst time to discover an
// emit-type bug for the first time.
//
// # WHAT THIS TEST DOES
//
// Spins ONE real `clickhouse/clickhouse-server` via testcontainers (the same
// pattern strict-scan / router-corpus / solver-memory-apportion / chclient's
// real-CH lanes already use), seeds one small monotonic counter series across
// a plain past-anchored window, then answers the IDENTICAL
// `/api/v1/query_range` request through two *prom.Handler instances backed by
// the SAME ClickHouse — one with the solver disabled (Mode=single, route A
// only), one with it forced on (Mode=sharded). Both handlers run the REAL
// production PromQL lowering + engine.ChsqlEmitter, so route B's per-shard
// SQL is whatever cerberus actually emits for a genuinely sliced plan, not a
// fixture standing in for it.
//
// Asserts two things route A alone cannot: the X-Cerberus-Route-Decision
// response header proves route B genuinely fired with K>=2 shards (not a
// silent fall-back to route A, which would make every other assertion here
// vacuous), and the decoded matrix result is BYTE-IDENTICAL between the two
// handlers — same series, same timestamps, same values. Bit-for-bit equality
// is the right bar, not an epsilon: docs/solver.md's slicing geometry runs
// each shard's rate()/increase() window computation exactly once per anchor,
// on the identical compat-gated SQL route A itself runs, restricted to that
// shard's own anchor subset — there is no cross-shard partial-aggregation
// step whose floating-point summation order could legitimately differ.
//
// Gated behind the `integration` build tag (Docker required); joins the
// established real-CH lane family (test/spec/strictscan_integration_test.go,
// internal/routerrules/realch_integration_test.go,
// internal/solver/executor_realch_integration_test.go,
// internal/chclient/conn_teardown_integration_test.go) wired as an extra step
// in .github/workflows/strict-scan.yml — run locally with:
//
//	go test -tags=integration -count=1 -run TestRouteB_MatchesRouteA_RealClickHouse ./internal/api/prom/...
package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// routeBCHImage matches the other real-CH integration lanes (strict-scan,
// router-rules, solver-memory-apportion, chclient) so every real-CH lane
// exercises the same server version.
const routeBCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// routeBMetric is the counter series both handlers query. Named away from
// any reserved suffix (_bucket / _count / _sum / _exp_hist) so it is read as
// an ordinary Sum metric, never mistaken for a histogram companion.
const routeBMetric = "route_b_diff_counter"

// routeBStepSeconds / routeBSampleCount together fix the seeded window: a
// 15s scrape interval spanning 20 minutes gives 80 anchor points at the
// query's own step, comfortably enough for the solver to find a K>=2 slicing
// under ModeSharded's dropped cost-threshold floor (docs/solver.md
// "Eligibility signals" — ModeSharded only requires the plan be structurally
// sliceable, not that it clear MinFanout/MinAnchorPairs).
const (
	routeBStepSeconds  = 15
	routeBSampleCount  = 80
	routeBCounterDelta = 2.0 // per-scrape increment; deterministic, non-zero rate()
)

// routeBSumDDL is the otel_metrics_sum table shape the real OTel-CH exporter
// creates (LowCardinality-keyed attribute maps, AggregationTemporality +
// IsMonotonic present) — the same table definition
// handler_histogram_integration_test.go's realExporterHistogramDDL uses,
// extracted to just the one table this test needs.
const routeBSumDDL = `CREATE TABLE otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`

// matrixSample is the decoded shape of one /api/v1/query_range matrix
// element.
type matrixSample struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

// decodeRangeQuery issues a query_range request and decodes the matrix
// result, also returning the raw response headers so the caller can inspect
// X-Cerberus-Route-Decision.
func decodeRangeQuery(t *testing.T, base, query string, start, end, step int64) ([]matrixSample, http.Header) {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		base, url.QueryEscape(query), start, end, step)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query_range %q status=%d body=%s", query, resp.StatusCode, body)
	}
	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string         `json:"resultType"`
			Result     []matrixSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v\nbody=%s", query, err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("query_range %q status=%q err=%s", query, parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("query_range %q resultType=%q, want matrix", query, parsed.Data.ResultType)
	}
	return parsed.Data.Result, resp.Header
}

// routeBDecisionK parses the "k=<N>" field out of an X-Cerberus-Route-Decision
// header value (see engine.routeDecisionValue's documented grammar:
// "<strategy>;k=<K>;reason=<reason>" when routed). Fails the test if the
// header is absent, doesn't start with the sharded-timeslice strategy, or has
// no parseable k= field — any of which means route B did not genuinely fire,
// which would make the equality assertion below vacuous.
func routeBDecisionK(t *testing.T, header http.Header) int {
	t.Helper()
	v := header.Get(engine.HeaderRouteDecision)
	if v == "" {
		t.Fatalf("response carries no %s header — the solver did not classify this request at all", engine.HeaderRouteDecision)
	}
	if !strings.HasPrefix(v, solver.StrategyShardedTimeslice+";") {
		t.Fatalf("%s = %q, want it to start with %q — route B did not fire (a silent fall-back to route A "+
			"would make the byte-identical-result assertion vacuous: of course route A matches itself)",
			engine.HeaderRouteDecision, v, solver.StrategyShardedTimeslice)
	}
	for _, field := range strings.Split(v, ";") {
		if k, ok := strings.CutPrefix(field, "k="); ok {
			n, err := strconv.Atoi(k)
			if err != nil {
				t.Fatalf("%s = %q: k field %q did not parse as an int: %v", engine.HeaderRouteDecision, v, k, err)
			}
			return n
		}
	}
	t.Fatalf("%s = %q has no k= field", engine.HeaderRouteDecision, v)
	return 0
}

// newRouteBHandler wires a *prom.Handler against client with the given
// solver Mode, sharing the SAME real ClickHouse connection (and therefore the
// same seeded data) route A and route B are compared over. mode="single"
// disables the solver (route A only, but still computes the shadow
// X-Cerberus-Route-Decision header — see solver.ModeSingle's doc comment);
// mode="sharded" forces route B for any structurally eligible plan.
func newRouteBHandler(t *testing.T, client *chclient.Client, mode string) *prom.Handler {
	t.Helper()
	h := prom.New(client, schema.DefaultOTelMetrics(), nil)

	cfg := solver.DefaultConfig()
	cfg.Mode = mode
	cfg.Parallel = 4
	cfg.Timeout = 30 * time.Second
	cfg.MaxOutputRows = 1_000_000 // far above this fixture's single-series output
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid (mode=%s): %v", mode, err)
	}
	h.Engine.Solver = solver.New(cfg, engine.ChsqlEmitter{}, solver.ExecDeps{Client: client})
	return h
}

func TestRouteB_MatchesRouteA_RealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	container, err := tcclickhouse.Run(
		ctx,
		routeBCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Exec(ctx, routeBSumDDL); err != nil {
		t.Fatalf("create otel_metrics_sum: %v", err)
	}

	// Past-anchored window (never `now()`-relative) so both handlers see an
	// identical, jitter-free query regardless of wall-clock skew between the
	// two HTTP round trips.
	windowStart := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)

	var sb strings.Builder
	sb.WriteString(`INSERT INTO otel_metrics_sum
    (ResourceAttributes, ServiceName, MetricName, Attributes, StartTimeUnix, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES `)
	for i := range routeBSampleCount {
		ts := windowStart.Add(time.Duration(i*routeBStepSeconds) * time.Second)
		tsStr := ts.Format("2006-01-02 15:04:05.000")
		value := float64(i+1) * routeBCounterDelta
		if i > 0 {
			sb.WriteString(",\n")
		}
		fmt.Fprintf(&sb,
			`(map('service.name','api'), 'api', '%s', map('job','api'), toDateTime64('%s', 9), toDateTime64('%s', 9), %g, 2, true)`,
			routeBMetric, tsStr, tsStr, value)
	}
	if err := client.Exec(ctx, sb.String()); err != nil {
		t.Fatalf("seed %s: %v", routeBMetric, err)
	}

	hA := newRouteBHandler(t, client, solver.ModeSingle)
	hB := newRouteBHandler(t, client, solver.ModeSharded)

	muxA := http.NewServeMux()
	hA.Mount(muxA)
	srvA := httptest.NewServer(muxA)
	t.Cleanup(srvA.Close)

	muxB := http.NewServeMux()
	hB.Mount(muxB)
	srvB := httptest.NewServer(muxB)
	t.Cleanup(srvB.Close)

	query := fmt.Sprintf(`rate(%s[1m])`, routeBMetric)
	start := windowStart.Unix()
	end := windowStart.Add(time.Duration(routeBSampleCount*routeBStepSeconds) * time.Second).Unix()

	resultA, headerA := decodeRangeQuery(t, srvA.URL, query, start, end, routeBStepSeconds)
	resultB, headerB := decodeRangeQuery(t, srvB.URL, query, start, end, routeBStepSeconds)

	// Route A's own shadow classification must NOT report routed (Mode=single
	// always returns routed=false — see solver.ModeSingle's doc comment); a
	// route-a shadow-only value here is expected and fine, but any
	// sharded-timeslice value would mean the two handlers weren't actually
	// configured differently.
	if v := headerA.Get(engine.HeaderRouteDecision); strings.HasPrefix(v, solver.StrategyShardedTimeslice) {
		t.Fatalf("route-A handler (Mode=single) reports %s=%q — it should never route, "+
			"this test's two handlers are not actually configured differently", engine.HeaderRouteDecision, v)
	}

	k := routeBDecisionK(t, headerB)
	if k < 2 {
		t.Fatalf("route B fired with K=%d shards, want >= 2 — a K=1 \"sharded\" run is degenerate "+
			"(docs/solver.md: \"one shard is route A\") and would not exercise cross-shard behaviour at all", k)
	}
	t.Logf("route B fired with K=%d shards", k)

	if len(resultA) == 0 {
		t.Fatalf("route A returned zero series for %q — the fixture query itself is broken", query)
	}
	if len(resultA) != len(resultB) {
		t.Fatalf("series count differs: route A=%d, route B=%d", len(resultA), len(resultB))
	}

	byLabels := func(rows []matrixSample) map[string]matrixSample {
		out := make(map[string]matrixSample, len(rows))
		for _, r := range rows {
			b, err := json.Marshal(r.Metric)
			if err != nil {
				t.Fatalf("marshal metric labels: %v", err)
			}
			out[string(b)] = r
		}
		return out
	}
	setA, setB := byLabels(resultA), byLabels(resultB)

	for labels, a := range setA {
		b, ok := setB[labels]
		if !ok {
			t.Fatalf("series %s present in route A, absent from route B", labels)
		}
		if len(a.Values) == 0 {
			t.Fatalf("series %s: route A returned zero (timestamp, value) points — the fixture's rate() window is misconfigured", labels)
		}
		if len(a.Values) != len(b.Values) {
			t.Fatalf("series %s: point count differs: route A=%d, route B=%d\nA=%v\nB=%v",
				labels, len(a.Values), len(b.Values), a.Values, b.Values)
		}
		for i := range a.Values {
			if a.Values[i] != b.Values[i] {
				t.Fatalf("series %s point %d: route A=%v, route B=%v — route B's real, sliced SQL "+
					"disagrees with route A's over a real ClickHouse server",
					labels, i, a.Values[i], b.Values[i])
			}
		}
	}
	for labels := range setB {
		if _, ok := setA[labels]; !ok {
			t.Fatalf("series %s present in route B, absent from route A", labels)
		}
	}
}
