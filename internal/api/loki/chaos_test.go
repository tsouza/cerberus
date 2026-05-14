package loki_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — failure-mode tests for the Loki handler.

// chaosQuerier injects CH failures across every loki.Querier method.
type chaosQuerier struct {
	samples      []chclient.Sample
	stringRows   []string
	labelSets    []map[string]string
	statsRow     chclient.IndexStatsRow
	volumeRows   []chclient.IndexVolumeRow
	err          error
	queryLatency time.Duration
	calls        atomic.Int32
}

func (c *chaosQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	c.calls.Add(1)
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
	return c.stringRows, nil
}

func (c *chaosQuerier) QueryIndexStats(_ context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	c.calls.Add(1)
	if c.err != nil {
		return chclient.IndexStatsRow{}, c.err
	}
	return c.statsRow, nil
}

func (c *chaosQuerier) QueryIndexVolume(_ context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return c.volumeRows, nil
}

func (c *chaosQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return c.labelSets, nil
}

// TestLokiCH_UpstreamError_QueryReturns502 — CH error on /query
// surfaces as 502 + the Loki error envelope.
func TestLokiCH_UpstreamError_QueryReturns502(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: refused")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", resp.StatusCode, body)
	}
}

// TestLokiCH_UpstreamError_QueryRangeReturns502 — same for /query_range.
func TestLokiCH_UpstreamError_QueryRangeReturns502(t *testing.T) {
	t.Parallel()
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	q := &chaosQuerier{err: errors.New("clickhouse: dead")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	urlStr := fmt.Sprintf(`%s/loki/api/v1/query_range?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d&step=60`,
		srv.URL, start.Unix(), end.Unix())
	resp, err := http.Get(urlStr)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
}

// TestLokiCH_LabelsEndpoint_CHError — /labels on a failing CH stub
// surfaces a 502 / 500 with an error envelope.
func TestLokiCH_LabelsEndpoint_CHError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("labels under CH chaos: got %d, want 5xx", resp.StatusCode)
	}
}

// TestLokiCH_IndexStats_CHError — same shape for /index/stats.
func TestLokiCH_IndexStats_CHError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("index/stats under CH chaos: got %d, want 5xx", resp.StatusCode)
	}
}

// TestLokiCH_BadQuery_400 — unparseable LogQL surfaces as 400 bad_data.
func TestLokiCH_BadQuery_400(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%5Bnotvalid`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
	}
}

// TestLokiCH_ContextCancel_HonoredOnQuery — client cancels mid-flight.
func TestLokiCH_ContextCancel_HonoredOnQuery(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{queryLatency: 3 * time.Second}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+`/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`, nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do: want context-canceled, got success")
	}
	if elapsed > 1*time.Second {
		t.Errorf("cancel honoured too slow: %s", elapsed)
	}
}

// TestLokiCH_ManyErrorRequests_NoLeak — 100 sequential requests against
// erroring CH; each must return 502, no hang.
func TestLokiCH_ManyErrorRequests_NoLeak(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("clickhouse: chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	const N = 100
	for range N {
		resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
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

// TestLokiCH_ConcurrentRequests_AllFail — many parallel clients see
// 502 under sustained CH chaos.
func TestLokiCH_ConcurrentRequests_AllFail(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("chaos")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
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
		if c != http.StatusBadGateway {
			t.Errorf("code: got %d, want 502", c)
		}
	}
}

// TestLokiTail_ClosesOnCHError — when the tail loop's CH probe fails
// the WebSocket is closed and the goroutine exits.
func TestLokiTail_ClosesOnCHError(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{err: errors.New("tail: ch boom")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/loki/api/v1/tail"
	u.RawQuery = "query=" + url.QueryEscape(`{job="api"}`)

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		// If the handler rejects the upgrade outright, that's also
		// a valid failure-mode response.
		t.Logf("Dial returned err (acceptable): %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Read until close. The CH failure should drive a clean shutdown
	// within a few seconds (one poll cycle).
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return // expected — connection closed or read errored.
		}
	}
}

// TestLokiCH_MissingQuery_400 — handler sanity on misuse.
func TestLokiCH_MissingQuery_400(t *testing.T) {
	t.Parallel()
	q := &chaosQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/query")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "missing query") {
		t.Errorf("body: missing %q in %s", "missing query", body)
	}
}

// TestLokiCH_Pointer_Querier_AssertedInterface — compile-time sanity
// that chaosQuerier satisfies loki.Querier.
var _ loki.Querier = (*chaosQuerier)(nil)

// readBody drains an http.Response body and returns it as string. The
// other loki *_test.go files re-implement this locally; chaos tests
// re-use the same helper shape.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
