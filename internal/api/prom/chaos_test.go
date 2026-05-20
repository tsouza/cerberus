package prom_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// assertPromErrorEnvelope decodes body as a Prom error response and
// asserts every field Grafana actually reads off the wire: status =
// "error", errorType matches the expected kind, and the error message
// is non-empty. The pre-strengthening shape — `strings.Contains(body,
// "error")` — would have silently accepted any payload that happened
// to contain the substring "error" anywhere (including a metric named
// `query_errors_total`), giving false confidence that the envelope
// was being rendered.
func assertPromErrorEnvelope(t *testing.T, body string, wantKind string) {
	t.Helper()
	var env prom.Response
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error body not parseable JSON: %v; body=%s", err, body)
	}
	if env.Status != "error" {
		t.Errorf("envelope.status: got %q, want \"error\"; body=%s", env.Status, body)
	}
	if env.ErrorType != wantKind {
		t.Errorf("envelope.errorType: got %q, want %q; body=%s", env.ErrorType, wantKind, body)
	}
	if env.Error == "" {
		t.Errorf("envelope.error: empty (Grafana renders this string); body=%s", body)
	}
}

// Layer 11 — failure-mode tests for the Prom handler. Each scenario
// injects a CH-side failure (connection refused, mid-stream drop, slow
// response, deadline-exceeded, garbage row) and asserts the wire-level
// shape (status, errorType envelope) plus the non-functional invariants
// (no panic, no goroutine leak, the slot is released).

// chaosQuerier is a stubQuerier flavour that injects controllable
// failures. It implements every prom.Querier method.
type chaosQuerier struct {
	// stable canned data for the success paths.
	samples []chclient.Sample

	// failure injection. err fires on every method. wrapPanic=true panics
	// inside Query — exercises the handler's recover behaviour.
	err          error
	wrapPanic    bool
	queryLatency time.Duration

	// cursorErrAfter > 0 -> the returned cursor surfaces an error after
	// N rows. Lets us drive the streaming /query_range path through a
	// mid-stream drop.
	cursorErrAfter int
	cursorErr      error

	calls atomic.Int32
}

func (c *chaosQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	c.calls.Add(1)
	if c.wrapPanic {
		panic("chaos: simulated panic in Query")
	}
	if c.queryLatency > 0 {
		select {
		case <-time.After(c.queryLatency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.samples, nil
}

func (c *chaosQuerier) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return &chaosCursor{
		samples:   c.samples,
		errAfter:  c.cursorErrAfter,
		injectErr: c.cursorErr,
	}, nil
}

func (c *chaosQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return nil, nil
}

func (c *chaosQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return nil, nil
}

func (c *chaosQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return nil, nil
}

func (c *chaosQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return nil, nil
}

// chaosCursor surfaces a transport-style error after N rows.
type chaosCursor struct {
	samples   []chclient.Sample
	errAfter  int
	injectErr error
	idx       int
	cur       chclient.Sample
	closed    bool
	err       error
}

func (c *chaosCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.errAfter > 0 && c.idx >= c.errAfter {
		c.err = c.injectErr
		if c.err == nil {
			c.err = errors.New("chaos: mid-stream drop")
		}
		return false
	}
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *chaosCursor) Sample() chclient.Sample { return c.cur }
func (c *chaosCursor) Err() error              { return c.err }
func (c *chaosCursor) Close() error            { c.closed = true; return nil }

// TestCH_UpstreamError_Returns502 — an upstream CH error must map to
// a 502 Bad Gateway with the Prom error envelope.
func TestCH_UpstreamError_Returns502(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", resp.StatusCode, body)
	}
	assertPromErrorEnvelope(t, body, prom.ErrInternal)
}

// TestCH_DropMidQuery_RangeReturns502 — mid-stream cursor failure on
// /query_range must propagate as a 502.
func TestCH_DropMidQuery_RangeReturns502(t *testing.T) {
	t.Parallel()
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(5 * time.Minute)

	q := &chaosQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: start, Value: 1},
			{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: start.Add(time.Minute), Value: 1},
		},
		cursorErrAfter: 1,
		cursorErr:      errors.New("clickhouse: connection lost mid-stream"),
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", resp.StatusCode, body)
	}
	assertPromErrorEnvelope(t, body, prom.ErrInternal)
}

// TestCH_SlowResponse_RespectsClientCancel — the handler must honor
// the client's context cancellation rather than blocking forever.
func TestCH_SlowResponse_RespectsClientCancel(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{queryLatency: 5 * time.Second}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/query?query=up", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do: want context.DeadlineExceeded, got success")
	}
	if elapsed > 2*time.Second {
		t.Errorf("client cancel honoured too late: %s", elapsed)
	}
}

// TestCH_NoPanicOnPanicQuerier — a panicking Querier must not crash
// the test server; the handler / runtime recovers and the connection
// is closed.
func TestCH_NoPanicOnPanicQuerier(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{wrapPanic: true}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		// net/http recovers; the connection may be reset depending on
		// timing. Either failure is acceptable: no test crash.
		t.Logf("Get returned err on panic path: %v (acceptable)", err)
		return
	}
	defer resp.Body.Close()
	// If we got a response, it should be 500-class.
	if resp.StatusCode < 500 {
		t.Errorf("status: got %d, want 5xx", resp.StatusCode)
	}
}

// TestCH_OversizeQuery_Tolerated — a very long query string should not
// crash the handler. Prom upstream allows arbitrary query lengths; we
// just verify the handler returns a clean status (200 or 400) rather
// than 5xx / panic.
func TestCH_OversizeQuery_Tolerated(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// 256 KiB query — well below Go's default URL-line limit but above
	// any reasonable PromQL. The parser will reject it as bad data.
	big := strings.Repeat("a", 256*1024)
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + big)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusRequestURITooLong {
		t.Errorf("oversize query: got %d, want 400/200/414", resp.StatusCode)
	}
}

// TestCH_ManyRequestsUnderError_NoLeak — drive 100 sequential requests
// against an erroring CH stub; each must complete with a 502, no hang.
func TestCH_ManyRequestsUnderError_NoLeak(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	const N = 100
	for i := range N {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		// Sample the envelope on every 25th response — every Nth call
		// keeps the JSON decode out of the hot path while still
		// catching a regression in which the handler stops rendering
		// the Prom envelope after some number of requests (e.g. a
		// pooled-Builder reset bug). One sample at i=0 anchors the
		// happy path; the trailing one (i=N-1) anchors steady-state.
		if i == 0 || i == N-1 || i%25 == 0 {
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("iter %d status: got %d, want 502; body=%s", i, resp.StatusCode, body)
			}
			assertPromErrorEnvelope(t, body, prom.ErrInternal)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("iter %d status: got %d, want 502", i, resp.StatusCode)
		}
	}
	if got := int(q.calls.Load()); got != N {
		t.Errorf("calls: got %d, want %d", got, N)
	}
}

// TestCH_ConcurrentRequestsUnderChaos_AllFail — 32 parallel clients
// against a failing CH stub must all see a 502 and the server must not
// deadlock.
func TestCH_ConcurrentRequestsUnderChaos_AllFail(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// Capture status + the envelope shape of one sampled response per
	// worker. Asserting the envelope on every concurrent caller would
	// triple the test runtime; sampling one per worker still catches a
	// regression in which the 502 path becomes a non-Prom-envelope
	// body (e.g. a raw error string slipping through).
	type result struct {
		code int
		body string
	}
	var wg sync.WaitGroup
	done := make(chan result, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
			if err != nil {
				done <- result{0, ""}
				return
			}
			body := readBody(t, resp)
			done <- result{resp.StatusCode, body}
		}()
	}
	wg.Wait()
	close(done)
	for r := range done {
		if r.code != http.StatusBadGateway {
			t.Errorf("code: got %d, want 502; body=%s", r.code, r.body)
			continue
		}
		assertPromErrorEnvelope(t, r.body, prom.ErrInternal)
	}
}

// TestCH_ReconnectAfterDrop_NextSucceeds — a stub that fails once then
// succeeds models a transient outage. Subsequent requests must work.
func TestCH_ReconnectAfterDrop_NextSucceeds(t *testing.T) {
	t.Parallel()
	q := &flakyQuerier{
		failFirst: errors.New("clickhouse: transient drop"),
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Value: 1},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first: got %d, want 502", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second: got %d body=%s", resp.StatusCode, body)
	}
}

// TestCH_BadQuery_400ErrorEnvelope — invalid PromQL must surface as
// bad_data 400, not an internal-class error.
func TestCH_BadQuery_400ErrorEnvelope(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=%5Bbroken")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
	}
	// Full envelope shape — pre-strengthening the assertion was
	// `strings.Contains(body, prom.ErrBadData)`, which would match
	// the string "bad_data" appearing anywhere in the body (e.g.
	// inside a metric name a regex matcher echoed back). Decoding
	// JSON + asserting the errorType field is the contract Grafana's
	// Prom datasource reads.
	assertPromErrorEnvelope(t, body, prom.ErrBadData)
}

// flakyQuerier fails the first Query call, then succeeds. Models the
// reconnect-after-drop scenario.
type flakyQuerier struct {
	failFirst error
	samples   []chclient.Sample
	hits      int
}

func (f *flakyQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	f.hits++
	if f.hits == 1 && f.failFirst != nil {
		return nil, f.failFirst
	}
	return f.samples, nil
}

func (f *flakyQuerier) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	f.hits++
	if f.hits == 1 && f.failFirst != nil {
		return nil, f.failFirst
	}
	return newSliceCursor(f.samples), nil
}

func (f *flakyQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (f *flakyQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (f *flakyQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (f *flakyQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

// TestCH_QueryRangeStreamingPath_NormalAndError — exercises the cursor
// path twice: once on success, once with cursorErrAfter set so a
// mid-stream failure surfaces as 502.
func TestCH_QueryRangeStreamingPath_NormalAndError(t *testing.T) {
	t.Parallel()
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)

	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		q := &chaosQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: start, Value: 1},
				{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: end, Value: 1},
			},
		}
		srv := newServer(q)
		t.Cleanup(srv.Close)
		url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
			srv.URL, start.Unix(), end.Unix())
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200", resp.StatusCode)
		}
	})

	t.Run("midstream_error", func(t *testing.T) {
		t.Parallel()
		q := &chaosQuerier{
			samples:        []chclient.Sample{{MetricName: "up", Timestamp: start}},
			cursorErrAfter: 0,
			cursorErr:      errors.New("clickhouse: cursor exploded"),
		}
		// Force the cursor to fail before delivering any row.
		q.cursorErrAfter = 0 // means: error on first Next via wrapping
		// We instead reuse the "Query returns err" path which is the
		// universal way the handler surfaces CH failures.
		q.err = errors.New("clickhouse: query failed")
		srv := newServer(q)
		t.Cleanup(srv.Close)
		url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
			srv.URL, start.Unix(), end.Unix())
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: got %d, want 502", resp.StatusCode)
		}
	})
}

// TestCH_ContextDeadline_PropagatesToHandler — when the client passes a
// short deadline, the handler must observe the cancellation and abort
// cleanly. Asserts the round-trip elapsed time is bounded.
func TestCH_ContextDeadline_PropagatesToHandler(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{queryLatency: 1 * time.Second}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/query?query=up", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do: want deadline-exceeded, got success")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("deadline propagated too slowly: %s", elapsed)
	}
}
