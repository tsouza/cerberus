//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// cancellationBuilds are the pinned builds the cancellation probes run
// against, with whether each emitted CPU-bound function is interrupted
// mid-call there. The expectations are observations of the builds; the test
// separately requires chopt.CancellationGaps to agree with them.
//
//   - 26.6.1.1193: neither arrayFold nor the replace family checks for
//     cancellation inside a call.
//   - 26.7.13.12: arrayFold does (ClickHouse#108192); the replace family does
//     not yet.
//   - 26.8.10.6: both do (ClickHouse#112483).
var cancellationBuilds = []struct {
	image        string
	foldBounded  bool
	regexBounded bool
}{
	{"clickhouse/clickhouse-server:26.6.1.1193-alpine", false, false},
	{"clickhouse/clickhouse-server:26.7.13.12-alpine", true, false},
	{"clickhouse/clickhouse-server:26.8.10.6-alpine", true, true},
}

// cancelShape is one cerberus-emitted query whose evaluation spends seconds
// inside a single call of function.
type cancelShape struct {
	name     string
	function string
	query    string
	bounded  func(foldBounded, regexBounded bool) bool
}

var cancelShapes = []cancelShape{
	{
		// double_exponential_smoothing lowers to an arrayFold over the
		// window's samples; one series carrying foldSamples samples makes
		// that single call the bulk of the query.
		name:     "array_fold",
		function: "arrayFold",
		query:    "double_exponential_smoothing(" + foldMetric + "[10m], 0.5, 0.5)",
		bounded:  func(fold, _ bool) bool { return fold },
	},
	{
		// Every PromQL selector normalizes label names with
		// replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'); a label name of
		// regexKeyChunks * regexChunkChars characters that all need
		// replacing makes that single call the bulk of the query.
		name:     "regex_replace",
		function: "replaceRegexpAll",
		query:    regexMetric,
		bounded:  func(_, regex bool) bool { return regex },
	},
}

// Seed and budget constants. The seeds are sized so the CPU-bound call runs
// for several seconds on the CI substrate — far past cancelDeadline plus
// cancelBound, so an uninterrupted call is unambiguous — yet stays within the
// container's memory and finishes well inside naturalRunBudget.
const (
	foldMetric       = "foldprobe"
	foldSamples      = 400_000
	regexMetric      = "regexprobe"
	regexKeyChunks   = 100
	regexChunkChars  = 1_000_000 // repeat()'s own per-call cap
	cancelDeadline   = time.Second
	cancelBound      = 2 * time.Second
	handlerSlack     = 3 * time.Second
	naturalRunBudget = 90 * time.Second
	runningBudget    = 30 * time.Second
	closeBudget      = 30 * time.Second
	pollInterval     = 50 * time.Millisecond
	shardedDB        = "sharded"
	shardCluster     = "chserver"
	shardCount       = 2
	siblingCount     = 2
	shardedPoolConns = shardCount * siblingCount * 2
)

// cancelEvalTime is the instant every probe evaluates at.
var cancelEvalTime = time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)

// TestCancellation_CPUBoundEmittedShapesAcrossBuilds extends the real-server
// cancellation proof from cooperative sleeps to the CPU-bound functions
// cerberus emits. For every pinned build and each shape it drives:
//
//   - a request deadline through the production Prometheus handler: the client
//     is answered 503 errorType=timeout on time on every build, and the
//     server either stops within cancelBound or — on a build that cannot
//     interrupt the call — is observed still running after the handler
//     returned and its admission slot was free again;
//   - a client disconnect mid-call through the same handler: 503
//     errorType=canceled, with the same bounded/unbounded split;
//   - routed sibling cancellation: two statements dispatched through a
//     data-shard client over Distributed tables, cancelled together, closed
//     through the fan-out gate's KILL QUERY ... SYNC release. On a build that
//     interrupts the call no statement — initiator or remote child — is left
//     running when the gate capacity is released; on one that cannot, the
//     capacity is released while the work still runs.
//
// Every scenario asserts the server eventually cleans the work up, and that
// the admission slot is free the moment the client is answered while the
// connection pool and the fan-out gate are usable again afterwards. Which branch a build takes is fixed by the table above and
// must agree with chopt.CancellationGaps, the version policy cerberus reports
// at boot.
func TestCancellation_CPUBoundEmittedShapesAcrossBuilds(t *testing.T) {
	clusterConfig, err := filepath.Abs(filepath.Join("testdata", "cluster.xml"))
	if err != nil {
		t.Fatalf("cluster config path: %v", err)
	}
	for _, build := range cancellationBuilds {
		t.Run(build.image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			s := startServer(ctx, t, build.image, tcclickhouse.WithConfigFile(clusterConfig))
			seedCancellationProbe(ctx, t, s)

			set := chopttest.ResolveEnabledSet(ctx, t, s.admin, chopt.SelectionAuto)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())

			for _, shape := range cancelShapes {
				t.Run(shape.name, func(t *testing.T) {
					bounded := shape.bounded(build.foldBounded, build.regexBounded)
					assertCancellationPolicy(t, s.version, shape.function, bounded)

					t.Run("request_deadline", func(t *testing.T) {
						probeRequestDeadline(ctx, t, s, rules, shape, bounded)
					})
					t.Run("client_disconnect", func(t *testing.T) {
						probeClientDisconnect(ctx, t, s, rules, shape, bounded)
					})
					t.Run("routed_siblings", func(t *testing.T) {
						probeRoutedSiblings(ctx, t, s, shape, bounded)
					})
				})
			}
		})
	}
}

// assertCancellationPolicy requires chopt.CancellationGaps to report a gap for
// function on version exactly when the build was observed not to interrupt it.
func assertCancellationPolicy(t *testing.T, version chopt.Version, function string, bounded bool) {
	t.Helper()
	gap := false
	for _, g := range chopt.CancellationGaps(version) {
		gap = gap || slices.Contains(g.Functions, function)
	}
	if gap == bounded {
		t.Errorf("chopt reports a %s cancellation gap on %s = %v, but the build interrupts it = %v", function, version, gap, bounded)
	}
}

// promProbe is a production Prometheus handler over its own client, behind a
// one-slot admission limiter so a leaked slot shows as a refused acquisition.
type promProbe struct {
	mux     http.Handler
	limiter *admit.Limiter
	client  *chclient.Client
}

func newPromProbe(t *testing.T, s *server, rules engine.SettingsRules, timeout time.Duration) promProbe {
	t.Helper()
	client := s.client(t, adminUser, adminPassword, chclient.Config{QueryTimeout: timeout})
	h := prom.New(client, schema.DefaultOTelMetrics(), nil)
	h.QueryTimeout = timeout
	h.Limiter = admit.New("prom", 1)
	h.Engine.SetSettings(rules)
	mux := http.NewServeMux()
	h.Mount(mux)
	return promProbe{mux: mux, limiter: h.Limiter, client: client}
}

// serve issues the instant query for shape under ctx and returns the recorder.
func (p promProbe) serve(ctx context.Context, shape cancelShape) *httptest.ResponseRecorder {
	params := url.Values{}
	params.Set("query", shape.query)
	params.Set("time", strconv.FormatInt(cancelEvalTime.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	p.mux.ServeHTTP(rec, req)
	return rec
}

// assertAdmissionFree requires the handler's admission slot to be free again.
func (p promProbe) assertAdmissionFree(t *testing.T) {
	t.Helper()
	actx, cancel := context.WithTimeout(context.Background(), pollInterval)
	defer cancel()
	release, ok := p.limiter.Acquire(actx)
	if !ok {
		t.Error("the admission slot is still held after the handler returned")
		return
	}
	release()
}

// assertPoolReleased requires every pooled connection to be back in the pool.
func (p promProbe) assertPoolReleased(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(cancelBound)
	for {
		// clickhouse-go reports Open as the connections currently checked
		// out of the pool; Idle ones are not counted there.
		st := p.client.Conn().Stats()
		if st.Open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("connection pool: %d connection(s) still checked out after the handler returned", st.Open)
			return
		}
		time.Sleep(pollInterval)
	}
}

// assertErrorType requires rec to be the Prometheus 503 error envelope with
// errorType want.
func assertErrorType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Status != "error" || body.ErrorType != want {
		t.Errorf("response = HTTP %d status=%q errorType=%q; want HTTP %d status=\"error\" errorType=%q",
			rec.Code, body.Status, body.ErrorType, http.StatusServiceUnavailable, want)
	}
}

func probeRequestDeadline(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, bounded bool) {
	p := newPromProbe(t, s, rules, cancelDeadline)
	qid := cancelQueryID(shape, "deadline")
	start := time.Now()
	rec := p.serve(chclient.WithQueryID(ctx, qid), shape)
	answered := time.Since(start)
	assertErrorType(t, rec, "timeout")
	// The answer waits for cerberus's KILL QUERY ... SYNC, which confirms
	// promptly where the call is interruptible and gives up after
	// chclient.KillDataShardQueryTimeout where it is not.
	budget := cancelDeadline + handlerSlack
	if !bounded {
		budget += chclient.KillDataShardQueryTimeout
	}
	if answered > budget {
		t.Errorf("the client was answered after %s; want within %s", answered, budget)
	}
	p.assertAdmissionFree(t)
	s.assertServerWorkEnds(ctx, t, []string{qid}, bounded)
	p.assertPoolReleased(t)
	duration := s.finishedDuration(ctx, t, qid)
	if stopped := duration <= cancelDeadline+cancelBound; stopped != bounded {
		t.Errorf("server-side duration %s under a %s deadline: stopped within %s = %v; want %v",
			duration, cancelDeadline, cancelBound, stopped, bounded)
	}
}

func probeClientDisconnect(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, bounded bool) {
	p := newPromProbe(t, s, rules, 0)
	qid := cancelQueryID(shape, "disconnect")
	reqCtx, disconnect := context.WithCancel(chclient.WithQueryID(ctx, qid))
	defer disconnect()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- p.serve(reqCtx, shape) }()

	s.waitRunningFor(ctx, t, qid, cancelDeadline)
	disconnect()
	budget := handlerSlack
	if !bounded {
		budget += chclient.KillDataShardQueryTimeout
	}
	select {
	case rec := <-done:
		assertErrorType(t, rec, "canceled")
	case <-time.After(budget):
		t.Fatalf("the handler did not return within %s of the client disconnecting", budget)
	}
	p.assertAdmissionFree(t)
	s.assertServerWorkEnds(ctx, t, []string{qid}, bounded)
	p.assertPoolReleased(t)
}

func probeRoutedSiblings(ctx context.Context, t *testing.T, s *server, shape cancelShape, bounded bool) {
	client := s.client(t, adminUser, adminPassword, chclient.Config{
		Database:       shardedDB,
		DataShardCount: shardCount,
		MaxOpenConns:   shardedPoolConns,
		MaxIdleConns:   shardedPoolConns,
	})
	eng := &engine.Engine{Optimizer: optimizer.Default()}
	dr, err := eng.DryRunSQL(ctx, prom.NewExplainLang(schema.DefaultOTelMetrics(), cancelEvalTime, promql.ResourceBounds{}), shape.query)
	if err != nil {
		t.Fatalf("emit %s: %v", shape.query, err)
	}

	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	ids := make([]string, siblingCount)
	cursors := make([]chclient.Cursor, siblingCount)
	for i := range cursors {
		ids[i] = cancelQueryID(shape, fmt.Sprintf("sibling%d", i))
		cursors[i], err = client.QueryCursor(chclient.WithQueryID(dispatchCtx, ids[i]), dr.SQL, dr.Args...)
		if err != nil {
			t.Fatalf("dispatch sibling %d: %v", i, err)
		}
	}
	for _, id := range ids {
		s.waitRunningFor(ctx, t, id, cancelDeadline)
	}

	cancelDispatch()
	var wg sync.WaitGroup
	for _, cur := range cursors {
		wg.Add(1)
		go func(cur chclient.Cursor) {
			defer wg.Done()
			_ = cur.Close()
		}(cur)
	}
	closed := make(chan struct{})
	go func() { wg.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(closeBudget):
		t.Fatalf("closing the cancelled siblings did not return within %s", closeBudget)
	}

	// The gate capacity is released now. On a build that interrupts the
	// call, KILL QUERY ... SYNC confirmed every statement dead first; on one
	// that cannot, the capacity is advertised free while the work runs on.
	if running := s.runningCount(ctx, t, ids); (running == 0) != bounded {
		t.Errorf("%d statement(s) of the cancelled siblings still run once the fan-out gate released their capacity; "+
			"want none = %v", running, bounded)
	}
	if !s.goneWithin(ctx, t, ids, naturalRunBudget) {
		t.Fatalf("server work for %v still running after %s", ids, naturalRunBudget)
	}

	fctx, cancel := context.WithTimeout(ctx, cancelBound)
	defer cancel()
	cur, err := client.QueryCursor(fctx, "SELECT 1")
	if err != nil {
		t.Fatalf("a follow-up dispatch could not acquire the fan-out gate: %v", err)
	}
	_ = cur.Close()
}

// cancelQueryID is a unique query_id for one probe dispatch.
func cancelQueryID(shape cancelShape, scenario string) string {
	return fmt.Sprintf("cancel-%s-%s-%d", shape.name, scenario, time.Now().UnixNano())
}

// runningCount counts the statements still executing for ids, including any
// remote child a Distributed read dispatched on their behalf.
func (s *server) runningCount(ctx context.Context, t *testing.T, ids []string) uint64 {
	t.Helper()
	var n uint64
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT count() FROM system.processes WHERE has(?, query_id) OR has(?, initial_query_id)", ids, ids).Scan(&n)
	if err != nil {
		t.Fatalf("read system.processes: %v", err)
	}
	return n
}

// waitRunningFor waits until qid has been executing for at least d, so a
// cancellation issued next lands inside the query's CPU-bound call rather
// than in its read.
func (s *server) waitRunningFor(ctx context.Context, t *testing.T, qid string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(runningBudget)
	for {
		var elapsed float64
		var n uint64
		err := s.admin.Conn().QueryRow(ctx,
			"SELECT count(), max(elapsed) FROM system.processes WHERE query_id = ?", qid).Scan(&n, &elapsed)
		if err != nil {
			t.Fatalf("read system.processes: %v", err)
		}
		if n > 0 && elapsed >= d.Seconds() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query %s never ran for %s within %s", qid, d, runningBudget)
		}
		time.Sleep(pollInterval)
	}
}

// assertServerWorkEnds runs right after the client was answered and its
// admission released. On a build that interrupts the call the work for ids
// must stop within cancelBound; on one that cannot it is still running at
// that moment — the capacity is free while the server keeps working. Either
// way it must end on its own within naturalRunBudget.
func (s *server) assertServerWorkEnds(ctx context.Context, t *testing.T, ids []string, bounded bool) {
	t.Helper()
	if bounded {
		if !s.goneWithin(ctx, t, ids, cancelBound) {
			t.Errorf("server work for %v still running %s after the client was answered", ids, cancelBound)
		}
	} else if s.runningCount(ctx, t, ids) == 0 {
		t.Errorf("server work for %v already ended when the client was answered, on a build that cannot interrupt the call", ids)
	}
	if !s.goneWithin(ctx, t, ids, naturalRunBudget) {
		t.Fatalf("server work for %v still running after %s", ids, naturalRunBudget)
	}
}

// goneWithin polls until no statement for ids runs, or budget passes.
func (s *server) goneWithin(ctx context.Context, t *testing.T, ids []string, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if s.runningCount(ctx, t, ids) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// finishedDuration returns how long the server ran qid, from its terminal
// query_log row.
func (s *server) finishedDuration(ctx context.Context, t *testing.T, qid string) time.Duration {
	t.Helper()
	s.flushLogs(ctx, t)
	var ms, rows uint64
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(query_duration_ms), count() FROM system.query_log WHERE query_id = ? AND type != 'QueryStart'", qid).Scan(&ms, &rows)
	if err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no terminal row for %s", qid)
	}
	return time.Duration(ms) * time.Millisecond
}

// seedCancellationProbe applies cerberus's metrics DDL to the default
// database, seeds the two probe series there, and mirrors them into a
// data-shard layout — Distributed wrappers over *_local tables on a cluster
// whose one replica is this server — for the routed-sibling probe.
func seedCancellationProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	evalSec := cancelEvalTime.Unix()
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT 'svc', '%s', map('host', 'a'), toDateTime64(%d, 9) - toIntervalMillisecond(1 + number * 2), toFloat64(number %% 97)
FROM numbers(%d)`, foldMetric, evalSec, foldSamples))
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT 'svc', '%s', map(arrayStringConcat(arrayMap(x -> repeat('.', %d), range(%d))), 'v'), toDateTime64(%d, 9) - toIntervalSecond(1), 1`,
		regexMetric, regexChunkChars, regexKeyChunks, evalSec))

	s.exec(ctx, t, "CREATE DATABASE "+shardedDB)
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		local := shardedDB + "." + table + ddl.DataShardLocalSuffix
		s.exec(ctx, t, fmt.Sprintf("CREATE TABLE %s AS %s.%s", local, serverDB, table))
		s.exec(ctx, t, fmt.Sprintf("CREATE TABLE %s.%s AS %s ENGINE = Distributed(%s, %s, %s)",
			shardedDB, table, local, shardCluster, shardedDB, table+ddl.DataShardLocalSuffix))
		s.exec(ctx, t, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s.%s", local, serverDB, table))
	}
}
