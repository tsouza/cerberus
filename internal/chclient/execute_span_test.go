package chclient

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
)

// executeSpanExporter is the package-wide in-memory exporter installed
// by TestMain — OTel's global tracer-provider delegate is one-shot, so
// a single shared exporter is the only way to keep .Start(...)
// emissions visible across the package's tests.
var executeSpanExporter = tracetest.NewInMemoryExporter()

func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(executeSpanExporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	os.Exit(m.Run())
}

// TestStartExecuteSpan_Attributes pins the attribute contract on the
// execute span chclient stamps on every ClickHouse round-trip. Three
// invariants:
//
//  1. The span name is the canonical cerbtrace.SpanExecute string.
//  2. db.system carries the "clickhouse" semantic-conventions value so
//     APM dashboards filtering on that label catch cerberus's CH calls.
//  3. db.statement carries the SQL (truncated to MaxStatementLen), and
//     cerberus.sql_length the un-truncated byte count.
func TestStartExecuteSpan_Attributes(t *testing.T) {
	executeSpanExporter.Reset()

	sql := "SELECT 1"
	_, span := startExecuteSpan(context.Background(), sql, "clickhouse:9000")
	span.End()

	if err := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}).ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	spans := executeSpanExporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Name != cerbtrace.SpanExecute {
		t.Errorf("span name = %q, want %q", got.Name, cerbtrace.SpanExecute)
	}

	attrs := map[string]string{}
	intAttrs := map[string]int64{}
	for _, a := range got.Attributes {
		switch a.Value.Type() {
		case 0: // INVALID — skip
		default:
			if a.Value.AsString() != "" {
				attrs[string(a.Key)] = a.Value.AsString()
			}
		}
		if a.Key == cerbtrace.AttrSQLLength {
			intAttrs[string(a.Key)] = a.Value.AsInt64()
		}
	}
	if attrs["db.system"] != "clickhouse" {
		t.Errorf("db.system = %q, want \"clickhouse\"", attrs["db.system"])
	}
	if attrs["db.statement"] != sql {
		t.Errorf("db.statement = %q, want %q", attrs["db.statement"], sql)
	}
	if intAttrs[string(cerbtrace.AttrSQLLength)] != int64(len(sql)) {
		t.Errorf("cerberus.sql_length = %d, want %d",
			intAttrs[string(cerbtrace.AttrSQLLength)], len(sql))
	}
}

// TestStartExecuteSpan_Truncation pins the truncation contract: a SQL
// statement longer than MaxStatementLen lands on the span as the
// prefix-plus-ellipsis variant, but cerberus.sql_length reports the
// original byte count so dashboards can still measure cardinality.
func TestStartExecuteSpan_Truncation(t *testing.T) {
	executeSpanExporter.Reset()

	sql := strings.Repeat("a", cerbtrace.MaxStatementLen+50)
	_, span := startExecuteSpan(context.Background(), sql, "clickhouse:9000")
	span.End()
	if err := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}).ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	got := executeSpanExporter.GetSpans()[0]
	var stmt string
	var length int64
	for _, a := range got.Attributes {
		if string(a.Key) == "db.statement" {
			stmt = a.Value.AsString()
		}
		if a.Key == cerbtrace.AttrSQLLength {
			length = a.Value.AsInt64()
		}
	}
	if len(stmt) > cerbtrace.MaxStatementLen {
		t.Errorf("db.statement length = %d, want <= %d", len(stmt), cerbtrace.MaxStatementLen)
	}
	if !strings.HasSuffix(stmt, "…") {
		t.Errorf("db.statement should end with ellipsis when truncated, got %q", stmt[len(stmt)-5:])
	}
	if length != int64(len(sql)) {
		t.Errorf("cerberus.sql_length = %d, want %d (original bytes)", length, len(sql))
	}
}

// TestStartExecuteSpan_ServiceGraphAttributes pins the attribute set
// the OTel-Collector `servicegraph` connector needs to derive the
// cerberus -> clickhouse edge. Three invariants:
//
//  1. Span kind is Client. The servicegraph connector only consults
//     CLIENT-side spans to figure out the "callee" of each edge; an
//     INTERNAL-kind span carrying peer.service is ignored.
//  2. peer.service = "clickhouse". This is the literal value the
//     connector populates the `server` label of
//     `traces_service_graph_request_total{client, server}` with.
//  3. server.address + net.peer.name carry the configured CH addr —
//     present so older dashboards on the v1.20 net.* semconv keys
//     keep rendering after the migration to server.* in v1.21.
func TestStartExecuteSpan_ServiceGraphAttributes(t *testing.T) {
	executeSpanExporter.Reset()

	_, span := startExecuteSpan(context.Background(), "SELECT 1", "clickhouse:9000")
	span.End()
	if err := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}).ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	spans := executeSpanExporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.SpanKind != trace.SpanKindClient {
		t.Errorf("span kind = %v, want SpanKindClient", got.SpanKind)
	}
	attrs := map[string]string{}
	for _, a := range got.Attributes {
		if a.Value.Type() != 0 && a.Value.AsString() != "" {
			attrs[string(a.Key)] = a.Value.AsString()
		}
	}
	if attrs["peer.service"] != "clickhouse" {
		t.Errorf("peer.service = %q, want %q", attrs["peer.service"], "clickhouse")
	}
	if attrs["server.address"] != "clickhouse:9000" {
		t.Errorf("server.address = %q, want %q", attrs["server.address"], "clickhouse:9000")
	}
	if attrs["net.peer.name"] != "clickhouse:9000" {
		t.Errorf("net.peer.name = %q, want %q", attrs["net.peer.name"], "clickhouse:9000")
	}
}

// TestStartExecuteSpan_EmptyAddr verifies the span still opens cleanly
// when the CH addr is unknown (e.g. the test-only newWithConn seam in
// the chaos tests doesn't propagate it). peer.service stays stamped —
// it's the constant signal — while server.address / net.peer.name are
// omitted rather than written as empty strings.
func TestStartExecuteSpan_EmptyAddr(t *testing.T) {
	executeSpanExporter.Reset()

	_, span := startExecuteSpan(context.Background(), "SELECT 1", "")
	span.End()
	if err := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}).ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	got := executeSpanExporter.GetSpans()[0]
	attrs := map[string]struct{}{}
	peer := ""
	for _, a := range got.Attributes {
		attrs[string(a.Key)] = struct{}{}
		if string(a.Key) == "peer.service" {
			peer = a.Value.AsString()
		}
	}
	if peer != "clickhouse" {
		t.Errorf("peer.service = %q, want %q", peer, "clickhouse")
	}
	if _, ok := attrs["server.address"]; ok {
		t.Errorf("server.address should be omitted when addr is empty")
	}
	if _, ok := attrs["net.peer.name"]; ok {
		t.Errorf("net.peer.name should be omitted when addr is empty")
	}
}
