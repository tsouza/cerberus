package loki_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestHandler_LineFormat_AppliesTemplate — a `| line_format` query
// over the handler should return stream values with the template
// applied. Pins the wiring through handleQuery → execute →
// buildInstantData → toStreamsWithTransform → postProcessExtract.
//
// The unit test in post_process_test.go covers the transform
// shape; this test pins the integration with the HTTP layer + the
// stub Querier (no real CH).
func TestHandler_LineFormat_AppliesTemplate(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "hello world", Labels: map[string]string{"job": "api"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL, `{job="api"} | line_format "[{{.job}}] {{__line__}}"`)
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if len(streams[0].Values) != 1 {
		t.Fatalf("expected 1 value, got %d", len(streams[0].Values))
	}
	got := streams[0].Values[0][1]
	want := "[api] hello world"
	if got != want {
		t.Errorf("line value: got %q, want %q", got, want)
	}
}

// TestHandler_Decolorize_StripsAnsi — `| decolorize` should strip
// the ANSI CSI codes from the underlying log line before it hits the
// stream Values array.
func TestHandler_Decolorize_StripsAnsi(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "\x1b[31mERROR\x1b[0m: connect refused",
				Labels:     map[string]string{"job": "api"},
				Timestamp:  ts,
			},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL, `{job="api"} | decolorize`)
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected stream shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	want := "ERROR: connect refused"
	if got != want {
		t.Errorf("decolorize output: got %q, want %q", got, want)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("ANSI ESC not stripped: %q", got)
	}
}

// TestHandler_DecolorizeThenLineFormat_Composes — the two stages
// compose left-to-right: decolorize first cleans the line, then
// line_format wraps the cleaned text. The composition path is what
// breaks if the toStreamsWithTransform iteration order regresses.
func TestHandler_DecolorizeThenLineFormat_Composes(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "\x1b[31mERROR\x1b[0m: oops",
				Labels:     map[string]string{"job": "api"},
				Timestamp:  ts,
			},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL,
		`{job="api"} | decolorize | line_format "[{{.job}}] {{__line__}}"`)
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected stream shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	want := "[api] ERROR: oops"
	if got != want {
		t.Errorf("compose output: got %q, want %q", got, want)
	}
}

// TestHandler_LineFormat_BadTemplate_400 — a malformed Go-template
// surfaces as a 400 (bad data) rather than a silent no-op. Without
// this, a typo'd template would render every line as the raw input
// — confusing for the user.
func TestHandler_LineFormat_BadTemplate_400(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	url := srv.URL + "/loki/api/v1/query?query=" +
		urlEncode(`{job="api"} | line_format "{{ .unclosed"`)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-2xx for malformed template, got 200")
	}
}

// TestHandler_NoLineFormat_PassThrough — a query with no post-fetch
// stages goes through the identity path. Regression test for the
// nil-transform branch in toStreamsWithTransform.
func TestHandler_NoLineFormat_PassThrough(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "untouched line", Labels: map[string]string{"job": "api"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL, `{job="api"} |= "untouched"`)
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected stream shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	if got != "untouched line" {
		t.Errorf("expected pass-through; got %q", got)
	}
}

// getStreams issues a Loki instant query against srv and decodes the
// response into the per-stream Values shape. Keeps each test focused
// on the assertions, not the JSON-decoding boilerplate.
func getStreams(t *testing.T, srvURL, query string) []loki.Stream {
	t.Helper()

	resp, err := http.Get(srvURL + "/loki/api/v1/query?query=" + urlEncode(query))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var parsed queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Data.ResultType != "streams" {
		t.Fatalf("resultType: got %q, want streams", parsed.Data.ResultType)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	return streams
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}
