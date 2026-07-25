package telemetry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// decodeRecords parses the JSON lines a slog.JSONHandler wrote.
func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// TestInstallErrorHandler_LogsStructuredWarning is the alerting guard
// for cerberus's own telemetry pipeline. When the OTel SDK reports a
// failure the gateway has just lost the metrics / traces / logs an
// operator would use to diagnose it, so the report has to be (a) above
// the level lifecycle chatter sits at, and (b) structured — a level and
// a component field are what make it selectable by an alert rule.
func TestInstallErrorHandler_LogsStructuredWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	telemetry.InstallErrorHandler(logger)

	otel.Handle(errors.New("exporter unreachable"))

	records := decodeRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("records: got %d want 1 (%q)", len(records), buf.String())
	}
	rec := records[0]
	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v; want WARN — INFO is not selectable by an alert", got)
	}
	if got := rec["component"]; got != "otel" {
		t.Errorf("component = %v; want otel", got)
	}
	if got, ok := rec["err"].(string); !ok || got != "exporter unreachable" {
		t.Errorf("err = %v; want the native error message", rec["err"])
	}
	if msg, ok := rec["msg"].(string); !ok || msg == "" {
		t.Errorf("msg = %v; want a non-empty fixed message", rec["msg"])
	}
}

// TestInstallErrorHandler_AboveInfo pins the level ordering rather than
// the literal: whatever level the handler uses, it must outrank INFO,
// which this codebase reserves for lifecycle events nothing alerts on.
func TestInstallErrorHandler_AboveInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	telemetry.InstallErrorHandler(logger)

	otel.Handle(errors.New("export failed"))

	if records := decodeRecords(t, &buf); len(records) != 1 {
		t.Fatalf("record dropped by a WARN-gated handler: got %d records (%q)", len(records), buf.String())
	}
}

// TestInstallErrorHandler_NilLoggerUsesDefault covers the fallback: a
// nil logger must not swallow the report, and the default is resolved
// per call so a later slog.SetDefault is honoured.
func TestInstallErrorHandler_NilLoggerUsesDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	telemetry.InstallErrorHandler(nil)
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	otel.Handle(errors.New("boom"))

	records := decodeRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("records: got %d want 1 (%q)", len(records), buf.String())
	}
	if got := records[0]["component"]; got != "otel" {
		t.Errorf("component = %v; want otel", got)
	}
}

// TestInstallErrorHandler_NilErrorIsNotLogged keeps the handler from
// manufacturing noise the SDK never reported.
func TestInstallErrorHandler_NilErrorIsNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	telemetry.InstallErrorHandler(logger)

	otel.Handle(nil)

	if records := decodeRecords(t, &buf); len(records) != 0 {
		t.Errorf("nil error produced %d records (%q)", len(records), buf.String())
	}
}
