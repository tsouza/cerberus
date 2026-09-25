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

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
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
// inside a single call of function. The work in that call scales with a size
// the shape seeds; calibrateShape picks the size per build and substrate.
type cancelShape struct {
	name     string
	function string
	// queryFor is the PromQL the shape sends for the series named metric.
	queryFor func(metric string) string
	// seedSQL inserts a series named metric of the given size into the
	// default database's otel_metrics_gauge.
	seedSQL func(metric string, size int) string
	// baseSize is the first size calibration tries; maxSize bounds it, so a
	// slow substrate cannot grow the seed past the container's memory.
	baseSize, maxSize int
	bounded           func(foldBounded, regexBounded bool) bool
	// inCall reports, from every system.processes reading of the shape's
	// query so far (oldest first), that the query is running the part of
	// function's call whose interruption the build table describes. The
	// work ahead of that part — the read, the per-row label normalization,
	// an aggregation, a filter, the other actions of the call's own
	// expression — stops on a cancellation on some or all builds, and it
	// grows with the seed and with the runner's load: a cancellation that
	// lands in it can end the query within milliseconds whatever the build's
	// cancellation gap. Every probe therefore cancels only once inCall
	// holds, never after a fixed elapsed time.
	inCall func(readings []callProgress) bool
	// query is the calibrated query, set by calibrateShape.
	query string
}

// callProgress is the part of one system.processes row the inCall
// predicates read.
type callProgress struct {
	elapsed             time.Duration
	readRows, totalRows uint64
	// filterPassedRows and functionExecutions are the query's
	// FilterTransformPassedRows and FunctionExecute profile events: the rows
	// its FilterTransforms have passed, and the function executions it has
	// started — once per block for an ordinary function, once per element
	// for a lambda a higher-order function applies element by element.
	filterPassedRows, functionExecutions uint64
}

// inputRead reports that the query's sources have read every row they will
// read.
func (p callProgress) inputRead() bool { return p.totalRows > 0 && p.readRows >= p.totalRows }

// foldLoopRunning is array_fold's inCall. The fold is evaluated by the
// ExpressionTransform that follows the emitted `WHERE length(window_vals) >=
// 2`, whose FilterTransform passes the aggregated series row last. By then
// the query has executed its functions once per block, a few hundred times.
// arrayFold first splits its input into per-element arguments, which no
// pinned build interrupts, and then applies its lambda once per element: the
// loop ClickHouse#108192 made check for cancellation. The fold's own
// expression executes only a handful of functions outside that loop, so the
// count passing foldLoopGrowth times its value at the filter's pass is the
// loop running.
func foldLoopRunning(readings []callProgress) bool {
	i := slices.IndexFunc(readings, func(p callProgress) bool { return p.filterPassedRows > 0 })
	return i >= 0 && readings[len(readings)-1].functionExecutions > foldLoopGrowth*readings[i].functionExecutions
}

// foldLoopGrowth is the factor by which array_fold's function executions
// must grow past their count at the filter's pass; see foldLoopRunning.
const foldLoopGrowth = 2

// regexCallRunning is regex_replace's inCall. The normalization runs
// replaceRegexpAll once over the block the read produced, so for the whole
// call the query's function-execution count stands still. The actions ahead
// of it in the same expression each run briefly over bytes the read already
// produced; on 26.7.13.12 a cancellation landing after the read's progress
// report but before the call was observed to end the query at once. None of
// those actions takes as long as producing its input took, so the count
// standing still, with the input read, for longer than the query needed to
// read that input is the call running.
func regexCallRunning(readings []callProgress) bool {
	i := slices.IndexFunc(readings, callProgress.inputRead)
	if i < 0 {
		return false
	}
	last := len(readings) - 1
	since := last
	for since > i && readings[since-1].functionExecutions == readings[last].functionExecutions {
		since--
	}
	return readings[last].elapsed-readings[since].elapsed > readings[i].elapsed
}

var cancelShapes = []cancelShape{
	{
		// double_exponential_smoothing lowers to an arrayFold over the
		// window's samples; one series whose size samples all fall inside
		// the window makes that single call the bulk of the query. The fold
		// holds about 4 KiB per sample, and the routed-sibling scenario runs
		// two at once, so maxSize keeps both inside the container's memory.
		name:     "array_fold",
		function: "arrayFold",
		queryFor: func(metric string) string { return "double_exponential_smoothing(" + metric + "[10m], 0.5, 0.5)" },
		seedSQL: func(metric string, size int) string {
			return fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT 'svc', '%s', map('host', 'a'), toDateTime64(%d, 9) - toIntervalMicrosecond(1 + number * %d), toFloat64(number %% 97)
FROM numbers(%d)`, metric, cancelEvalTime.Unix(), foldSampleSpacingMicros, size)
		},
		baseSize: 200_000,
		maxSize:  700_000,
		bounded:  func(fold, _ bool) bool { return fold },
		inCall:   foldLoopRunning,
	},
	{
		// Every PromQL selector normalizes label names with
		// replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'); a label name of size
		// chunks of regexChunkChars characters that all need replacing makes
		// that single call the bulk of the query.
		name:     "regex_replace",
		function: "replaceRegexpAll",
		queryFor: func(metric string) string { return metric },
		seedSQL: func(metric string, size int) string {
			return fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT 'svc', '%s', map(arrayStringConcat(arrayMap(x -> repeat('.', %d), range(%d))), 'v'), toDateTime64(%d, 9) - toIntervalSecond(1), 1`,
				metric, regexChunkChars, size, cancelEvalTime.Unix())
		},
		baseSize: 30,
		maxSize:  200,
		bounded:  func(_, regex bool) bool { return regex },
		inCall:   regexCallRunning,
	},
}

// Seed and budget constants. Calibration grows each shape's seed until the
// call leaves at least calibrationTarget of its uncancelled run to judge on
// the substrate at hand, within the shape's maxSize.
const (
	foldSampleSpacingMicros = 100       // 6 million samples fit the 10m window
	regexChunkChars         = 1_000_000 // repeat()'s own per-call cap
	calibrationRounds       = 4
	settleBudget            = 2 * time.Second
	handlerSlack            = 3 * time.Second
	naturalRunBudget        = 120 * time.Second
	runningBudget           = 30 * time.Second
	closeBudget             = 30 * time.Second
	pollInterval            = 50 * time.Millisecond
	shardedDB               = "sharded"
	shardCluster            = "chserver"
	shardCount              = 2
	siblingCount            = 2
	shardedPoolConns        = shardCount * siblingCount * 2
)

// calibrationTarget is how much of a calibrated shape's natural run the call
// aims to leave after the latest point a probe cancels it (see
// calibrateShape): the widest separation an assertion needs, with half again
// as headroom for run-to-run variance. The headroom is also the window the
// request-deadline probe places its deadline in
// (cancelExpectation.requestDeadline).
const calibrationTarget = minRemaining * 3 / 2

// cancelProbeNanoCPUs throttles the cancellation probes' server to half a
// CPU. The probed functions are single-threaded, so the throttle stretches
// one call's wall time without growing its memory. That matters twice: the
// fold holds about 4 KiB per sample, so reaching calibrationTarget on a fast
// runner by size alone would exhaust memory once two siblings run at once;
// and an interrupted call's own teardown — freeing that memory — grows with
// the size, which would narrow the gap to an uninterrupted one. Measured at
// 700,000 samples unthrottled, interrupted siblings took 2.5 s to end; at
// half a CPU with calibrated sizes, every interrupted call ended within 1.4 s.
const cancelProbeNanoCPUs = 500_000_000

// cancelEvalTime is the instant every probe evaluates at.
var cancelEvalTime = time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)

// TestCancellation_CPUBoundEmittedShapesAcrossBuilds extends the real-server
// cancellation proof from cooperative sleeps to the CPU-bound functions
// cerberus emits. For every pinned build and each shape it drives:
//
//   - a request deadline through the production Prometheus handler, answered
//     503 errorType=timeout;
//   - a client disconnect mid-call through the same handler, answered 503
//     errorType=canceled;
//   - routed sibling cancellation: two statements dispatched through a
//     data-shard client over Distributed tables, cancelled together and
//     closed through the fan-out gate's release.
//
// In every scenario the capacity the dispatch held — admission slot or gate
// weight — is released only after cerberus's KILL QUERY ... SYNC. On a build
// that interrupts the call no statement, initiator or remote child, still
// runs at that release, and the server's query_log shows the work ended
// within a quarter of the work that was left at the cancellation. On a build
// that cannot, query_log shows the server kept evaluating for at least half of
// it. Every scenario also
// asserts the work eventually ends, that the connection pool and the fan-out
// gate are usable again, and that the client is answered within a bounded
// time. Which branch a build takes is fixed by the table above and must agree
// with chopt.CancellationGaps, the version policy cerberus reports at boot.
func TestCancellation_CPUBoundEmittedShapesAcrossBuilds(t *testing.T) {
	clusterConfig, err := filepath.Abs(filepath.Join("testdata", "cluster.xml"))
	if err != nil {
		t.Fatalf("cluster config path: %v", err)
	}
	for _, build := range cancellationBuilds {
		t.Run(build.image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			s := startServer(ctx, t, build.image, tcclickhouse.WithConfigFile(clusterConfig),
				testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) { hc.NanoCPUs = cancelProbeNanoCPUs }))
			seedCancellationProbe(ctx, t, s)

			// The fleet probe's cluster arm reads every replica's build
			// through clusterAllReplicas; this cluster's one replica is s.
			if versions, err := s.admin.ProbeClusterVersions(ctx, shardCluster); err != nil || len(versions) != 1 || versions[0] != s.version {
				t.Fatalf("ProbeClusterVersions(%q) = %v, %v; want [%s]", shardCluster, versions, err, s.version)
			}

			set := chopttest.ResolveEnabledSet(ctx, t, s.admin, chopt.SelectionAuto)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())

			for _, shape := range cancelShapes {
				t.Run(shape.name, func(t *testing.T) {
					bounded := shape.bounded(build.foldBounded, build.regexBounded)
					assertCancellationPolicy(t, s.version, shape.function, bounded)
					shape, natural, entered := s.calibrateShape(ctx, t, shape)
					want := cancelExpectation{bounded: bounded, natural: natural, entered: entered}

					t.Run("request_deadline", func(t *testing.T) {
						probeRequestDeadline(ctx, t, s, rules, shape, want)
					})
					t.Run("client_disconnect", func(t *testing.T) {
						probeClientDisconnect(ctx, t, s, rules, shape, want)
					})
					t.Run("routed_siblings", func(t *testing.T) {
						probeRoutedSiblings(ctx, t, s, shape, want)
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
	deadline := time.Now().Add(settleBudget)
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

func probeRequestDeadline(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, want cancelExpectation) {
	deadline := want.requestDeadline()
	p := newPromProbe(t, s, rules, deadline)
	qid := cancelQueryID(shape, "deadline")
	start := time.Now()
	rec := p.serve(chclient.WithQueryID(ctx, qid), shape)
	answered := time.Since(start)
	assertErrorType(t, rec, "timeout")
	// The answer waits for cerberus's KILL QUERY ... SYNC, which confirms
	// promptly where the call is interruptible and gives up after
	// chclient.KillDataShardQueryTimeout where it is not.
	budget := deadline + handlerSlack
	if !want.bounded {
		budget += chclient.KillDataShardQueryTimeout
	}
	if answered > budget {
		t.Errorf("the client was answered after %s; want within %s", answered, budget)
	}
	p.assertAdmissionFree(t)
	// The server's own max_execution_time is the cancellation here.
	s.assertServerWorkEnds(ctx, t, []string{qid}, want, start.Add(deadline))
	p.assertPoolReleased(t)
}

func probeClientDisconnect(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, want cancelExpectation) {
	p := newPromProbe(t, s, rules, 0)
	qid := cancelQueryID(shape, "disconnect")
	reqCtx, disconnect := context.WithCancel(chclient.WithQueryID(ctx, qid))
	defer disconnect()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- p.serve(reqCtx, shape) }()

	s.waitInCall(ctx, t, shape, qid)
	cancelAt := time.Now()
	disconnect()
	budget := handlerSlack
	if !want.bounded {
		budget += chclient.KillDataShardQueryTimeout
	}
	select {
	case rec := <-done:
		assertErrorType(t, rec, "canceled")
	case <-time.After(budget):
		t.Fatalf("the handler did not return within %s of the client disconnecting", budget)
	}
	p.assertAdmissionFree(t)
	s.assertServerWorkEnds(ctx, t, []string{qid}, want, cancelAt)
	p.assertPoolReleased(t)
}

// dryRunShape emits shape's calibrated query to ClickHouse SQL against the
// metrics schema at cancelEvalTime. Both the natural run and every
// concurrent dispatch (the routed-siblings probe and its calibration replica)
// share this one emission path.
func dryRunShape(ctx context.Context, t *testing.T, shape cancelShape) engine.DryRun {
	t.Helper()
	eng := &engine.Engine{Optimizer: optimizer.Default()}
	dr, err := eng.DryRunSQL(ctx, prom.NewExplainLang(schema.DefaultOTelMetrics(), cancelEvalTime, promql.ResourceBounds{}), shape.query)
	if err != nil {
		t.Fatalf("emit %s: %v", shape.query, err)
	}
	return dr
}

func probeRoutedSiblings(ctx context.Context, t *testing.T, s *server, shape cancelShape, want cancelExpectation) {
	client := s.client(t, adminUser, adminPassword, chclient.Config{
		Database:       shardedDB,
		DataShardCount: shardCount,
		MaxOpenConns:   shardedPoolConns,
		MaxIdleConns:   shardedPoolConns,
	})
	dr := dryRunShape(ctx, t, shape)

	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	ids := make([]string, siblingCount)
	cursors := make([]chclient.Cursor, siblingCount)
	for i := range cursors {
		ids[i] = cancelQueryID(shape, fmt.Sprintf("sibling%d", i))
		cur, err := client.QueryCursor(chclient.WithQueryID(dispatchCtx, ids[i]), dr.SQL, dr.Args...)
		cursors[i] = cur
		if err != nil {
			t.Fatalf("dispatch sibling %d: %v", i, err)
		}
	}
	for _, id := range ids {
		s.waitInCall(ctx, t, shape, id)
	}

	cancelAt := time.Now()
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

	// The gate capacity is released now.
	s.assertServerWorkEnds(ctx, t, ids, want, cancelAt)

	fctx, cancel := context.WithTimeout(ctx, settleBudget)
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

// waitInCall reads the running query qid every pollInterval until
// shape.inCall holds for the readings so far, so a cancellation issued next
// lands inside the query's CPU-bound call rather than in work ahead of it.
func (s *server) waitInCall(ctx context.Context, t *testing.T, shape cancelShape, qid string) {
	t.Helper()
	deadline := time.Now().Add(runningBudget)
	var readings []callProgress
	for {
		if p, running := s.callProgress(ctx, t, qid); running {
			if readings = append(readings, p); shape.inCall(readings) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("query %s never reached its %s call within %s", qid, shape.function, runningBudget)
		}
		time.Sleep(pollInterval)
	}
}

// callProgress reads qid's row in system.processes; running is false when
// qid is not executing.
func (s *server) callProgress(ctx context.Context, t *testing.T, qid string) (p callProgress, running bool) {
	t.Helper()
	var (
		n       uint64
		elapsed float64
	)
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT count(), max(elapsed), max(read_rows), max(total_rows_approx), "+
			"max(ProfileEvents['FilterTransformPassedRows']), max(ProfileEvents['FunctionExecute']) "+
			"FROM system.processes WHERE query_id = ?", qid).
		Scan(&n, &elapsed, &p.readRows, &p.totalRows, &p.filterPassedRows, &p.functionExecutions)
	if err != nil {
		t.Fatalf("read system.processes: %v", err)
	}
	p.elapsed = time.Duration(elapsed * float64(time.Second))
	return p, n > 0
}

// cancelExpectation is what one shape must show on one build: whether the
// build interrupts its CPU-bound call, how long the server takes to run the
// shape to completion when nothing cancels it, and how far into that run the
// query reached the call.
type cancelExpectation struct {
	bounded bool
	natural time.Duration
	entered time.Duration
}

// requestDeadline is the request timeout the deadline probe sends. Unlike
// the other probes it cannot wait for inCall: the deadline is fixed before
// the query starts. It sits midway between the natural run's entry into the
// call and the latest instant that still leaves minRemaining of the call to
// judge, which calibration keeps at least minRemaining/2 apart — so the
// deadline clears the entry, and the probe's remainder clears minRemaining,
// by the same margin.
func (w cancelExpectation) requestDeadline() time.Duration {
	return w.entered + (w.natural-w.entered-minRemaining)/2
}

// Separation, relative to the work left at the cancellation (remaining). An
// interrupted call must end within remaining/interruptedFraction; an
// uninterrupted one runs the call out and must end no sooner than
// remaining/uninterruptedFraction. Both scale with the calibrated workload,
// so neither is tied to a runner's speed, and the band between a quarter and
// a half of the remainder separates them. Observed under cancelProbeNanoCPUs:
// interrupted calls ended within 0.14 of the remainder, uninterrupted ones
// after 0.7 or more of it. minRemaining is the least remainder the probe will
// judge, so that a quarter of it still stands clear of scheduling noise.
const (
	interruptedFraction   = 4
	uninterruptedFraction = 2
	minRemaining          = 6 * time.Second
)

// assertServerWorkEnds runs the moment cerberus has released the capacity a
// cancelled dispatch held — its admission slot or its fan-out gate weight.
//
// The verdict is read from the server's own query_log and anchored on the
// shape's measured natural duration, not on a fixed threshold: remaining is
// how much of the shape's work was left when cancelAt came. The probe first
// requires remaining to be at least minRemaining, failing diagnosably when
// calibration could not reach that on the substrate. On a build that
// interrupts the call, KILL QUERY ... SYNC confirmed the work dead before
// the release, so no statement for ids — initiator or remote child — may
// still run, and the work must have ended within remaining/interruptedFraction
// of cancelAt. On a build that cannot, the server runs the call out, so the
// work must have ended at least remaining/uninterruptedFraction after
// cancelAt. Either way it must end on its own within naturalRunBudget.
func (s *server) assertServerWorkEnds(ctx context.Context, t *testing.T, ids []string, want cancelExpectation, cancelAt time.Time) {
	t.Helper()
	if want.bounded {
		if running := s.runningCount(ctx, t, ids); running != 0 {
			t.Errorf("%d statement(s) for %v still run once their capacity was released", running, ids)
		}
	}
	if !s.goneWithin(ctx, t, ids, naturalRunBudget) {
		t.Fatalf("server work for %v still running after %s", ids, naturalRunBudget)
	}
	started, ended := s.workSpan(ctx, t, ids)
	remaining := want.natural - cancelAt.Sub(started)
	delay := ended.Sub(cancelAt)
	t.Logf("server work for %v: natural %s, %s left at the cancellation, ended %s after it", ids, want.natural, remaining, delay)
	if remaining < minRemaining {
		t.Fatalf("only %s of the shape's %s natural run was left at the cancellation; the probe needs at least %s "+
			"to separate an interrupted call from an uninterrupted one — raise the shape's maxSize for this substrate",
			remaining, want.natural, minRemaining)
	}
	if limit := remaining / interruptedFraction; want.bounded && delay > limit {
		t.Errorf("server work for %v ended %s after the cancellation; want within %s (a quarter of the remaining work) "+
			"on a build that interrupts the call", ids, delay, limit)
	}
	if floor := remaining / uninterruptedFraction; !want.bounded && delay < floor {
		t.Errorf("server work for %v ended %s after the cancellation; want at least %s (half the remaining work) on a build "+
			"that cannot interrupt the call", ids, delay, floor)
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

// workSpan returns when the first statement for ids — or any remote child it
// dispatched — started and when the last one ended, from the server's own
// query_log.
func (s *server) workSpan(ctx context.Context, t *testing.T, ids []string) (started, ended time.Time) {
	t.Helper()
	s.flushLogs(ctx, t)
	var (
		startMicros, endMicros int64
		rows                   uint64
	)
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT toInt64(min(toUnixTimestamp64Micro(query_start_time_microseconds))), "+
			"toInt64(max(toUnixTimestamp64Micro(event_time_microseconds))), count() FROM system.query_log "+
			"WHERE (has(?, query_id) OR has(?, initial_query_id)) AND type != 'QueryStart'",
		ids, ids).Scan(&startMicros, &endMicros, &rows)
	if err != nil {
		t.Fatalf("read query_log for %v: %v", ids, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no terminal row for %v", ids)
	}
	return time.UnixMicro(startMicros), time.UnixMicro(endMicros)
}

// naturalRun runs shape's emitted SQL to completion, uncancelled, and returns
// how long the server took — the yardstick assertServerWorkEnds measures a
// cancellation against — and how far into that run shape.inCall first held.
// A run inCall was never seen to hold in reports entered as natural: none of
// its work can be counted as the call's.
func (s *server) naturalRun(ctx context.Context, t *testing.T, shape cancelShape) (natural, entered time.Duration) {
	t.Helper()
	dr := dryRunShape(ctx, t, shape)
	qid := cancelQueryID(shape, "natural")
	done := make(chan error, 1)
	go func() {
		rows, err := s.admin.Conn().Query(clickhouse.Context(ctx, clickhouse.WithQueryID(qid)), dr.SQL, dr.Args...)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
		}
		done <- err
	}()
	var (
		readings []callProgress
		inCall   bool
	)
	for finished := false; !finished; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("natural run of %s: %v", shape.query, err)
			}
			finished = true
		case <-time.After(pollInterval):
			if p, running := s.callProgress(ctx, t, qid); running && !inCall {
				readings = append(readings, p)
				if inCall = shape.inCall(readings); inCall {
					entered = p.elapsed
				}
			}
		}
	}
	started, ended := s.workSpan(ctx, t, []string{qid})
	natural = ended.Sub(started)
	if !inCall {
		return natural, natural
	}
	return natural, entered
}

// seedCancellationProbe applies cerberus's metrics DDL to the default
// database and mirrors it into a data-shard layout — Distributed wrappers over
// *_local tables on a cluster whose one replica is this server — for the
// routed-sibling probe. The probe series themselves are seeded by
// calibrateShape, into both.
func seedCancellationProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, "CREATE DATABASE "+shardedDB)
	for _, table := range shardedTables {
		local := shardedDB + "." + table + ddl.DataShardLocalSuffix
		s.exec(ctx, t, fmt.Sprintf("CREATE TABLE %s AS %s.%s", local, serverDB, table))
		s.exec(ctx, t, fmt.Sprintf("CREATE TABLE %s.%s AS %s ENGINE = Distributed(%s, %s, %s)",
			shardedDB, table, local, shardCluster, shardedDB, table+ddl.DataShardLocalSuffix))
	}
}

// shardedTables are the metrics tables the PromQL gauge read spans.
var shardedTables = []string{"otel_metrics_gauge", "otel_metrics_sum"}

// calibrateShape sizes shape's seed to the substrate: it seeds a series at
// the shape's baseSize, measures the uncancelled run, and re-seeds larger in
// proportion until the work left after the latest cancellation a probe makes
// clears calibrationTarget or the size reaches maxSize, for at most
// calibrationRounds rounds. That latest cancellation is the routed-sibling
// probe's, and the work left for it is measured by actually running that
// probe's own dispatch shape (siblingsElapsedToCall), not estimated from the
// lone run: two statements sharing cancelProbeNanoCPUs's one throttled CPU do
// not reach the call in siblingCount times the lone run's entered — measured
// on this substrate, the two-statement contention cost far more than the
// even split a linear scale-up assumes (a calibration that estimated 10.7s
// of remaining work this way left only 4.9s at the real cancellation). It
// returns the shape with its calibrated query, that query's natural duration
// and the point the lone run entered the call. A run still short of the
// target at maxSize is returned as is: the scenario assertions then fail
// with the separation they could not get, rather than judging a probe that
// cannot tell the two outcomes apart.
func (s *server) calibrateShape(ctx context.Context, t *testing.T, shape cancelShape) (cancelShape, time.Duration, time.Duration) {
	t.Helper()
	size := shape.baseSize
	var natural, entered time.Duration
	for round := range calibrationRounds {
		metric := fmt.Sprintf("%s_probe_%d", shape.name, size)
		s.exec(ctx, t, shape.seedSQL(metric, size))
		local := shardedDB + ".otel_metrics_gauge" + ddl.DataShardLocalSuffix
		s.exec(ctx, t, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s.otel_metrics_gauge WHERE MetricName = ?", local, serverDB), metric)
		shape.query = shape.queryFor(metric)
		natural, entered = s.naturalRun(ctx, t, shape)
		siblingsEntered := s.siblingsElapsedToCall(ctx, t, shape)
		left := natural - siblingsEntered
		t.Logf("calibration round %d: %s at size %d ran %s, lone entered after %s, siblings entered after %s, leaving %s (target %s)",
			round, shape.name, size, natural, entered, siblingsEntered, left, calibrationTarget)
		if left >= calibrationTarget || size >= shape.maxSize {
			break
		}
		next := size * calibrationGrowth
		if left > 0 {
			next = int(float64(size) * float64(calibrationTarget) / float64(left) * calibrationOvershoot)
		}
		size = min(max(next, size+1), shape.maxSize)
	}
	return shape, natural, entered
}

// siblingsElapsedToCall dispatches siblingCount concurrent statements of
// shape's calibrated query over the sharded tables — the same dispatch
// probeRoutedSiblings performs against the real scenario — and returns how
// long, from dispatch, the last of them took to reach the call. Calibration
// measures this directly instead of scaling the lone run's entered by
// siblingCount, because the two figures diverge substantially on the
// throttled substrate cancelProbeNanoCPUs creates: contending for the one
// available CPU costs the pair more than an even split of it would, an
// effect a linear model has no way to capture. The dispatch is cancelled and
// its siblings closed before returning, so calibration leaves no statement
// running past this call.
func (s *server) siblingsElapsedToCall(ctx context.Context, t *testing.T, shape cancelShape) time.Duration {
	t.Helper()
	client := s.client(t, adminUser, adminPassword, chclient.Config{
		Database:       shardedDB,
		DataShardCount: shardCount,
		MaxOpenConns:   shardedPoolConns,
		MaxIdleConns:   shardedPoolConns,
	})
	dr := dryRunShape(ctx, t, shape)

	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	start := time.Now()
	ids := make([]string, siblingCount)
	cursors := make([]chclient.Cursor, siblingCount)
	for i := range cursors {
		ids[i] = cancelQueryID(shape, fmt.Sprintf("calibsibling%d", i))
		cur, err := client.QueryCursor(chclient.WithQueryID(dispatchCtx, ids[i]), dr.SQL, dr.Args...)
		if err != nil {
			t.Fatalf("dispatch calibration sibling %d: %v", i, err)
		}
		cursors[i] = cur
	}
	for _, id := range ids {
		s.waitInCall(ctx, t, shape, id)
	}
	elapsed := time.Since(start)

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
		t.Fatalf("closing calibration siblings did not return within %s", closeBudget)
	}
	if !s.goneWithin(ctx, t, ids, naturalRunBudget) {
		t.Fatalf("calibration siblings for %v still running after %s", ids, naturalRunBudget)
	}
	return elapsed
}

// calibrationGrowth multiplies a seed whose run left no work to judge, which
// gives no proportion to scale by.
const calibrationGrowth = 2

// calibrationOvershoot scales a re-seed a little past the proportional size,
// since a shape's run is not exactly linear in its size.
const calibrationOvershoot = 1.2
