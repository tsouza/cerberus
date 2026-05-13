package chclient

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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
	_, span := startExecuteSpan(context.Background(), sql)
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
	_, span := startExecuteSpan(context.Background(), sql)
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
