package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/logtest"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// TestNewSlogHandler_NilProvider returns the local handler unchanged
// so callers that disable telemetry don't pay a fan-out cost.
func TestNewSlogHandler_NilProvider(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	local := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewSlogHandler(local, nil)
	if h != local {
		t.Errorf("nil provider should return local handler unchanged; got %T", h)
	}
}

// TestNewSlogHandler_FansOutToBoth confirms one slog record reaches
// both the local handler (stderr-shape sink) AND the OTel bridge
// (OTLP-shape sink) without duplication, attribute loss, or
// truncation.
func TestNewSlogHandler_FansOutToBoth(t *testing.T) {
	t.Parallel()

	var localBuf bytes.Buffer
	localHandler := slog.NewTextHandler(&localBuf, &slog.HandlerOptions{Level: slog.LevelInfo})

	recorder := logtest.NewRecorder()
	handler := NewSlogHandler(localHandler, recorder)
	logger := slog.New(handler)
	logger.Info(
		"test event",
		"cerberus.ql", "promql",
		"cerberus.route", "GET /api/v1/query",
	)

	// Local sink: text format ends up in the buffer.
	if !strings.Contains(localBuf.String(), `msg="test event"`) {
		t.Errorf("local handler missing message; got %q", localBuf.String())
	}
	if !strings.Contains(localBuf.String(), `cerberus.ql=promql`) {
		t.Errorf("local handler missing structured attr; got %q", localBuf.String())
	}

	// OTel sink: the bridge produced exactly one record with the
	// expected body + attributes.
	records := flattenRecorder(recorder)
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}
	if got := records[0].Body.AsString(); got != "test event" {
		t.Errorf("bridge body: got %q, want %q", got, "test event")
	}
	got := attrMap(records[0].Attributes)
	want := map[string]string{
		"cerberus.ql":    "promql",
		"cerberus.route": "GET /api/v1/query",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("bridge attr %s: got %q, want %q", k, got[k], v)
		}
	}
}

// TestNewSlogHandler_NoopProviderIsStderrOnly confirms wiring through
// the no-op LoggerProvider doesn't double-emit or drop the local
// record. This is the production "telemetry disabled" code path.
func TestNewSlogHandler_NoopProviderIsStderrOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	local := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewSlogHandler(local, lognoop.NewLoggerProvider())
	slog.New(h).Info("hello noop")
	if !strings.Contains(buf.String(), "hello noop") {
		t.Errorf("noop provider should still write stderr; got %q", buf.String())
	}
}

// TestNewSlogHandler_PreservesAttrs walks slog's WithAttrs chain and
// confirms both sinks see the augmented record.
func TestNewSlogHandler_PreservesAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	local := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	recorder := logtest.NewRecorder()

	handler := NewSlogHandler(local, recorder)
	logger := slog.New(handler).With("api", "loki")
	logger.Info("dispatched", "sql", "SELECT 1")

	if !strings.Contains(buf.String(), "api=loki") {
		t.Errorf("local: missing api attr; got %q", buf.String())
	}

	records := flattenRecorder(recorder)
	if len(records) == 0 {
		t.Fatalf("bridge: expected record, got none")
	}
	got := attrMap(records[0].Attributes)
	if got["api"] != "loki" {
		t.Errorf("bridge: api attr lost in WithAttrs; got %+v", got)
	}
	if got["sql"] != "SELECT 1" {
		t.Errorf("bridge: sql attr lost; got %+v", got)
	}
}

// TestFanoutHandler_FirstErrorWins confirms that a handler returning
// an error is surfaced even when the other handler succeeds.
func TestFanoutHandler_FirstErrorWins(t *testing.T) {
	t.Parallel()

	ok := stubHandler{enabled: true}
	bad := stubHandler{enabled: true, err: errors.New("boom")}
	h := fanoutHandler{handlers: []slog.Handler{ok, bad}}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hi", callerPC())
	err := h.Handle(context.Background(), rec)
	if err == nil || err.Error() != "boom" {
		t.Errorf("expected first error; got %v", err)
	}
}

// TestFanoutHandler_EnabledIsOr — true if ANY sub-handler accepts the level.
func TestFanoutHandler_EnabledIsOr(t *testing.T) {
	t.Parallel()

	off := stubHandler{enabled: false}
	on := stubHandler{enabled: true}
	h := fanoutHandler{handlers: []slog.Handler{off, on}}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("OR semantics broken: expected enabled when one sub-handler accepts")
	}
	allOff := fanoutHandler{handlers: []slog.Handler{off, off}}
	if allOff.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("OR semantics broken: expected disabled when all sub-handlers reject")
	}
}

// TestProvidersHasLoggerProvider — telemetry.New (no-op endpoint)
// installs a non-nil LoggerProvider so callers can pass it
// unconditionally to NewSlogHandler.
func TestProvidersHasLoggerProvider(t *testing.T) {
	t.Parallel()
	providers, err := New(context.Background(), Config{}) // empty endpoint = no-op
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if providers.LoggerProvider == nil {
		t.Errorf("LoggerProvider must be non-nil even with telemetry disabled")
	}
	// noop logger provider should accept a Logger() call without panicking.
	logger := providers.LoggerProvider.Logger("test")
	if logger == nil {
		t.Errorf("noop Logger() returned nil")
	}
}

type stubHandler struct {
	enabled bool
	err     error
}

func (s stubHandler) Enabled(context.Context, slog.Level) bool  { return s.enabled }
func (s stubHandler) Handle(context.Context, slog.Record) error { return s.err }
func (s stubHandler) WithAttrs([]slog.Attr) slog.Handler        { return s }
func (s stubHandler) WithGroup(string) slog.Handler             { return s }

func callerPC() uintptr {
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:])
	return pcs[0]
}

func flattenRecorder(r *logtest.Recorder) []logtest.Record {
	var out []logtest.Record
	for _, scoped := range r.Result() {
		out = append(out, scoped...)
	}
	return out
}

func attrMap(attrs []otellog.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range attrs {
		out[kv.Key] = kv.Value.AsString()
	}
	return out
}

// Compile-time assertion that the sdklog.LoggerProvider satisfies
// otellog.LoggerProvider — guards against an SDK refactor that breaks
// the interface.
var _ otellog.LoggerProvider = (*sdklog.LoggerProvider)(nil)
