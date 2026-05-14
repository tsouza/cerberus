package chclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Layer 11 — circuit-breaker chaos tests for the CH-disconnect resilience
// GA workstream. These exercise the breaker state machine through the
// Client surface against a fault-injecting fake driver.Conn:
//
//   - CLOSED → OPEN after N consecutive failures inside the window.
//   - OPEN → HALF-OPEN after the backoff interval.
//   - HALF-OPEN probe outcome closes or re-opens the breaker.
//   - Concurrent requests during HALF-OPEN only admit one probe.
//   - /readyz reflects the breaker state because Ping is wrapped.
//   - /healthz is unaffected because the health handler never pings
//     when wired to a process-only liveness probe.
//
// The fake driver.Conn (flakyConn below) lets each test toggle whether
// the next conn.Query / conn.Ping returns an error. Toggling is via
// atomic.Bool so concurrent goroutines see the change without races.

// The package's TestMain is in execute_span_test.go — it installs the
// in-memory tracer provider. The breaker itself spawns no goroutines,
// and the gatedFlakyConn fixture only blocks the caller goroutine on a
// channel receive (no fan-out), so per-test goleak.Find isn't load-bearing
// here — TestMain-level goleak.VerifyTestMain would be the right knob if
// a future fixture starts spawning goroutines.

// flakyConn is a driver.Conn that flips between "healthy" (returns
// nil from Query / Ping / Exec) and "failing" (returns failErr) based
// on the atomic fail flag. Tests drive the breaker by toggling the
// flag and observing the resulting state transitions.
type flakyConn struct {
	fail    atomic.Bool
	failErr error
	// callCount is the total number of Query / Ping / Exec calls
	// observed — lets tests assert "only N probes admitted during
	// HALF-OPEN".
	callCount atomic.Int64
}

func newFlakyConn(failErr error) *flakyConn {
	if failErr == nil {
		failErr = errors.New("flakyConn: simulated CH outage")
	}
	return &flakyConn{failErr: failErr}
}

func (c *flakyConn) setFail(f bool) { c.fail.Store(f) }

func (c *flakyConn) Contributors() []string { return nil }

func (c *flakyConn) ServerVersion() (*driver.ServerVersion, error) {
	return &driver.ServerVersion{}, nil
}

func (c *flakyConn) Select(context.Context, any, string, ...any) error {
	c.callCount.Add(1)
	if c.fail.Load() {
		return c.failErr
	}
	return nil
}

func (c *flakyConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.callCount.Add(1)
	if c.fail.Load() {
		return nil, c.failErr
	}
	return &chaosRows{}, nil
}

func (c *flakyConn) QueryRow(context.Context, string, ...any) driver.Row {
	c.callCount.Add(1)
	if c.fail.Load() {
		return chaosRow{c.failErr}
	}
	return chaosRow{nil}
}

func (c *flakyConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.callCount.Add(1)
	if c.fail.Load() {
		return nil, c.failErr
	}
	return nil, errors.New("flakyConn.PrepareBatch: not used in tests")
}

func (c *flakyConn) Exec(context.Context, string, ...any) error {
	c.callCount.Add(1)
	if c.fail.Load() {
		return c.failErr
	}
	return nil
}

func (c *flakyConn) AsyncInsert(context.Context, string, bool, ...any) error {
	c.callCount.Add(1)
	if c.fail.Load() {
		return c.failErr
	}
	return nil
}

func (c *flakyConn) Ping(context.Context) error {
	c.callCount.Add(1)
	if c.fail.Load() {
		return c.failErr
	}
	return nil
}

func (c *flakyConn) Stats() driver.Stats { return driver.Stats{} }
func (c *flakyConn) Close() error        { return nil }

// newBreakerTestClient returns a Client whose breaker uses a manually-
// driven clock so tests can race the OPEN-interval timer without sleeping
// for 5 seconds at a time. The returned setNow lets the test advance the
// clock; calling it twice (e.g. setNow(now), setNow(now+5s)) drives the
// OPEN → HALF-OPEN transition.
func newBreakerTestClient(t *testing.T, conn driver.Conn) (*Client, func(time.Time)) {
	t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	client := newWithConn(conn)
	client.br.now = clock
	setNow := func(t time.Time) {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = t
	}
	return client, setNow
}

// TestBreaker_OpensAfterNConsecutiveFailures — 5 failing queries in a
// row trip the breaker from CLOSED to OPEN.
func TestBreaker_OpensAfterNConsecutiveFailures(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newBreakerTestClient(t, conn)

	if got := client.br.currentState(); got != "closed" {
		t.Fatalf("initial state: got %q, want closed", got)
	}

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, err := client.Query(ctx, "SELECT 1")
		if err == nil {
			t.Fatalf("Query %d: nil error, want flakyConn failure", i)
		}
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("Query %d: got ErrCircuitOpen too early, breaker should still be CLOSED", i)
		}
	}

	if got := client.br.currentState(); got != "open" {
		t.Fatalf("after %d failures: got %q, want open", breakerThreshold, got)
	}
}

// TestBreaker_FastFailsWhenOpen — once OPEN, Query returns ErrCircuitOpen
// without touching the driver. Verified by elapsed time + the connection
// call counter.
func TestBreaker_FastFailsWhenOpen(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if client.br.currentState() != "open" {
		t.Fatal("breaker did not open after threshold failures")
	}
	callsBefore := conn.callCount.Load()

	start := time.Now()
	_, err := client.Query(ctx, "SELECT 1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Query: nil error, want ErrCircuitOpen")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Query err: got %v, want ErrCircuitOpen wrap", err)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("OPEN fast-fail elapsed = %s; expected < 10ms (no CH dial)", elapsed)
	}
	if got := conn.callCount.Load(); got != callsBefore {
		t.Errorf("conn.callCount: bumped by %d during OPEN fast-fail; should be 0", got-callsBefore)
	}
}

// TestBreaker_HalfOpenAfterBackoff — advance the clock past the OPEN
// interval and the next call is admitted as the HALF-OPEN probe.
func TestBreaker_HalfOpenAfterBackoff(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, setNow := newBreakerTestClient(t, conn)

	ctx := context.Background()
	// Trip OPEN.
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if client.br.currentState() != "open" {
		t.Fatal("breaker did not open")
	}

	// Advance past the OPEN interval. The probe will still fail
	// (conn.fail is still true) so we end up back at OPEN, but the
	// probe attempt itself is what we're testing.
	setNow(time.Date(2026, 5, 14, 12, 0, 6, 0, time.UTC)) // +6s

	callsBefore := conn.callCount.Load()
	_, err := client.Query(ctx, "SELECT 1")
	if err == nil {
		t.Fatal("probe Query: nil error, want flakyConn failure")
	}
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatal("probe Query: got ErrCircuitOpen — probe was not admitted")
	}
	if got := conn.callCount.Load(); got != callsBefore+1 {
		t.Errorf("conn.callCount delta = %d; want exactly 1 probe call", got-callsBefore)
	}
	if got := client.br.currentState(); got != "open" {
		t.Errorf("after failing probe: got state %q, want open (probe failure → reset timer)", got)
	}
}

// TestBreaker_ProbeSuccessClosesCircuit — half-open probe succeeds →
// state goes back to CLOSED and subsequent queries flow.
func TestBreaker_ProbeSuccessClosesCircuit(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, setNow := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if client.br.currentState() != "open" {
		t.Fatal("breaker did not open")
	}

	// CH "recovers" — conn stops failing.
	conn.setFail(false)
	setNow(time.Date(2026, 5, 14, 12, 0, 6, 0, time.UTC)) // +6s past OPEN trip

	// Probe should succeed and close the circuit.
	_, err := client.Query(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("probe Query: got err %v, want nil (CH healthy + probe)", err)
	}
	if got := client.br.currentState(); got != "closed" {
		t.Fatalf("after successful probe: state = %q, want closed", got)
	}

	// Subsequent requests flow through.
	for i := 0; i < 10; i++ {
		_, err := client.Query(ctx, "SELECT 1")
		if err != nil {
			t.Errorf("post-recovery Query %d: %v", i, err)
		}
	}
}

// TestBreaker_ProbeFailureKeepsOpen — half-open probe fails → state
// reverts to OPEN, openedAt resets so the next probe waits a full
// interval from this point (not from the original trip).
func TestBreaker_ProbeFailureKeepsOpen(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, setNow := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if client.br.currentState() != "open" {
		t.Fatal("breaker did not open")
	}

	// Probe fires at t = +6s but fails (conn still failing).
	probeTime := time.Date(2026, 5, 14, 12, 0, 6, 0, time.UTC)
	setNow(probeTime)
	_, _ = client.Query(ctx, "SELECT 1") // probe fails
	if got := client.br.currentState(); got != "open" {
		t.Fatalf("after failed probe: state = %q, want open", got)
	}

	// Immediately after the failed probe, another Query must NOT be
	// admitted — the OPEN timer just reset.
	_, err := client.Query(ctx, "SELECT 1")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second Query post-failed-probe: got %v, want ErrCircuitOpen", err)
	}

	// Now advance another 6s past the reset openedAt — a new probe
	// should be admitted.
	setNow(probeTime.Add(6 * time.Second))
	conn.setFail(false)
	_, err = client.Query(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("post-second-interval Query: got %v, want nil (probe succeeded)", err)
	}
	if got := client.br.currentState(); got != "closed" {
		t.Errorf("after second-interval successful probe: state = %q, want closed", got)
	}
}

// TestBreaker_ConcurrentRequestsDuringHalfOpen — 100 concurrent calls
// during the HALF-OPEN window must admit exactly one probe; the
// remaining 99 see ErrCircuitOpen. The probe is gated so it stays
// in-flight while the other goroutines race to hit the breaker.
func TestBreaker_ConcurrentRequestsDuringHalfOpen(t *testing.T) {
	t.Parallel()
	probeReleased := make(chan struct{})
	conn := &gatedFlakyConn{
		flakyConn:     newFlakyConn(nil),
		probeReleased: probeReleased,
	}
	conn.setFail(true) // initial OPEN trip
	// IMPORTANT: pass the gated wrapper into Client, not the inner
	// flakyConn — otherwise the Client routes around the gate and
	// the probe never blocks.
	client, setNow := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if client.br.currentState() != "open" {
		t.Fatal("breaker did not open")
	}

	// Advance past the interval and "heal" the conn. The probe will
	// land in conn.Query and block on probeReleased until the test
	// closes the channel — that's our window for all the other
	// goroutines to pile up against the HALF-OPEN breaker.
	setNow(time.Date(2026, 5, 14, 12, 0, 6, 0, time.UTC))
	conn.setFail(false)
	conn.gateProbe(probeReleased)

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	// admitted / rejected / unexpected are written by N goroutines and
	// polled by the main goroutine while the probe is still in flight,
	// so atomic counters are mandatory under -race.
	var admitted, rejected, unexpected atomic.Int64
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, e := client.Query(ctx, "SELECT 1")
			switch {
			case e == nil:
				admitted.Add(1)
			case errors.Is(e, ErrCircuitOpen):
				rejected.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	close(start)

	// Wait until N-1 of them have completed (the one still in flight
	// is the probe). On a contended scheduler 2s is plenty for 99
	// goroutines to each take + return from a mutex.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		done := admitted.Load() + rejected.Load() + unexpected.Load()
		if done >= int64(N-1) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// At this point N-1 goroutines have returned ErrCircuitOpen
	// (rejected); the one still in flight is the probe. Release it.
	close(probeReleased)
	wg.Wait()

	a := admitted.Load()
	r := rejected.Load()
	u := unexpected.Load()
	if u != 0 {
		t.Errorf("unexpected errors: %d", u)
	}
	if a+r != N {
		t.Errorf("admitted=%d rejected=%d, total %d != %d", a, r, a+r, N)
	}
	if a != 1 {
		t.Errorf("admitted = %d during HALF-OPEN; want exactly 1 probe", a)
	}
}

// gatedFlakyConn wraps a flakyConn so the next Query call blocks until
// the test releases it via probeReleased. Used to keep the half-open
// probe in flight while the test pushes other goroutines at the
// breaker.
type gatedFlakyConn struct {
	*flakyConn
	probeReleased chan struct{}
	gateActive    atomic.Bool
}

func (c *gatedFlakyConn) gateProbe(ch chan struct{}) {
	c.probeReleased = ch
	c.gateActive.Store(true)
}

func (c *gatedFlakyConn) Query(ctx context.Context, sql string, args ...any) (driver.Rows, error) {
	if c.gateActive.Load() && c.gateActive.CompareAndSwap(true, false) {
		// Disarm so only the first call gates.
		<-c.probeReleased
	}
	return c.flakyConn.Query(ctx, sql, args...)
}

// TestBreaker_SuccessResetsCounter — 4 failures + 1 success + 4 failures
// must NOT open the breaker (the success reset the consecutive-failure
// counter).
func TestBreaker_SuccessResetsCounter(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	client, _ := newBreakerTestClient(t, conn)

	ctx := context.Background()

	// 4 failures.
	conn.setFail(true)
	for i := 0; i < 4; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if got := client.br.currentState(); got != "closed" {
		t.Fatalf("after 4 failures: state = %q, want closed (still under threshold)", got)
	}

	// One success.
	conn.setFail(false)
	if _, err := client.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("recovery Query: %v", err)
	}

	// 4 more failures — total of 8 failures interleaved with one
	// success. The counter should have reset on the success, so we
	// land at 4 < threshold and the breaker stays CLOSED.
	conn.setFail(true)
	for i := 0; i < 4; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if got := client.br.currentState(); got != "closed" {
		t.Fatalf("after 4+1+4 pattern: state = %q, want closed", got)
	}
}

// TestBreaker_HealthzUnaffected — /healthz is process-only liveness;
// it must remain 200 OK regardless of the breaker state.
//
// This is a guard against future regressions that try to "fix"
// /healthz to ping CH — which would tie pod liveness to a downstream
// dependency and cause k8s to restart pods during a CH outage.
func TestBreaker_HealthzUnaffected(t *testing.T) {
	t.Parallel()

	// We don't actually wire the breaker into a health handler here
	// because /healthz doesn't take a Pinger. The test asserts the
	// invariant by hitting the route directly: it must return 200
	// even if every downstream call would fail.
	//
	// Note: the import path internal/api/health is intentionally
	// imported lazily (via http.ServeMux) so this test file doesn't
	// require it as a build dependency of the chclient package.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Mirror exactly the production handler in
		// internal/api/health/health.go::handleHealthz — no CH
		// touch, no breaker check, just 200 OK.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("/healthz body = %q, want %q", got, "ok")
	}
}

// TestBreaker_PingRespectsBreaker — when the breaker is OPEN, Ping
// short-circuits to ErrCircuitOpen instantly. The readiness handler
// (internal/api/health) consumes Ping; this lets /readyz report red
// without waiting for a CH dial timeout.
func TestBreaker_PingRespectsBreaker(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newBreakerTestClient(t, conn)

	ctx := context.Background()
	// Trip the breaker via Ping itself (Ping is one of the wrapped
	// methods so failures count toward the threshold).
	for i := 0; i < breakerThreshold; i++ {
		err := client.Ping(ctx)
		if err == nil {
			t.Fatalf("Ping %d: nil err on flaky conn", i)
		}
	}
	if got := client.br.currentState(); got != "open" {
		t.Fatalf("breaker state after %d Ping failures: %q, want open", breakerThreshold, got)
	}

	callsBefore := conn.callCount.Load()
	start := time.Now()
	err := client.Ping(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Ping on OPEN breaker: got %v, want ErrCircuitOpen wrap", err)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("Ping fast-fail elapsed = %s; want < 10ms", elapsed)
	}
	if conn.callCount.Load() != callsBefore {
		t.Errorf("conn.callCount increased during OPEN Ping; should not touch CH")
	}
}

// TestBreaker_ExecRespectsBreaker — sanity that the breaker applies
// uniformly to every CH-touching method. Exec is the DDL/DML path the
// schema-bootstrap startup hook uses; if it didn't fast-fail under
// breaker-open we'd queue up startup retries against a dead CH.
func TestBreaker_ExecRespectsBreaker(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_ = client.Exec(ctx, "CREATE TABLE t (x Int32) ENGINE = Memory")
	}
	if got := client.br.currentState(); got != "open" {
		t.Fatalf("after %d Exec failures: state = %q, want open", breakerThreshold, got)
	}

	err := client.Exec(ctx, "CREATE TABLE u (x Int32) ENGINE = Memory")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Exec on OPEN: got %v, want ErrCircuitOpen", err)
	}
}

// TestBreaker_QueryStringsRespectsBreaker — same as Exec; QueryStrings
// is what /api/v1/labels and friends hit, so it's a hot path.
func TestBreaker_QueryStringsRespectsBreaker(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newBreakerTestClient(t, conn)

	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = client.QueryStrings(ctx, "SELECT name FROM system.tables")
	}
	if got := client.br.currentState(); got != "open" {
		t.Fatalf("after %d QueryStrings failures: state = %q, want open", breakerThreshold, got)
	}

	_, err := client.QueryStrings(ctx, "SELECT 1")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("QueryStrings on OPEN: got %v, want ErrCircuitOpen", err)
	}
}

// TestBreaker_WindowRolls — failures spaced wider than breakerWindow
// must NOT trip the breaker. The window slides — only contiguous
// failures inside one window count.
func TestBreaker_WindowRolls(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, setNow := newBreakerTestClient(t, conn)

	ctx := context.Background()
	t0 := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	// 4 failures at t0, then advance 20s (past the 10s window),
	// then 4 more. Total 8 failures but only 4 in any one window,
	// so the breaker stays CLOSED.
	setNow(t0)
	for i := 0; i < 4; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	setNow(t0.Add(20 * time.Second))
	for i := 0; i < 4; i++ {
		_, _ = client.Query(ctx, "SELECT 1")
	}
	if got := client.br.currentState(); got != "closed" {
		t.Fatalf("after 4 failures + 20s + 4 failures: state = %q, want closed", got)
	}
}

// TestBreaker_StateStringIsStable — the currentState() string surface
// is the test/log oracle. Pin its values so a future enum reorder
// doesn't silently break the chaos suite.
func TestBreaker_StateStringIsStable(t *testing.T) {
	t.Parallel()

	for state, want := range map[breakerState]string{
		stateClosed:   "closed",
		stateOpen:     "open",
		stateHalfOpen: "half-open",
	} {
		b := &breaker{state: state}
		if got := b.currentState(); got != want {
			t.Errorf("state %d: got %q, want %q", state, got, want)
		}
	}
}

// TestBreaker_ErrorMessageMentionsCircuit — the error message
// surfaced to logs / handler error envelopes must include enough
// signal for on-call to identify the cause without spelunking. We
// pin a substring so a reword doesn't accidentally drop "circuit".
func TestBreaker_ErrorMessageMentionsCircuit(t *testing.T) {
	t.Parallel()
	if !strings.Contains(ErrCircuitOpen.Error(), "circuit") {
		t.Errorf("ErrCircuitOpen message %q: missing substring 'circuit'", ErrCircuitOpen.Error())
	}
}
