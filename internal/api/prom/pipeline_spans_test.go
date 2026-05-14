package prom_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// spanRecordingQuerier wraps stubQuerier with the chclient-side
// `execute` pipeline-stage span so the in-handler-process span chain
// reaches the full five-stage shape the pipeline-instrumentation contract
// promises. The real chclient package emits this span when it actually
// calls driver.Conn.Query; tests stub the driver out, so we have to emit
// it ourselves to keep the assertion meaningful.
type spanRecordingQuerier struct {
	stubQuerier
}

var spanQTracer = otel.Tracer("github.com/tsouza/cerberus/internal/chclient")

func (s *spanRecordingQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	_, span := spanQTracer.Start(ctx, cerbtrace.SpanExecute)
	defer span.End()
	return s.stubQuerier.Query(ctx, sql, args...)
}

func (s *spanRecordingQuerier) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	_, span := spanQTracer.Start(ctx, cerbtrace.SpanExecute)
	defer span.End()
	return s.stubQuerier.QueryCursor(ctx, sql, args...)
}

// pipelineSpanExporter is the package-wide in-memory exporter installed
// by TestMain. OTel's global tracer-provider can only be set once for
// the registered wrappers to follow the delegate; once it's set, the
// exporter stays live and tests just call .Reset() to clear state.
var pipelineSpanExporter = tracetest.NewInMemoryExporter()

func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(pipelineSpanExporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	os.Exit(m.Run())
}

// TestPipelineSpans_FiveStageChain asserts that a single /api/v1/query
// request emits the canonical five-span chain — parse → lower →
// optimize → emit → execute — that the pipeline instrumentation
// promises.
func TestPipelineSpans_FiveStageChain(t *testing.T) {
	pipelineSpanExporter.Reset()

	ts := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	q := &spanRecordingQuerier{stubQuerier: stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
		},
	}}
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&time=" + "1715423000")
	if err != nil {
		t.Fatalf("query request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Drain the exporter on the test goroutine before assertions —
	// SimpleSpanProcessor pushes on End(), but the chclient execute
	// span finishes on Cursor.Close(), which runs inside the handler
	// path, so the spans should all be flushed by request return.
	if err := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}).ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	spans := pipelineSpanExporter.GetSpans()
	got := map[string]int{}
	for _, s := range spans {
		got[s.Name]++
	}

	wantNames := []string{
		cerbtrace.SpanParse,
		cerbtrace.SpanLower,
		cerbtrace.SpanOptimize,
		cerbtrace.SpanEmit,
		cerbtrace.SpanExecute,
	}
	for _, name := range wantNames {
		if got[name] == 0 {
			t.Errorf("expected at least one %q span, got none. spans=%v", name, got)
		}
	}

	// Spot-check attribute coverage on the QL-stamped spans. The execute
	// span's db.system / db.statement assertions live in chclient's own
	// unit test (TestStartExecuteSpan), since the real attribute set is
	// produced inside that package — this test uses a stub Querier and
	// only emits the span name to verify the chain shape.
	for _, s := range spans {
		switch s.Name {
		case cerbtrace.SpanLower, cerbtrace.SpanParse:
			ok := false
			for _, a := range s.Attributes {
				if a.Key == cerbtrace.AttrQL && a.Value.AsString() == "promql" {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s span missing cerberus.ql=promql attribute: %+v", s.Name, s.Attributes)
			}
		}
	}
}
