//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	"github.com/moby/moby/client"
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
//     cancellation inside a call; its arrayFold runs out past both KILL
//     QUERY and max_execution_time.
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
	// warmSize is the seed warmShape runs the shape at before calibrating.
	warmSize int
	// meter is the unit the shape's work is judged in; see workMeter.
	meter   workMeter
	bounded func(foldBounded, regexBounded bool) bool
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
		// Large enough that the fold's lambda runs, small enough to cost
		// milliseconds.
		warmSize: 1_000,
		meter:    loopExecutions,
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
		warmSize: 1,
		meter:    wallClock,
		bounded:  func(_, regex bool) bool { return regex },
		inCall:   regexCallRunning,
	},
}

// Seed and budget constants. Calibration grows each shape's seed until the
// call leaves at least its calibration target of work to judge on the
// substrate at hand, within the shape's maxSize.
const (
	foldSampleSpacingMicros = 100       // 6 million samples fit the 10m window
	regexChunkChars         = 1_000_000 // repeat()'s own per-call cap
	calibrationRounds       = 4
	settleBudget            = 2 * time.Second
	handlerSlack            = 3 * time.Second
	naturalRunBudget        = 120 * time.Second
	// runningBudget bounds waitInCall: how long a dispatched statement gets
	// to reach its probed function's call. Sampling siblingsAtCall
	// multiple times per calibration round (calibrationSiblingSamples)
	// lengthens how long that can legitimately take, and calibration's own
	// re-seeding grows the same table round over round, so a later round's
	// entry can take noticeably longer than an earlier one at a smaller
	// size; 60s covers that observed growth with headroom to spare.
	runningBudget    = 60 * time.Second
	closeBudget      = 30 * time.Second
	shardedDB        = "sharded"
	shardCluster     = "chserver"
	shardCount       = 2
	siblingCount     = 2
	shardedPoolConns = shardCount * siblingCount * 2
)

// Calibration targets: how much of a calibrated shape's work the call aims to
// leave after the latest point a probe cancels it (see calibrateShape), in
// each meter. Each is the least remainder an assertion judges, with
// calibrationHeadroom over it for run-to-run variance.
const (
	calibrationHeadroom  = 1.5
	calibrationTarget    = time.Duration(calibrationHeadroom * float64(minRemaining))
	calibrationLoopShare = calibrationHeadroom / minLoopLeftFraction
)

// cancelProbeNanoCPUs throttles the cancellation probes' server to half a
// CPU. The probed functions are single-threaded, so the throttle stretches
// one call's wall time without growing its memory: the fold holds about
// 4 KiB per sample, so stretching a call on a fast runner by size alone would
// exhaust memory once two siblings run at once.
//
// A lower throttle (tried at both a quarter and 30% of a CPU) was rejected:
// the heavier throttle starved the ClickHouse
// server's own thread pool enough that KILL QUERY's confirmation itself
// missed its deadline repeatedly under real GitHub Actions contention —
// #3746's regression, surfaced as calibration retrying past the whole
// test's 20-minute timeout rather than a wrong margin. Half a CPU keeps
// query cancellation itself reliable; calibrateShape's
// calibrationSiblingSamples (below) is what actually fixes #3746's margin
// noise, by taking the worst of several sibling samples per round instead
// of trusting one.
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
// runs at that release, and the server's own accounting shows it performed at
// most a quarter of the work that was left at the cancellation. On a build
// that cannot, it shows the server kept evaluating at least half of it. The
// work is measured in the shape's workMeter, against the shape's natural run
// on a server warmed to the state every probe shares (warmShape). Every
// scenario also asserts the work eventually ends, that the connection pool and the fan-out
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
					shape, prof := s.calibrateShape(ctx, t, shape)
					want := cancelExpectation{bounded: bounded, meter: shape.meter, naturalProfile: prof}

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

// probeRequestDeadline drives the production handler's request deadline. The
// deadline is fixed when the request starts, and how long the query then
// takes to reach the call varies with the load on the runner, so each
// dispatch holds the server at the query's observed entry into the call until
// just before the deadline expires on it (observeToDeadline).
// landRequestDeadline re-arms a deadline that passed before the entry, and
// only the dispatch that lands is judged. Every dispatch, judged or not, must
// be answered 503 errorType=timeout within budget and free its admission slot.
func probeRequestDeadline(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, want cancelExpectation) {
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer docker.Close()
	var (
		p promProbe
		d *promDispatch
	)
	dispatch := func(deadline time.Duration) deadlineObservation {
		s.awaitMergesIdle(ctx, t)
		p = newPromProbe(t, s, rules, deadline)
		d = &promDispatch{ctx: ctx, t: t, s: s, docker: docker, qid: cancelQueryID(shape, "deadline"), start: time.Now()}
		done := make(chan *httptest.ResponseRecorder, 1)
		d.done = done
		go func() { done <- p.serve(chclient.WithQueryID(ctx, d.qid), shape) }()
		obs := observeToDeadline(shape.inCall, deadline, d)
		if d.rec == nil {
			d.rec = <-done
		}
		answered := time.Since(d.start)
		if d.rec.Code == http.StatusOK && !shape.inCall(obs.seen) {
			t.Fatalf("query %s answered within its %s deadline before any reading saw it inside its %s call: the "+
				"readings stalled, so nothing held it", d.qid, deadline, shape.function)
		}
		assertErrorType(t, d.rec, "timeout")
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
		return obs
	}
	// A dispatch that cannot be judged still runs on where its call cannot be
	// interrupted; the next one must not share the server with it.
	discard := func() {
		if !s.goneWithin(ctx, t, []string{d.qid}, naturalRunBudget) {
			t.Fatalf("server work for unjudged dispatch %s still running after %s", d.qid, naturalRunBudget)
		}
		p.assertPoolReleased(t)
	}
	deadline, seen, err := want.landRequestDeadline(shape.inCall, dispatch, discard)
	if err != nil {
		t.Fatalf("%s request deadline: %v", shape.function, err)
	}
	t.Logf("request deadline %s (first %s) landed query %s inside its %s call, held there for %s", deadline,
		want.requestDeadline(), d.qid, shape.function, d.resumedAt.Sub(d.heldFrom))
	// The server's own max_execution_time is the cancellation here. It
	// expires within resumeLead of the server resuming, and the work in
	// between counts as performed after the cancellation.
	s.assertServerWorkEnds(ctx, t, []string{d.qid}, want, d.resumedAt, d.resumedAt.Sub(d.heldFrom),
		map[string][]callProgress{d.qid: seen})
	p.assertPoolReleased(t)
}

// promDispatch is one request-deadline dispatch through a promProbe, as
// observeToDeadline drives it. Its hold pauses the server's container: the
// cgroup freezer stops every server thread, so the query makes no progress
// while the wall clock runs on.
type promDispatch struct {
	ctx    context.Context
	t      *testing.T
	s      *server
	docker *testcontainers.DockerClient
	qid    string
	start  time.Time
	done   <-chan *httptest.ResponseRecorder
	// rec is the answer, once it has arrived.
	rec *httptest.ResponseRecorder
	// heldFrom is when the pause had taken effect, and resumedAt when the
	// unpause was requested: the server was certainly paused between them,
	// so the span undercounts the hold rather than credit the query with
	// time it did not run.
	heldFrom, resumedAt time.Time
}

func (d *promDispatch) sinceSent() time.Duration { return time.Since(d.start) }

func (d *promDispatch) await() bool {
	select {
	case d.rec = <-d.done:
		return false
	case <-time.After(pollInterval):
		return true
	}
}

func (d *promDispatch) read() (callProgress, bool) { return d.s.callProgress(d.ctx, d.t, d.qid) }

func (d *promDispatch) hold(until time.Duration) time.Duration {
	// The unpause runs on every path out, a failed or late-failing pause
	// included, and outlives the probe's own context, so the container is
	// never left frozen.
	unpaused := false
	unpause := func() error {
		_, err := d.docker.ContainerUnpause(context.WithoutCancel(d.ctx), d.s.containerID, client.ContainerUnpauseOptions{})
		unpaused = err == nil
		return err
	}
	defer func() {
		if !unpaused {
			_ = unpause()
		}
	}()
	if _, err := d.docker.ContainerPause(d.ctx, d.s.containerID, client.ContainerPauseOptions{}); err != nil {
		d.t.Fatalf("pause %s: %v", d.s.image, err)
	}
	d.heldFrom = time.Now()
	time.Sleep(until - d.sinceSent())
	d.resumedAt = time.Now()
	if err := unpause(); err != nil {
		d.t.Fatalf("unpause %s: %v", d.s.image, err)
	}
	return d.sinceSent()
}

func probeClientDisconnect(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, shape cancelShape, want cancelExpectation) {
	s.awaitMergesIdle(ctx, t)
	p := newPromProbe(t, s, rules, 0)
	qid := cancelQueryID(shape, "disconnect")
	reqCtx, disconnect := context.WithCancel(chclient.WithQueryID(ctx, qid))
	defer disconnect()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- p.serve(reqCtx, shape) }()

	seen := s.waitInCall(ctx, t, shape, qid)
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
	s.assertServerWorkEnds(ctx, t, []string{qid}, want, cancelAt, 0, map[string][]callProgress{qid: seen})
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
	d := s.dispatchSiblings(ctx, t, shape, "sibling")
	defer d.cancel()
	seen := s.siblingsInCall(ctx, t, shape, d)
	cancelAt := d.cancelAndClose(t)

	// The gate capacity is released now.
	s.assertServerWorkEnds(ctx, t, d.ids, want, cancelAt, 0, seen)

	fctx, cancel := context.WithTimeout(ctx, settleBudget)
	defer cancel()
	cur, err := d.client.QueryCursor(fctx, "SELECT 1")
	if err != nil {
		t.Fatalf("a follow-up dispatch could not acquire the fan-out gate: %v", err)
	}
	_ = cur.Close()
}

// siblingDispatch is siblingCount concurrent statements of a shape's
// calibrated query, dispatched through a data-shard client over the
// Distributed tables. probeRoutedSiblings and calibration's
// siblingsAtCall share it, so calibration measures the very dispatch
// the probe cancels.
type siblingDispatch struct {
	client  *chclient.Client
	ids     []string
	cursors []chclient.Cursor
	cancel  context.CancelFunc
	start   time.Time
}

// dispatchSiblings dispatches siblingCount statements of shape's calibrated
// query; scenario names their query ids.
func (s *server) dispatchSiblings(ctx context.Context, t *testing.T, shape cancelShape, scenario string) siblingDispatch {
	t.Helper()
	s.awaitMergesIdle(ctx, t)
	d := siblingDispatch{
		client: s.client(t, adminUser, adminPassword, chclient.Config{
			Database:       shardedDB,
			DataShardCount: shardCount,
			MaxOpenConns:   shardedPoolConns,
			MaxIdleConns:   shardedPoolConns,
		}),
		ids:     make([]string, siblingCount),
		cursors: make([]chclient.Cursor, siblingCount),
	}
	dr := dryRunShape(ctx, t, shape)
	dispatchCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.start = time.Now()
	for i := range d.cursors {
		d.ids[i] = cancelQueryID(shape, fmt.Sprintf("%s%d", scenario, i))
		cur, err := d.client.QueryCursor(chclient.WithQueryID(dispatchCtx, d.ids[i]), dr.SQL, dr.Args...)
		if err != nil {
			cancel()
			t.Fatalf("dispatch %s %d: %v", scenario, i, err)
		}
		d.cursors[i] = cur
	}
	return d
}

// siblingsInCall waits until every sibling of d is inside shape's call and
// returns each one's readings, the last of them taken after the wait for the
// slowest, so it shows the sibling as it is at the cancellation that follows.
func (s *server) siblingsInCall(ctx context.Context, t *testing.T, shape cancelShape, d siblingDispatch) map[string][]callProgress {
	t.Helper()
	seen := make(map[string][]callProgress, len(d.ids))
	for _, id := range d.ids {
		seen[id] = s.waitInCall(ctx, t, shape, id)
	}
	for _, id := range d.ids {
		p, running := s.callProgress(ctx, t, id)
		if !running {
			t.Fatalf("sibling %s finished its %s call before the last sibling reached its own", id, shape.function)
		}
		seen[id] = append(seen[id], p)
	}
	return seen
}

// cancelAndClose cancels d's dispatch, closes every sibling's cursor within
// closeBudget, and returns the instant of the cancellation.
func (d siblingDispatch) cancelAndClose(t *testing.T) time.Time {
	t.Helper()
	cancelAt := time.Now()
	d.cancel()
	var wg sync.WaitGroup
	for _, cur := range d.cursors {
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
		t.Fatalf("closing the cancelled siblings %v did not return within %s", d.ids, closeBudget)
	}
	return cancelAt
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
// lands inside the query's CPU-bound call rather than in work ahead of it. It
// returns the readings, oldest first; the last shows the query as it is now.
func (s *server) waitInCall(ctx context.Context, t *testing.T, shape cancelShape, qid string) []callProgress {
	t.Helper()
	deadline := time.Now().Add(runningBudget)
	var readings []callProgress
	for {
		if p, running := s.callProgress(ctx, t, qid); running {
			if readings = append(readings, p); shape.inCall(readings) {
				return readings
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

// assertServerWorkEnds runs the moment cerberus has released the capacity a
// cancelled dispatch held — its admission slot or its fan-out gate weight.
// seen holds each statement's readings up to the cancellation at cancelAt,
// and held is how much of the time up to cancelAt the server spent paused,
// which no statement ran in.
//
// On a build that interrupts the call, KILL QUERY ... SYNC confirmed the work
// dead before the release, so no statement for ids — initiator or remote
// child — may still run. Either way the work must end on its own within
// naturalRunBudget. The verdict on how much of the call the server evaluated
// after the cancellation is read from the server's own accounting and
// anchored on the shape's natural run, not on a fixed threshold, in the
// shape's meter: the probe first requires the cancellation to have left at
// least the floor the meter judges, failing diagnosably when calibration
// could not reach it on the substrate, and then requires the work performed
// after it to be separated from what was left (see separated).
func (s *server) assertServerWorkEnds(ctx context.Context, t *testing.T, ids []string, want cancelExpectation, cancelAt time.Time,
	held time.Duration, seen map[string][]callProgress,
) {
	t.Helper()
	if want.bounded {
		if running := s.runningCount(ctx, t, ids); running != 0 {
			t.Errorf("%d statement(s) for %v still run once their capacity was released", running, ids)
		}
	}
	if !s.goneWithin(ctx, t, ids, naturalRunBudget) {
		t.Fatalf("server work for %v still running after %s", ids, naturalRunBudget)
	}
	switch want.meter {
	case wallClock:
		s.assertWallClockSeparation(ctx, t, ids, want, cancelAt, held)
	case loopExecutions:
		for _, id := range ids {
			s.assertLoopSeparation(ctx, t, id, want, seen[id])
		}
	}
}

// assertWallClockSeparation judges ids under wallClock: remaining is how much
// of the natural run was left at cancelAt, counting only the time up to it
// that the server was not held paused, and the delay is how long after it
// query_log shows the last statement ending.
func (s *server) assertWallClockSeparation(ctx context.Context, t *testing.T, ids []string, want cancelExpectation, cancelAt time.Time,
	held time.Duration,
) {
	t.Helper()
	started, ended := s.workSpan(ctx, t, ids)
	remaining := want.natural - (cancelAt.Sub(started) - held)
	delay := ended.Sub(cancelAt)
	t.Logf("server work for %v: natural %s, %s left at the cancellation (held paused %s), ended %s after it", ids,
		want.natural, remaining, held, delay)
	if remaining < minRemaining {
		t.Fatalf("only %s of the shape's %s natural run was left at the cancellation; the probe needs at least %s "+
			"to separate an interrupted call from an uninterrupted one — raise the shape's maxSize for this substrate",
			remaining, want.natural, minRemaining)
	}
	ok, bound := separated(want.bounded, float64(delay), float64(remaining))
	switch {
	case ok:
	case want.bounded:
		t.Errorf("server work for %v ended %s after the cancellation; want within %s (a quarter of the remaining work) "+
			"on a build that interrupts the call", ids, delay, time.Duration(bound))
	default:
		t.Errorf("server work for %v ended %s after the cancellation; want at least %s (half the remaining work) on a build "+
			"that cannot interrupt the call", ids, delay, time.Duration(bound))
	}
}

// assertLoopSeparation judges the statement id under loopExecutions: left is
// how many of the natural run's loop executions it still had to perform when
// it was last read before the cancellation, and after is how many it
// performed from that reading to its end, by its query_log row.
func (s *server) assertLoopSeparation(ctx context.Context, t *testing.T, id string, want cancelExpectation, seen []callProgress) {
	t.Helper()
	j, err := want.judgeLoop(seen, s.finalExecutions(ctx, t, id))
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	t.Logf("server work for %s: natural loop %d executions, %d left at the cancellation, %d performed after it",
		id, want.loopExecutions, j.left, j.after)
	switch {
	case j.ok:
	case want.bounded:
		t.Errorf("%s performed %d loop executions after the cancellation; want at most %.0f (a quarter of the %d left) "+
			"on a build that interrupts the call", id, j.after, j.bound, j.left)
	default:
		t.Errorf("%s performed %d loop executions after the cancellation; want at least %.0f (half of the %d left) on a "+
			"build that cannot interrupt the call", id, j.after, j.bound, j.left)
	}
}

// finalExecutions is the FunctionExecute count query_log reports for the
// finished statement id.
func (s *server) finalExecutions(ctx context.Context, t *testing.T, id string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var n, rows uint64
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(ProfileEvents['FunctionExecute']), count() FROM system.query_log WHERE query_id = ? AND type != 'QueryStart'", id).
		Scan(&n, &rows)
	if err != nil {
		t.Fatalf("read query_log for %s: %v", id, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no terminal row for %s", id)
	}
	return n
}

// awaitMergesIdle waits until the server runs no background merge, before a
// probe dispatches a statement whose progress it times. Every probe's verdict
// flushes the system logs (flushLogs), and once a log table has gathered
// enough parts the server merges them: on cancelProbeNanoCPUs's half CPU such
// a merge, started just before the next dispatch, slows that statement's
// work ahead of its call, and an inCall predicate that compares how long
// that work stood still with how long the read took (regexCallRunning) then
// reports the call entered while the statement is still ahead of it. A
// dispatch on an idle server meets the same server every probe calibrated
// against. The merges are the server's own small system-table merges; one
// that outlasts naturalRunBudget fails the probe.
func (s *server) awaitMergesIdle(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(naturalRunBudget)
	for {
		var merges uint64
		if err := s.admin.Conn().QueryRow(ctx, "SELECT count() FROM system.merges").Scan(&merges); err != nil {
			t.Fatalf("read system.merges: %v", err)
		}
		if merges == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d background merge(s) still running after %s", merges, naturalRunBudget)
		}
		time.Sleep(pollInterval)
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

// runShape dispatches shape's emitted SQL as qid on the administrative
// connection, uncancelled, and returns a channel that delivers the outcome
// once every row has been read.
func (s *server) runShape(ctx context.Context, t *testing.T, shape cancelShape, qid string) <-chan error {
	t.Helper()
	s.awaitMergesIdle(ctx, t)
	dr := dryRunShape(ctx, t, shape)
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
	return done
}

// naturalRun runs shape's emitted SQL to completion, uncancelled, and returns
// its profile.
func (s *server) naturalRun(ctx context.Context, t *testing.T, shape cancelShape) naturalProfile {
	t.Helper()
	qid := cancelQueryID(shape, "natural")
	done := s.runShape(ctx, t, shape, qid)
	var (
		readings []callProgress
		inCall   bool
		prof     naturalProfile
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
					prof.entered = p.elapsed
				}
			}
		}
	}
	started, ended := s.workSpan(ctx, t, []string{qid})
	prof.natural = ended.Sub(started)
	if !inCall {
		prof.entered = prof.natural
		return prof
	}
	if shape.meter == loopExecutions {
		base, _ := foldLoopBaseline(readings)
		prof.loopExecutions = s.finalExecutions(ctx, t, qid) - base
	}
	return prof
}

// warmShape brings the server to the state in which it runs shape's
// expression for calibration and for every probe after it. ClickHouse
// JIT-compiles an expression once it has seen it
// min_count_to_compile_expression times, and the compiled code runs the same
// work in fewer function executions and less time: array_fold's lambda
// performs half the executions once compiled. Calibration that measured a run
// before the compilation would judge every later, compiled probe against a
// run that was not its own. warmShape runs shape over a seed of warmSize
// until the server has seen it min_count_to_compile_expression times and then
// warmSteadyRuns more, and requires those last runs to agree on their
// function-execution count: the state every later run of the expression
// shares, whatever the seed's size. The JIT cache is keyed by the expression,
// not by the series a query reads or the table it reads it through, so the
// routed siblings' Distributed statements share it.
func (s *server) warmShape(ctx context.Context, t *testing.T, shape cancelShape) {
	t.Helper()
	var threshold uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT toUInt64(value) FROM system.settings WHERE name = 'min_count_to_compile_expression'").Scan(&threshold); err != nil {
		t.Fatalf("read min_count_to_compile_expression: %v", err)
	}
	metric := shape.name + "_warm"
	s.exec(ctx, t, shape.seedSQL(metric, shape.warmSize))
	shape.query = shape.queryFor(metric)
	var counts []uint64
	for range threshold + warmSteadyRuns {
		qid := cancelQueryID(shape, "warm")
		if err := <-s.runShape(ctx, t, shape, qid); err != nil {
			t.Fatalf("warm run of %s: %v", shape.query, err)
		}
		counts = append(counts, s.finalExecutions(ctx, t, qid))
	}
	steady := counts[threshold:]
	t.Logf("warm-up: %s at size %d performed %v function executions over %d runs", shape.name, shape.warmSize, counts, len(counts))
	for _, n := range steady {
		if n != steady[0] {
			t.Fatalf("%s's runs past the server's compile threshold (%d) still disagree on their function executions (%v); "+
				"no run can stand for another's work", shape.name, threshold, counts)
		}
	}
}

// warmSteadyRuns is how many runs past the compile threshold warmShape
// compares: two are the fewest that can agree.
const warmSteadyRuns = 2

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

// calibrateShape sizes shape's seed to the substrate. It first warms the
// server (warmShape), then seeds a series at the shape's baseSize, measures
// the uncancelled run, and re-seeds larger in proportion to the shortfall
// until the work left after the latest cancellation a probe makes clears the
// calibration target in the shape's meter, or the size reaches maxSize, for
// at most calibrationRounds rounds. That latest cancellation is the
// routed-sibling probe's, and the work left for it is measured by actually
// running that probe's own dispatch (siblingsAtCall), not estimated from the
// lone run: two statements sharing cancelProbeNanoCPUs's one throttled CPU
// neither reach the call nor advance through it at the lone run's pace. A
// single siblingsAtCall measurement is itself noisy under contention, so each
// round takes calibrationSiblingSamples measurements and keeps the worst (the
// one leaving the least work), the same way a single slow sample would sink
// the real probe. It returns the shape with its calibrated query and that
// query's natural profile. A run still short of the target at maxSize is
// returned as is: the scenario assertions then fail with the separation they
// could not get, rather than judging a probe that cannot tell the two
// outcomes apart.
func (s *server) calibrateShape(ctx context.Context, t *testing.T, shape cancelShape) (cancelShape, naturalProfile) {
	t.Helper()
	s.warmShape(ctx, t, shape)
	size := shape.baseSize
	var prof naturalProfile
	for round := range calibrationRounds {
		metric := fmt.Sprintf("%s_probe_%d", shape.name, size)
		s.exec(ctx, t, shape.seedSQL(metric, size))
		local := shardedDB + ".otel_metrics_gauge" + ddl.DataShardLocalSuffix
		s.exec(ctx, t, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s.otel_metrics_gauge WHERE MetricName = ?", local, serverDB), metric)
		shape.query = shape.queryFor(metric)
		prof = s.naturalRun(ctx, t, shape)
		want := cancelExpectation{meter: shape.meter, naturalProfile: prof}
		margin := math.Inf(1)
		for sample := range calibrationSiblingSamples {
			m := want.calibrationMargin(s.siblingsAtCall(ctx, t, shape))
			t.Logf("calibration round %d sample %d: %s at size %d siblings left %.2f of the target", round, sample, shape.name, size, m)
			margin = min(margin, m)
		}
		t.Logf("calibration round %d: %s at size %d ran %s, lone entered after %s, loop %d executions, "+
			"worst of %d sibling samples left %.2f of the target", round, shape.name, size, prof.natural, prof.entered,
			prof.loopExecutions, calibrationSiblingSamples, margin)
		if margin >= 1 || size >= shape.maxSize {
			break
		}
		next := size * calibrationGrowth
		if margin > 0 {
			next = int(float64(size) / margin * calibrationOvershoot)
		}
		size = min(max(next, size+1), shape.maxSize)
	}
	return shape, prof
}

// calibrationMargin is the work the routed siblings' cancellation leaves, as
// a fraction of the calibration target in w's meter: under wallClock the
// natural run less the time the slowest sibling took to reach the call,
// against calibrationTarget; under loopExecutions the share of the loop the
// sibling furthest through it still had ahead, against calibrationLoopShare.
func (w cancelExpectation) calibrationMargin(elapsed time.Duration, seen map[string][]callProgress) float64 {
	if w.meter == wallClock {
		return float64(w.natural-elapsed) / float64(calibrationTarget)
	}
	if w.loopExecutions == 0 {
		return 0
	}
	margin := math.Inf(1)
	for _, readings := range seen {
		left, _ := w.loopLeft(readings)
		margin = min(margin, float64(left)/float64(w.loopExecutions)/calibrationLoopShare)
	}
	return margin
}

// siblingsAtCall performs the routed-sibling probe's dispatch of shape's
// calibrated query up to the instant the probe cancels it, and returns how
// long, from dispatch, the last sibling took to reach the call, with every
// sibling's readings at that instant. The dispatch is then cancelled and its
// siblings closed, so calibration leaves no statement running past this call.
func (s *server) siblingsAtCall(ctx context.Context, t *testing.T, shape cancelShape) (time.Duration, map[string][]callProgress) {
	t.Helper()
	d := s.dispatchSiblings(ctx, t, shape, "calibsibling")
	defer d.cancel()
	seen := s.siblingsInCall(ctx, t, shape, d)
	elapsed := time.Since(d.start)
	d.cancelAndClose(t)
	if !s.goneWithin(ctx, t, d.ids, naturalRunBudget) {
		t.Fatalf("calibration siblings for %v still running after %s", d.ids, naturalRunBudget)
	}
	return elapsed, seen
}

// calibrationGrowth multiplies a seed whose run left no work to judge, which
// gives no proportion to scale by.
const calibrationGrowth = 2

// calibrationOvershoot scales a re-seed a little past the proportional size,
// since a shape's run is not exactly linear in its size.
const calibrationOvershoot = 1.2

// calibrationSiblingSamples is how many siblingsAtCall measurements
// calibrateShape takes per round, keeping the worst. Repeated measurements at
// the same size were observed to disagree by several seconds under
// contention, one leaving 11.3 s and the very next only 2.4 s.
const calibrationSiblingSamples = 3
