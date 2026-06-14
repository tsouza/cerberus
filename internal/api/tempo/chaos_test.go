package tempo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — failure-mode tests for the Tempo handler.

type chaosQuerier struct {
	samples      []chclient.Sample
	strings      []string
	err          error
	wrapPanic    bool
	queryLatency time.Duration
	calls        atomic.Int32
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

func (c *chaosQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return c.strings, nil
}

var _ tempo.Querier = (*chaosQuerier)(nil)

// assertTempoErrorEnvelope decodes body as Tempo's distinct error shape
// (`{traceID, spanID, error, message}`) and asserts error=true with a
// non-empty message — the fields Grafana's Tempo datasource reads.
func assertTempoErrorEnvelope(t *testing.T, body string) {
	t.Helper()
	var env tempo.ErrorResponse
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error body not parseable JSON: %v; body=%s", err, body)
	}
	if !env.Error {
		t.Errorf("envelope.error: got false, want true; body=%s", body)
	}
	if env.Message == "" {
		t.Errorf("envelope.message: empty (Grafana renders this string); body=%s", body)
	}
}

// TestTempoCH_PanicQuerier_Returns500Envelope — RC1 robustness contract
// for the Tempo head, mirroring the Prom + Loki panic tests. A panicking
// Querier must NOT drop the TCP connection: the shared
// telemetry.QueryMiddleware recover path renders a clean Tempo 500 error
// envelope and counts the failed query, instead of unwinding through
// net/http and leaving a bare conn-reset.
func TestTempoCH_PanicQuerier_Returns500Envelope(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{wrapPanic: true}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
	if err != nil {
		t.Fatalf("GET: connection dropped on panic (want clean 500): %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", resp.StatusCode, body)
	}
	assertTempoErrorEnvelope(t, body)
}

// TestTempoCH_SearchUpstreamError_Returns502 — CH-side failure on
// /api/search must surface as 502 with the Tempo error envelope.
func TestTempoCH_SearchUpstreamError_Returns502(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", resp.StatusCode, body)
	}
	// Tempo envelope: {traceID, spanID, error, message}.
	if !strings.Contains(body, "message") {
		t.Errorf("body missing tempo envelope: %s", body)
	}
}

// TestTempoCH_TraceByID_UpstreamError_Returns502 — same for /api/traces/{id}.
func TestTempoCH_TraceByID_UpstreamError_Returns502(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: read failure")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/0123456789abcdef")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
}

// TestTempoCH_SearchTags_UpstreamError — /api/search/tags maps a CH
// failure to a 5xx with the Tempo envelope.
func TestTempoCH_SearchTags_UpstreamError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("search/tags under CH chaos: got %d, want 5xx", resp.StatusCode)
	}
}

// TestTempoCH_SearchTagValues_UpstreamError — same for tag values.
func TestTempoCH_SearchTagValues_UpstreamError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("tag values under CH chaos: got %d, want 5xx", resp.StatusCode)
	}
}

// TestTempoCH_BadQuery_400 — unparseable TraceQL on /api/search must
// surface 400, not 5xx.
func TestTempoCH_BadQuery_400(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// `{` is incomplete and should fail the parser.
	resp, err := http.Get(srv.URL + `/api/search?q=%7B`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
	}
}

// TestTempoCH_ContextCancel_HonoredOnSearch — short client deadline
// drives a fast abort.
func TestTempoCH_ContextCancel_HonoredOnSearch(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{queryLatency: 3 * time.Second}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+`/api/search?q=%7B%7D`, nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do: want canceled, got success")
	}
	if elapsed > 1*time.Second {
		t.Errorf("cancel honoured too slow: %s", elapsed)
	}
}

// TestTempoCH_ConcurrentChaos_AllFail — parallel requests under a
// failing CH stub must all see 5xx.
func TestTempoCH_ConcurrentChaos_AllFail(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
			if err != nil {
				codes <- 0
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c < 500 {
			t.Errorf("code: got %d, want 5xx", c)
		}
	}
}

// TestTempoCH_ManyErrorRequests_NoLeak — sequential pressure under CH
// chaos completes cleanly.
func TestTempoCH_ManyErrorRequests_NoLeak(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	const N = 100
	for range N {
		resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: got %d, want 502", resp.StatusCode)
		}
	}
}

// TestTempoCH_EchoEndpoint_NotBlockedByCHError — /api/echo doesn't touch
// CH, so a failing CH stub must not affect it.
func TestTempoCH_EchoEndpoint_NotBlockedByCHError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("CH dead")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || body != "echo" {
		t.Fatalf("echo: status=%d body=%q", resp.StatusCode, body)
	}
}
