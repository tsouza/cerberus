//go:build chdb

// chDB-backed round trip for the trace-by-id status message, span
// events and span links. The unit tests in trace_events_test.go pin the
// projection shape and the decoder against hand-written label maps; this
// test executes the emitted SQL against an embedded ClickHouse with the
// exporter's Nested layout, so it also pins what ClickHouse actually
// returns: toJSONString over arrayZip renders unnamed tuples as
// positional arrays, and a nanosecond event timestamp keeps its full
// precision through the stringified encoding.
package tempo_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestTraceByID_StatusMessageEventsLinks_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String),
    Events Nested (
        Timestamp DateTime64(9),
        Name LowCardinality(String),
        Attributes Map(LowCardinality(String), String)
    ),
    Links Nested (
        TraceId String,
        SpanId String,
        TraceState String,
        Attributes Map(LowCardinality(String), String)
    )
) ENGINE = MergeTree() ORDER BY (Timestamp);`)
	// Span 1 is an errored root carrying one exception event (at a
	// timestamp with nanosecond digits a float64 could not hold) and one
	// link. Span 2 carries neither, so the empty-Nested path is covered.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('c0000000000000000000000000000003', '3000000000000001', '', 'GET /cart', 'Server', 'cart', 1000, toDateTime64('2026-05-01 10:00:00.000000001', 9), 'Error', 'cart is empty', '', '', map(), map('service.name', 'cart'),
        [toDateTime64('2026-05-01 10:00:00.123456789', 9)], ['exception'], [map('exception.message', 'cart is empty', 'exception.type', 'EmptyCart')],
        ['d0000000000000000000000000000004'], ['4000000000000001'], ['k=v'], [map('link.kind', 'follows')]),
    ('c0000000000000000000000000000003', '3000000000000002', '3000000000000001', 'load', 'Internal', 'cart', 500, toDateTime64('2026-05-01 10:00:00.000000002', 9), 'Unset', '', '', '', map(), map('service.name', 'cart'),
        [], [], [], [], [], [], []);`)

	mux := http.NewServeMux()
	tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/c0000000000000000000000000000003")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	var tr tempo.TraceByIDResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}

	spans := map[string]tempo.SpanEntry{}
	for _, b := range tr.Batches {
		for _, sp := range b.Spans {
			spans[sp.SpanID] = sp
		}
	}
	root, ok := spans["3000000000000001"]
	if !ok {
		t.Fatalf("root span missing; body: %s", body)
	}
	child, ok := spans["3000000000000002"]
	if !ok {
		t.Fatalf("child span missing; body: %s", body)
	}

	if root.Status.Message != "cart is empty" {
		t.Errorf("root status.message = %q, want %q", root.Status.Message, "cart is empty")
	}
	// 2026-05-01T10:00:00.123456789Z in Unix nanoseconds.
	const wantEventNanos = "1777629600123456789"
	if len(root.Events) != 1 {
		t.Fatalf("root events = %+v, want exactly one exception event", root.Events)
	}
	ev := root.Events[0]
	if ev.Name != "exception" || ev.TimeUnixNano != wantEventNanos {
		t.Errorf("root event = (%q, %s), want (%q, %s)", ev.Name, ev.TimeUnixNano, "exception", wantEventNanos)
	}
	if ev.Attributes["exception.message"] != "cart is empty" || ev.Attributes["exception.type"] != "EmptyCart" {
		t.Errorf("root event attributes = %v, want exception.message and exception.type", ev.Attributes)
	}
	if len(root.Links) != 1 {
		t.Fatalf("root links = %+v, want exactly one link", root.Links)
	}
	want := tempo.SpanLinkEntry{
		TraceID:    "d0000000000000000000000000000004",
		SpanID:     "4000000000000001",
		TraceState: "k=v",
		Attributes: map[string]string{"link.kind": "follows"},
	}
	l := root.Links[0]
	if l.TraceID != want.TraceID || l.SpanID != want.SpanID || l.TraceState != want.TraceState ||
		len(l.Attributes) != 1 || l.Attributes["link.kind"] != "follows" {
		t.Errorf("root link = %+v, want %+v", l, want)
	}

	if child.Status.Message != "" || len(child.Events) != 0 || len(child.Links) != 0 {
		t.Errorf("child span = status %q, events %+v, links %+v; want all empty",
			child.Status.Message, child.Events, child.Links)
	}
}
