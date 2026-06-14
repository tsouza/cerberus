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

// TestHandler_LineFormat_WithTemplateFuncs — verifies the line_format
// template surface includes the cerberus FuncMap (lower / upper /
// regexReplaceAll / etc). Pinning these via a handler-integration test
// catches the wiring through newLineFormatStep — a regression that
// removed `templateFuncs()` from the funcmap would silently strip
// these funcs and crash with "function not defined" at query time.
func TestHandler_LineFormat_WithTemplateFuncs(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "HELLO World", Labels: map[string]string{"job": "API"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL,
		"{job=\"API\"} | line_format `[{{ lower .job }}] {{ upper __line__ }}`")
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	want := "[api] HELLO WORLD"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestHandler_LineFormat_SprigPipeline_NowExecutes pins the
// deliberate-subset reversal: templates that used the full Loki funcmap
// (sprig math/encoding, Loki-native bytes/duration/align) previously
// failed at text/template PARSE time with "function ... not defined",
// which handler.go wrapped as a 400. They now parse + execute, so
// getStreams (which fails on any non-200) proves the 400→200 closure
// AND the rendered output matches Loki.
func TestHandler_LineFormat_SprigPipeline_NowExecutes(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		expr string
		line string
		want string
	}{
		{
			name: "int|add",
			expr: "{job=\"api\"} | line_format `{{ .count | int | add 1 }}`",
			line: "x",
			want: "42", // .count = "41"
		},
		{
			name: "b64enc",
			expr: "{job=\"api\"} | line_format `{{ b64enc __line__ }}`",
			line: "hello",
			want: "aGVsbG8=",
		},
		{
			name: "bytes_div",
			expr: "{job=\"api\"} | line_format `{{ div (bytes \"2KB\") 1000 }}`",
			line: "x",
			want: "2",
		},
		{
			name: "duration_seconds",
			expr: "{job=\"api\"} | line_format `{{ duration_seconds \"90s\" }}`",
			line: "x",
			want: "90",
		},
		{
			name: "alignRight",
			expr: "{job=\"api\"} | line_format `{{ alignRight 6 __line__ }}`",
			line: "hi",
			want: "    hi",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{
				samples: []chclient.Sample{
					{MetricName: tc.line, Labels: map[string]string{"job": "api", "count": "41"}, Timestamp: ts},
				},
			}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			streams := getStreams(t, srv.URL, tc.expr)
			if len(streams) != 1 || len(streams[0].Values) != 1 {
				t.Fatalf("unexpected shape: %+v", streams)
			}
			if got := streams[0].Values[0][1]; got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestHandler_LineFormat_Timestamp_RendersRealValue pins the
// __timestamp__ fix end-to-end: previously hardwired to func() string {
// return "" } (always blank), it now returns the sample's timestamp as
// a time.Time so `{{ __timestamp__ | date "..." }}` formats the real
// value threaded through toStreamsWithTransform.
func TestHandler_LineFormat_Timestamp_RendersRealValue(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "x", Labels: map[string]string{"job": "api"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL,
		"{job=\"api\"} | line_format `{{ __timestamp__ | date \"2006-01-02T15:04:05Z07:00\" }}`")
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	want := ts.Format("2006-01-02T15:04:05Z07:00")
	if got != want {
		t.Errorf("__timestamp__ render: got %q, want %q (must NOT be blank)", got, want)
	}
	if got == "" {
		t.Error("__timestamp__ rendered blank — the old hardwired stub regressed")
	}
}

// TestHandler_LabelFormat_GroupsByOutputLabels — when `| label_format`
// renames a label, the streams response must group rows by the
// POST-format label set. Without this, the rename would be visible in
// streams[*].stream but the canonical key would still use the old
// labels — splitting what should be one stream into two.
func TestHandler_LabelFormat_GroupsByOutputLabels(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "row 1", Labels: map[string]string{"job": "api"}, Timestamp: ts},
			{MetricName: "row 2", Labels: map[string]string{"job": "api"}, Timestamp: ts.Add(time.Second)},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL, `{job="api"} | label_format svc=job`)
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream (post-format key), got %d: %+v", len(streams), streams)
	}
	got := streams[0].Stream
	if got["svc"] != "api" {
		t.Errorf("expected svc=api in renamed labels; got %v", got)
	}
	if _, ok := got["job"]; ok {
		t.Errorf("expected job to be removed by rename; got %v", got)
	}
	if len(streams[0].Values) != 2 {
		t.Errorf("expected 2 values rolled into one stream, got %d", len(streams[0].Values))
	}
}

// TestHandler_LabelFormat_TemplateThenLineFormat — composition:
// label_format sets a templated label, then line_format references
// the new label. Pins that the line_format template sees the
// post-format dot map.
func TestHandler_LabelFormat_TemplateThenLineFormat(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "boom", Labels: map[string]string{"job": "api", "severity": "ERROR"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	streams := getStreams(t, srv.URL,
		"{job=\"api\"} | label_format lvl=`{{.severity}}` | line_format `[{{.lvl}}] {{__line__}}`")
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("unexpected shape: %+v", streams)
	}
	got := streams[0].Values[0][1]
	if got != "[ERROR] boom" {
		t.Errorf("line: got %q, want %q", got, "[ERROR] boom")
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
