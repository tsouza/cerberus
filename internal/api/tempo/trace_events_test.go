package tempo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	tracev1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// exceptionEventsJSON is the traceByIDEventsJSON value ClickHouse renders
// for a span carrying one `exception` event, the shape an OTel SDK's
// RecordError writes.
const exceptionEventsJSON = `[["1791820507237898700","exception",` +
	`{"exception.message":"not found","exception.type":"*errors.fundamental"}]]`

// followsFromLinksJSON is the traceByIDLinksJSON value for a span with one
// link to a span in another trace.
const followsFromLinksJSON = `[["0af7651916cd43dd8448eb211c80319c","b7ad6b7169203331","rojo=00f067aa0ba902b7",{"link.kind":"follows_from"}]]`

// erroredSpanSample is one trace-by-id row for an errored span that carries
// a status message, an exception event and a link.
func erroredSpanSample() chclient.Sample {
	return chclient.Sample{
		MetricName: "ReadLogWithIdempotencyKey",
		Timestamp:  time.Unix(0, 1791820507237898700),
		Value:      22463014,
		Labels: map[string]string{
			"service.name":            "ledger",
			traceByIDKeyTraceID:       "59b562309f731a1122b0835e45d0d17d",
			traceByIDKeySpanID:        "da3c362b496f7b3d",
			traceByIDKeyParentSpanID:  "94a4bdb40eba85bd",
			traceByIDKeySpanKind:      "Internal",
			traceByIDKeyStatusCode:    "Error",
			traceByIDKeyStatusMessage: "not found",
			traceByIDKeySpanAttrsJSON: "{}",
			traceByIDKeyEventsJSON:    exceptionEventsJSON,
			traceByIDKeyLinksJSON:     followsFromLinksJSON,
			traceByIDKeyScopeName:     "ledger",
			traceByIDKeyScopeVersion:  "",
		},
	}
}

// TestTraceByIDProjections_EmitsStatusMessageEventsLinks pins the SQL the
// trace-by-id wrap projection reads for the three span fields Grafana's
// trace view needs to show why a span failed: the status message, the
// span events (where SDKs record exceptions) and the span links.
func TestTraceByIDProjections_EmitsStatusMessageEventsLinks(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	plan := &chplan.Project{Input: &chplan.Scan{Table: s.SpansTable}, Projections: traceByIDProjections(s)}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	for _, want := range []string{
		"toString(`StatusMessage`)",
		"toJSONString(arrayZip(arrayMap(t -> toString(toUnixTimestamp64Nano(t)), `Events`.`Timestamp`), `Events`.`Name`, `Events`.`Attributes`))",
		"toJSONString(arrayZip(`Links`.`TraceId`, `Links`.`SpanId`, `Links`.`TraceState`, `Links`.`Attributes`))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("trace-by-id SQL missing %s\nSQL:\n%s", want, sql)
		}
	}
	for _, key := range []string{traceByIDKeyStatusMessage, traceByIDKeyEventsJSON, traceByIDKeyLinksJSON} {
		if !argsContain(args, key) {
			t.Errorf("trace-by-id SQL args missing reserved key %q; args: %v", key, args)
		}
	}
}

// TestTraceByIDProjections_OmitsUndeclaredOptionalColumns pins that a
// custom schema without status-message / events / links columns emits no
// reference to them, rather than a query ClickHouse would reject.
func TestTraceByIDProjections_OmitsUndeclaredOptionalColumns(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	s.StatusMessageColumn = ""
	s.EventsColumn = ""
	s.LinksColumn = ""
	plan := &chplan.Project{Input: &chplan.Scan{Table: s.SpansTable}, Projections: traceByIDProjections(s)}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	for _, unwanted := range []string{"StatusMessage", "`Events`", "`Links`"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("trace-by-id SQL references undeclared column %s\nSQL:\n%s", unwanted, sql)
		}
	}
	for _, key := range []string{traceByIDKeyStatusMessage, traceByIDKeyEventsJSON, traceByIDKeyLinksJSON} {
		if argsContain(args, key) {
			t.Errorf("trace-by-id SQL args carry reserved key %q for an undeclared column; args: %v", key, args)
		}
	}
}

// kvString returns the string value of key in an OTLP attribute list, or
// "" when absent.
func kvString(kvs []*commonv1.KeyValue, key string) string {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func argsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestDecodeSpanEvents pins the decode of ClickHouse's
// toJSONString(arrayZip(...)) rendering, including the defensive drops.
func TestDecodeSpanEvents(t *testing.T) {
	t.Parallel()

	got := decodeSpanEvents(exceptionEventsJSON)
	if len(got) != 1 {
		t.Fatalf("decodeSpanEvents: got %d events, want 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.TimeUnixNano != 1791820507237898700 {
		t.Errorf("TimeUnixNano = %d, want 1791820507237898700 (exact, no float rounding)", ev.TimeUnixNano)
	}
	if ev.Name != "exception" {
		t.Errorf("Name = %q, want exception", ev.Name)
	}
	if ev.Attributes["exception.message"] != "not found" {
		t.Errorf("Attributes[exception.message] = %q, want %q", ev.Attributes["exception.message"], "not found")
	}

	for name, raw := range map[string]string{
		"empty string":       "",
		"no events":          "[]",
		"malformed json":     "[[",
		"wrong arity":        `[["1","exception"]]`,
		"non-numeric time":   `[["soon","exception",{}]]`,
		"object not a tuple": `[{"1":"1","2":"exception","3":{}}]`,
	} {
		if got := decodeSpanEvents(raw); got != nil {
			t.Errorf("%s: decodeSpanEvents(%q) = %+v, want nil", name, raw, got)
		}
	}

	mixed := `[["5","good",{}],["bad"]]`
	if got := decodeSpanEvents(mixed); len(got) != 1 || got[0].Name != "good" {
		t.Errorf("a malformed tuple must drop only itself; got %+v", got)
	}
}

// TestDecodeSpanLinks pins the links decode, including the defensive drops.
func TestDecodeSpanLinks(t *testing.T) {
	t.Parallel()

	got := decodeSpanLinks(followsFromLinksJSON)
	if len(got) != 1 {
		t.Fatalf("decodeSpanLinks: got %d links, want 1: %+v", len(got), got)
	}
	want := spanLink{
		TraceID:    "0af7651916cd43dd8448eb211c80319c",
		SpanID:     "b7ad6b7169203331",
		TraceState: "rojo=00f067aa0ba902b7",
		Attributes: map[string]string{"link.kind": "follows_from"},
	}
	if got[0].TraceID != want.TraceID || got[0].SpanID != want.SpanID ||
		got[0].TraceState != want.TraceState || got[0].Attributes["link.kind"] != "follows_from" {
		t.Errorf("decodeSpanLinks = %+v, want %+v", got[0], want)
	}
	for _, raw := range []string{"", "[]", "{", `[["a","b"]]`} {
		if got := decodeSpanLinks(raw); got != nil {
			t.Errorf("decodeSpanLinks(%q) = %+v, want nil", raw, got)
		}
	}
}

// TestGroupBatchesProto_CarriesStatusMessageEventsLinks pins the proto
// path Grafana's Tempo datasource consumes on /api/v2/traces/{id}.
func TestGroupBatchesProto_CarriesStatusMessageEventsLinks(t *testing.T) {
	t.Parallel()

	tr := groupBatchesProto([]chclient.Sample{erroredSpanSample()})
	if len(tr.ResourceSpans) != 1 || len(tr.ResourceSpans[0].ScopeSpans) != 1 ||
		len(tr.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("expected exactly one span, got %+v", tr)
	}
	span := tr.ResourceSpans[0].ScopeSpans[0].Spans[0]

	if span.Status.GetCode() != tracev1.Status_STATUS_CODE_ERROR {
		t.Errorf("status code = %v, want STATUS_CODE_ERROR", span.Status.GetCode())
	}
	if span.Status.GetMessage() != "not found" {
		t.Errorf("status message = %q, want %q", span.Status.GetMessage(), "not found")
	}

	if len(span.Events) != 1 {
		t.Fatalf("events = %+v, want one exception event", span.Events)
	}
	ev := span.Events[0]
	if ev.Name != "exception" || ev.TimeUnixNano != 1791820507237898700 {
		t.Errorf("event = {name %q, time %d}, want {exception, 1791820507237898700}", ev.Name, ev.TimeUnixNano)
	}
	if v := kvString(ev.Attributes, "exception.message"); v != "not found" {
		t.Errorf("event exception.message = %q, want %q", v, "not found")
	}

	if len(span.Links) != 1 {
		t.Fatalf("links = %+v, want one link", span.Links)
	}
	link := span.Links[0]
	if len(link.TraceId) != traceIDByteLen || len(link.SpanId) != spanIDByteLen {
		t.Errorf("link IDs must decode to %d/%d raw bytes, got %d/%d",
			traceIDByteLen, spanIDByteLen, len(link.TraceId), len(link.SpanId))
	}
	if link.TraceState != "rojo=00f067aa0ba902b7" {
		t.Errorf("link trace state = %q", link.TraceState)
	}
	if v := kvString(link.Attributes, "link.kind"); v != "follows_from" {
		t.Errorf("link attribute link.kind = %q, want follows_from", v)
	}
}

// TestGroupBatches_CarriesStatusMessageEventsLinks pins the v1 JSON path,
// which must expose the same fields as the proto path.
func TestGroupBatches_CarriesStatusMessageEventsLinks(t *testing.T) {
	t.Parallel()

	batches := groupBatches([]chclient.Sample{erroredSpanSample()})
	if len(batches) != 1 || len(batches[0].Spans) != 1 {
		t.Fatalf("expected exactly one span, got %+v", batches)
	}
	span := batches[0].Spans[0]
	if span.Status.Message != "not found" {
		t.Errorf("status message = %q, want %q", span.Status.Message, "not found")
	}
	if len(span.Events) != 1 || span.Events[0].Name != "exception" ||
		span.Events[0].TimeUnixNano != "1791820507237898700" ||
		span.Events[0].Attributes["exception.message"] != "not found" {
		t.Errorf("events = %+v, want one exception event", span.Events)
	}
	if len(span.Links) != 1 || span.Links[0].SpanID != "b7ad6b7169203331" {
		t.Errorf("links = %+v, want one link", span.Links)
	}

	// The reserved keys must not leak into the resource attributes.
	for k := range batches[0].Resource.Attributes {
		if strings.HasPrefix(k, "__cerberus_") {
			t.Errorf("reserved key %q leaked into resource attributes", k)
		}
	}

	// A span with no events or links omits both fields on the wire.
	plain := erroredSpanSample()
	plain.Labels[traceByIDKeyEventsJSON] = "[]"
	plain.Labels[traceByIDKeyLinksJSON] = "[]"
	body, err := json.Marshal(groupBatches([]chclient.Sample{plain}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"events"`) || strings.Contains(string(body), `"links"`) {
		t.Errorf("empty events/links must be omitted from the JSON; got %s", body)
	}
}
