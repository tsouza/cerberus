package loki_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tsouza/cerberus/internal/chclient"
)

// tailStubQuerier wraps stubQuerier with a stream of samples surfaced
// over multiple calls to Query so the tail-poll loop sees fresh data
// on each tick. Behaviour mirrors a real ClickHouse + ingest pipeline:
// new rows arrive between polls, prior rows do not re-appear.
type tailStubQuerier struct {
	mu       sync.Mutex
	chunks   [][]chclient.Sample
	callIdx  int
	queries  int32
	lastSQL  string
	lastArgs []any

	// stringRows / labelSets carried so the same querier can serve
	// every loki.Querier method; the tail handler only uses Query.
	stringRows   []string
	labelSets    []map[string]string
	stringsErr   error
	labelSetsErr error

	// /index/stats canned response (not used by tail tests but required
	// by the Querier interface).
	statsRow chclient.IndexStatsRow
	statsErr error

	// /index/volume canned response.
	volumeRows []chclient.IndexVolumeRow
	volumeErr  error
}

func (s *tailStubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt32(&s.queries, 1)
	s.lastSQL = sql
	s.lastArgs = args
	if s.callIdx >= len(s.chunks) {
		return nil, nil
	}
	out := s.chunks[s.callIdx]
	s.callIdx++
	return out, nil
}

func (s *tailStubQuerier) QueryIndexStats(_ context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	return s.statsRow, s.statsErr
}

func (s *tailStubQuerier) QueryIndexVolume(_ context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	return s.volumeRows, s.volumeErr
}

func (s *tailStubQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return s.stringRows, s.stringsErr
}

func (s *tailStubQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return s.labelSets, s.labelSetsErr
}

// dialTail upgrades a synthetic httptest.Server to a WebSocket via the
// /loki/api/v1/tail endpoint. Helper so individual tests stay focused
// on the chunk-shape assertions.
func dialTail(t *testing.T, srv *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/loki/api/v1/tail"
	u.RawQuery = "query=" + url.QueryEscape(query)

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (status=%d)", err, resp.StatusCode)
		}
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestTail_StreamsChunks drives /tail with three poll cycles' worth of
// canned rows and asserts the chunk wire shape.
func TestTail_StreamsChunks(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &tailStubQuerier{
		chunks: [][]chclient.Sample{
			{
				{MetricName: "line-1", Labels: map[string]string{"job": "api"}, Timestamp: ts},
			},
			{
				{MetricName: "line-2", Labels: map[string]string{"job": "api"}, Timestamp: ts.Add(time.Second)},
				{MetricName: "line-3", Labels: map[string]string{"job": "api"}, Timestamp: ts.Add(2 * time.Second)},
			},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{job="api"}`)

	// Read up to two chunks; either may be empty due to ticker drift,
	// but the union of decoded lines must contain all three canned
	// rows.
	got := map[string]struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(got) < 3 {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var chunk struct {
			Streams        []map[string]any `json:"streams"`
			DroppedEntries []any            `json:"dropped_entries"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			t.Fatalf("unmarshal: %v (payload=%s)", err, payload)
		}
		if chunk.DroppedEntries == nil {
			t.Errorf("dropped_entries should be an empty array, not null")
		}
		for _, st := range chunk.Streams {
			rawValues, _ := st["values"].([]any)
			for _, v := range rawValues {
				pair, _ := v.([]any)
				if len(pair) == 2 {
					if line, ok := pair[1].(string); ok {
						got[line] = struct{}{}
					}
				}
			}
		}
	}

	want := []string{"line-1", "line-2", "line-3"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("expected to receive %q across chunks, got=%v", w, keys(got))
		}
	}
}

// TestTail_GracefulClose covers the client-disconnect path: when the
// WebSocket client closes the read side, the polling goroutine must
// exit cleanly (no panics, no goroutine leak).
func TestTail_GracefulClose(t *testing.T) {
	t.Parallel()

	q := &tailStubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{job="api"}`)
	// Force a close frame; the server-side runTailLoop should see the
	// read pump exit and tear down the connection promptly.
	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("write close: %v", err)
	}
	_ = conn.Close()

	// Allow the server time to observe the close. We can't directly
	// assert "no leaked goroutine" here without invasive helpers, but
	// the test exiting cleanly + no panics from runTailLoop covers the
	// happy path; goleak harness lands separately.
	time.Sleep(50 * time.Millisecond)
}

// TestTail_BadInput covers the 4xx validation paths. The server must
// reject misuse before the WebSocket upgrade so clients see plain
// HTTP failures.
func TestTail_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"missing query", `/loki/api/v1/tail`},
		{"bad start", `/loki/api/v1/tail?query=%7Bjob%3D%22api%22%7D&start=banana`},
		{"bad delay_for", `/loki/api/v1/tail?query=%7Bjob%3D%22api%22%7D&delay_for=banana`},
		{"delay_for too large", `/loki/api/v1/tail?query=%7Bjob%3D%22api%22%7D&delay_for=99`},
		{"negative delay_for", `/loki/api/v1/tail?query=%7Bjob%3D%22api%22%7D&delay_for=-1`},
		{"bad limit", `/loki/api/v1/tail?query=%7Bjob%3D%22api%22%7D&limit=banana`},
		{"metric query rejected", `/loki/api/v1/tail?query=rate(%7Bjob%3D%22api%22%7D%5B5m%5D)`},
		{"bad LogQL", `/loki/api/v1/tail?query=not+a+selector`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&tailStubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.raw)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}

// TestTail_SQLShape spins up a tail connection just long enough for the
// querier to receive one poll, then asserts the emitted SQL routes
// through QueryBuilder slots — no fmt.Sprintf cosplay.
func TestTail_SQLShape(t *testing.T) {
	t.Parallel()

	q := &tailStubQuerier{
		// One chunk's worth of empty samples — drives one poll then idles.
		chunks: [][]chclient.Sample{nil},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{job="api"}`)
	defer conn.Close()

	// Wait briefly for at least one Query call.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&q.queries) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&q.queries) == 0 {
		t.Fatalf("no Query call observed within deadline")
	}

	q.mu.Lock()
	sqlStr := q.lastSQL
	q.mu.Unlock()

	for _, frag := range []string{
		"`Body` AS `MetricName`",
		"`ResourceAttributes` AS `Attributes`",
		"`Timestamp` AS `TimeUnix`",
		"toFloat64(?) AS `Value`",
		"ORDER BY `Timestamp`",
		"LIMIT 100",
	} {
		if !strings.Contains(sqlStr, frag) {
			t.Errorf("expected SQL to contain %q, got %q", frag, sqlStr)
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
