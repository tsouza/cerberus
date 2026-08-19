package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// decodeLogRecords parses the JSON lines a slog.JSONHandler wrote, mirroring
// internal/telemetry's own test helper (there is no shared one to import
// without creating a test-only dependency between the two packages).
func decodeLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
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

// withCapturedDefaultLog swaps slog's package-global default logger for a
// JSON handler writing into buf, for the duration of the test. logQueryFailure
// (internal/engine/engine.go) logs through slog.Default() — the same
// convention internal/telemetry/errorhandler.go and internal/config/config.go
// already use for a subsystem that has no logger of its own threaded to it —
// so this is the only seam available to observe it.
func withCapturedDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// findQueryFailureRecords filters decoded records down to the ones this
// change's log line produces, so a test isn't tripped up by an unrelated WARN
// some other subsystem emits incidentally during the same run.
func findQueryFailureRecords(records []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == "cerberus query failed" {
			out = append(out, r)
		}
	}
	return out
}

// TestLogQueryFailure_EagerDispatchError is the core regression this change
// exists for: a production incident found cerberus emitted NO application log
// for a query that failed against ClickHouse — timeouts, OOMs, aborts were
// visible only by querying cerberus's own ClickHouse-side telemetry table
// directly. A dispatch error on the eager engine.Query path (the path every
// PromQL instant query, every LogQL query, and every eager TraceQL query
// funnels through) must now produce exactly one WARN-level, structured log
// line naming the failure.
func TestLogQueryFailure_EagerDispatchError(t *testing.T) {
	buf := withCapturedDefaultLog(t)

	q := &fakeQuerier{err: chclient.ErrQueryTimeout}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	if _, err := eng.Query(context.Background(), lang, "up"); err == nil {
		t.Fatal("Query: expected an error from the dispatch")
	}

	records := findQueryFailureRecords(decodeLogRecords(t, buf))
	if len(records) != 1 {
		t.Fatalf("cerberus query failed records = %d; want exactly 1 (log: %s)", len(records), buf.String())
	}
	rec := records[0]

	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v; want WARN", got)
	}
	if got := rec["cerberus_ql"]; got != "promql" {
		t.Errorf("cerberus_ql = %v; want promql", got)
	}
	// planShapeID is literal-free by design (docs: no metric names, no label
	// values) — the assertion is on the stable "cerb:" namespace prefix, not
	// on the query's own literals.
	shapeID, _ := rec["shape_id"].(string)
	if shapeID == "" || shapeID[:5] != "cerb:" {
		t.Errorf("shape_id = %q; want a non-empty cerb:-prefixed shape id", shapeID)
	}
	if got := rec["exit_class"]; got != "timeout" {
		t.Errorf("exit_class = %v; want timeout", got)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("duration_ms: missing")
	}
	if _, ok := rec["query_id"]; !ok {
		t.Error("query_id: missing")
	}
	errField, ok := rec["err"].(string)
	if !ok || errField == "" {
		t.Errorf("err = %v; want the native error's message", rec["err"])
	}
	// decision_reason is present (possibly empty-string) rather than absent —
	// the Solver is nil in this test (unclassified head), which routeFeatures
	// reports as an empty reason, not a missing key.
	if _, ok := rec["decision_reason"]; !ok {
		t.Error("decision_reason: missing key (want present, even if empty)")
	}
}

// TestLogQueryFailure_SuccessfulQuery_NoLog is the volume-discipline half of
// the same change: a clean query must NEVER produce this log line, or every
// production deployment would pay a per-query logging cost for the common
// case the task explicitly rules out.
func TestLogQueryFailure_SuccessfulQuery_NoLog(t *testing.T) {
	buf := withCapturedDefaultLog(t)

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "up", Value: 1}}}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	if _, err := eng.Query(context.Background(), lang, "up"); err != nil {
		t.Fatalf("Query: unexpected err: %v", err)
	}

	if records := findQueryFailureRecords(decodeLogRecords(t, buf)); len(records) != 0 {
		t.Errorf("cerberus query failed records = %d; want 0 on a clean query (log: %s)", len(records), buf.String())
	}
}

// TestLogQueryFailure_CursorDrainError pins the streaming sibling: a range
// query's cursor drain fails in the HANDLER (outside the engine package
// entirely), and the handler reports it back through
// Engine.ObserveDrainOutcome — the same seam that stamps the ClickHouse-side
// corpus. The log line must fire from that seam too, and must fire whether or
// not the corpus observer is even registered (most deployments run with
// CERBERUS_CH_OPT_CORPUS_ENABLED unset).
func TestLogQueryFailure_CursorDrainError(t *testing.T) {
	buf := withCapturedDefaultLog(t)

	eng := newEngine(&fakeQuerier{})
	// No QueryObserver registered — the default hot path.

	eng.ObserveDrainOutcome("qid-drain", "promql", 0, chclient.ErrMemoryLimitExceeded)

	records := findQueryFailureRecords(decodeLogRecords(t, buf))
	if len(records) != 1 {
		t.Fatalf("cerberus query failed records = %d; want exactly 1 (log: %s)", len(records), buf.String())
	}
	rec := records[0]
	if got := rec["exit_class"]; got != "oom" {
		t.Errorf("exit_class = %v; want oom", got)
	}
	if got := rec["query_id"]; got != "qid-drain" {
		t.Errorf("query_id = %v; want qid-drain", got)
	}
	// The drain seam has no plan in scope (see logQueryFailure's doc comment):
	// shape_id must be honestly empty, never fabricated.
	if got, ok := rec["shape_id"]; !ok || got != "" {
		t.Errorf("shape_id = %v; want empty string (no plan at the drain seam)", got)
	}
}

// TestLogQueryFailure_CursorDrain_NilErrorNoLog mirrors the success-path
// no-log guard for the drain seam: a clean drain (nil error) must not log.
func TestLogQueryFailure_CursorDrain_NilErrorNoLog(t *testing.T) {
	buf := withCapturedDefaultLog(t)

	eng := newEngine(&fakeQuerier{})
	eng.ObserveDrainOutcome("qid-drain", "promql", 0, nil)

	if records := findQueryFailureRecords(decodeLogRecords(t, buf)); len(records) != 0 {
		t.Errorf("cerberus query failed records = %d; want 0 on a clean drain (log: %s)", len(records), buf.String())
	}
}

// TestLogQueryFailure_UnclassifiedError pins the honest floor: a ClickHouse
// exception (or any error) that matches none of outcomeTokenForErr's
// cerberus-side outcomes and none of the synchronous timeout/cancel checks
// still produces the log line, classified "error" rather than being silently
// dropped — this is precisely the class of failure (a plain CH exception)
// the production incident found invisible.
func TestLogQueryFailure_UnclassifiedError(t *testing.T) {
	buf := withCapturedDefaultLog(t)

	q := &fakeQuerier{err: errors.New("Code: 999. DB::Exception: something exploded")}
	lang := &fakeLang{name: "logql"}
	eng := newEngine(q)

	if _, err := eng.Query(context.Background(), lang, "{app=\"x\"}"); err == nil {
		t.Fatal("Query: expected an error from the dispatch")
	}

	records := findQueryFailureRecords(decodeLogRecords(t, buf))
	if len(records) != 1 {
		t.Fatalf("cerberus query failed records = %d; want exactly 1 (log: %s)", len(records), buf.String())
	}
	if got := records[0]["exit_class"]; got != "error" {
		t.Errorf("exit_class = %v; want error", got)
	}
	if got := records[0]["cerberus_ql"]; got != "logql" {
		t.Errorf("cerberus_ql = %v; want logql", got)
	}
}
