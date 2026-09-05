package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/deltaprefix"
)

// TestSchema_BareInvocationIsError pins that `cerberus schema` with no verb
// reports an error and prints usage to stderr — falling through to a silent
// success would hide an operator mistake, mirroring TestRun_NoFlagsIsError
// for `migrate`.
func TestSchema_BareInvocationIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema(nil, &out, &errOut)
	if err == nil {
		t.Fatal("bare `cerberus schema` should error")
	}
	if err != errNoSchemaSubcommand {
		t.Errorf("error = %v; want errNoSchemaSubcommand", err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be written to stdout on error, got: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("bare invocation should print usage to stderr, got: %q", errOut.String())
	}
}

// TestSchema_HelpExitsCleanToStdout mirrors TestRunHelpExitsCleanToStdout:
// -h/--help must print usage to stdout and return no error, with nothing on
// stderr.
func TestSchema_HelpExitsCleanToStdout(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help"} {
		var out, errOut bytes.Buffer
		if err := runSchema([]string{flagArg}, &out, &errOut); err != nil {
			t.Fatalf("run %s should exit cleanly, got error: %v", flagArg, err)
		}
		if out.Len() == 0 {
			t.Errorf("run %s should print usage to stdout", flagArg)
		}
		if errOut.Len() != 0 {
			t.Errorf("run %s should write nothing to stderr, got: %q", flagArg, errOut.String())
		}
	}
}

// TestSchema_SubcommandHelpExitsClean mirrors TestSubcommandHelpExitsClean:
// every schema subcommand's -h exits 0 with usage on stdout and nothing on
// stderr.
func TestSchema_SubcommandHelpExitsClean(t *testing.T) {
	for _, sc := range []string{"delta-prefix-backfill", "delta-prefix-verify", "retire-idx-lower-body"} {
		var out, errOut bytes.Buffer
		if err := runSchema([]string{sc, "-h"}, &out, &errOut); err != nil {
			t.Errorf("run %s -h should exit cleanly, got: %v", sc, err)
		}
		if out.Len() == 0 {
			t.Errorf("run %s -h should print usage to stdout", sc)
		}
		if errOut.Len() != 0 {
			t.Errorf("run %s -h should write nothing to stderr, got: %q", sc, errOut.String())
		}
	}
}

// TestSchema_RootUsageListsSubcommands mirrors TestRootUsageListsSubcommands:
// the root usage names every subcommand so an operator can discover them.
func TestSchema_RootUsageListsSubcommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runSchema([]string{"-h"}, &out, &errOut); err != nil {
		t.Fatalf("run -h: %v", err)
	}
	usage := out.String()
	for _, name := range []string{"delta-prefix-backfill", "delta-prefix-verify", "retire-idx-lower-body"} {
		if !strings.Contains(usage, name) {
			t.Errorf("root usage should list subcommand %q, got:\n%s", name, usage)
		}
	}
}

// TestSchema_UnknownSubcommandIsError mirrors TestRunUnknownSubcommand: a
// mistyped verb is a clear cobra error, not a silent fall-through to the
// group's own "nothing to do".
func TestSchema_UnknownSubcommandIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-backfilll"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unknown subcommand should error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error should name the unknown command, got: %v", err)
	}
	if strings.Contains(err.Error(), "nothing to do") {
		t.Errorf("unknown subcommand must not fall through to the group's own 'nothing to do', got: %v", err)
	}
}

// TestDeltaPrefixBackfill_MissingBeforeIsError pins that --before is
// required and that the error names the flag and the help path, rather than
// a bare cobra usage error.
func TestDeltaPrefixBackfill_MissingBeforeIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-backfill"}, &out, &errOut)
	if err == nil {
		t.Fatal("delta-prefix-backfill with no --before should error")
	}
	if !strings.Contains(err.Error(), "--before") {
		t.Errorf("error should name --before, got: %v", err)
	}
}

// TestDeltaPrefixVerify_MissingBeforeIsError mirrors the backfill case for
// delta-prefix-verify.
func TestDeltaPrefixVerify_MissingBeforeIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-verify"}, &out, &errOut)
	if err == nil {
		t.Fatal("delta-prefix-verify with no --before should error")
	}
	if !strings.Contains(err.Error(), "--before") {
		t.Errorf("error should name --before, got: %v", err)
	}
}

// TestDeltaPrefixBackfill_InvalidBeforeIsError pins that an unparseable
// --before value surfaces the parse error rather than proceeding with a
// zero time.Time (which would silently backfill/compare against the Unix
// epoch instead of failing loudly).
func TestDeltaPrefixBackfill_InvalidBeforeIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-backfill", "--before", "not-a-time", "--dry-run"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unparseable --before should error")
	}
	if !strings.Contains(err.Error(), "parse --before") {
		t.Errorf("error should name the --before parse failure, got: %v", err)
	}
}

// TestDeltaPrefixBackfill_DryRunPrintsSQLWithoutConnecting confirms --dry-run
// prints the rendered INSERT ... SELECT statement to stdout and returns no
// error WITHOUT ever needing a live ClickHouse connection (no CERBERUS_CH_*
// env is set in this test process, so a real connection attempt would
// either fail fast or hang — --dry-run must short-circuit before either).
func TestDeltaPrefixBackfill_DryRunPrintsSQLWithoutConnecting(t *testing.T) {
	// schema.Metrics.DeltaPrefixTable only populates when the operator has
	// opted in (see TestDefaultOTelMetricsFromEnv_DeltaPrefix in
	// internal/schema) — set it here so config.FromEnv() resolves a real
	// table name for deltaPrefixColumns to build against.
	t.Setenv("CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED", "true")
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-backfill", "--before", "2026-08-20T00:00:00Z", "--dry-run"}, &out, &errOut)
	if err != nil {
		t.Fatalf("dry-run should not error: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "INSERT INTO") || !strings.Contains(got, "otel_metrics_sum_delta_prefix") {
		t.Errorf("dry-run should print the rendered INSERT statement, got: %q", got)
	}
	if !strings.Contains(got, "toStartOfDay(`TimeUnix`)") {
		t.Errorf("dry-run output should show the day-bucket GROUP BY, got: %q", got)
	}
}

// TestDeltaPrefixBackfill_NotOptedInIsError confirms that WITHOUT
// CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED set, --dry-run still fails with a
// clear error rather than silently rendering SQL against a table name the
// deployment's own resolved schema does not actually declare (schema.Metrics
// .DeltaPrefixTable is empty unless the operator opted in — see
// TestDefaultOTelMetricsFromEnv_DeltaPrefix in internal/schema).
func TestDeltaPrefixBackfill_NotOptedInIsError(t *testing.T) {
	t.Setenv("CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED", "")
	var out, errOut bytes.Buffer
	err := runSchema([]string{"delta-prefix-backfill", "--before", "2026-08-20T00:00:00Z", "--dry-run"}, &out, &errOut)
	if err == nil {
		t.Fatal("delta-prefix-backfill without CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED should error")
	}
	if !strings.Contains(err.Error(), "DeltaPrefixTable") {
		t.Errorf("error should name the missing DeltaPrefixTable, got: %v", err)
	}
}

// TestRetireIdxLowerBody_DryRunPrintsSQLWithoutConnecting confirms --dry-run
// prints the rendered ALTER TABLE DROP INDEX statement to stdout and returns
// no error WITHOUT ever needing a live ClickHouse connection — mirrors
// TestDeltaPrefixBackfill_DryRunPrintsSQLWithoutConnecting.
func TestRetireIdxLowerBody_DryRunPrintsSQLWithoutConnecting(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSchema([]string{"retire-idx-lower-body", "--dry-run"}, &out, &errOut)
	if err != nil {
		t.Fatalf("dry-run should not error: %v (stderr: %s)", err, errOut.String())
	}
	got := strings.TrimSpace(out.String())
	want := "ALTER TABLE default.otel_logs DROP INDEX IF EXISTS idx_lower_body"
	if got != want {
		t.Errorf("dry-run output = %q; want %q", got, want)
	}
}

// TestRetireIdxLowerBody_DryRunHonorsTableOverride confirms the verb reads
// the SAME CERBERUS_SCHEMA_LOGS_TABLE override every query-answering path
// reads, rather than hard-coding "otel_logs" — a mismatch here would drop
// the index on the wrong table (or silently no-op via IF EXISTS on a table
// that was never the operator's real logs table).
func TestRetireIdxLowerBody_DryRunHonorsTableOverride(t *testing.T) {
	t.Setenv("CERBERUS_SCHEMA_LOGS_TABLE", "logs_v2")
	t.Setenv("CERBERUS_CH_DATABASE", "otel")
	var out, errOut bytes.Buffer
	err := runSchema([]string{"retire-idx-lower-body", "--dry-run"}, &out, &errOut)
	if err != nil {
		t.Fatalf("dry-run should not error: %v (stderr: %s)", err, errOut.String())
	}
	got := strings.TrimSpace(out.String())
	want := "ALTER TABLE otel.logs_v2 DROP INDEX IF EXISTS idx_lower_body"
	if got != want {
		t.Errorf("dry-run output = %q; want %q", got, want)
	}
}

// testVerifyReport builds a deltaprefix.Report with an outside-retention
// day, so writeDeltaPrefixVerifyReport's tests below can drive both the
// PASS and FAIL branches with the SAME excluded-day shape.
func testVerifyReport(mismatches []deltaprefix.Mismatch, aggregate, base map[string]float64) deltaprefix.Report {
	return deltaprefix.Report{
		Before:               time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Tolerance:            0.01,
		AggregateTotals:      aggregate,
		BaseTotals:           base,
		Mismatches:           mismatches,
		Retention:            24 * time.Hour,
		RetentionBoundary:    time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		OutsideRetentionDays: []time.Time{time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)},
	}
}

// TestWriteDeltaPrefixVerifyReport_PassWithOutsideRetentionNote confirms
// cerberus issue #2652's core CLI requirement: a PASS report that excluded
// an outside-retention day prints a clearly LABELED, separate NOTE — never
// folded into an unexplained FAIL, and never silently dropped either.
func TestWriteDeltaPrefixVerifyReport_PassWithOutsideRetentionNote(t *testing.T) {
	rep := testVerifyReport(nil, map[string]float64{"m": 10}, map[string]float64{"m": 10})
	var buf bytes.Buffer
	if err := writeDeltaPrefixVerifyReport(&buf, rep, false); err != nil {
		t.Fatalf("writeDeltaPrefixVerifyReport: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "PASS:") {
		t.Errorf("expected a PASS line, got: %q", got)
	}
	if strings.Contains(got, "FAIL") {
		t.Errorf("an outside-retention day must never fold into an unexplained FAIL, got: %q", got)
	}
	if !strings.Contains(got, "NOTE:") || !strings.Contains(got, "outside the aggregate table's retention window") {
		t.Errorf("expected a separate outside-retention NOTE, got: %q", got)
	}
	if !strings.Contains(got, "cannot be backfilled") || !strings.Contains(got, "docs/operations.md") {
		t.Errorf("NOTE should point the operator at the runbook, got: %q", got)
	}
	if !strings.Contains(got, "2026-08-20") {
		t.Errorf("NOTE should list the excluded day, got: %q", got)
	}
}

// TestWriteDeltaPrefixVerifyReport_FailKeepsOutsideRetentionNoteSeparate
// confirms a GENUINE mismatch still prints the ordinary FAIL table, with
// the outside-retention NOTE appended afterward as a visibly distinct
// section rather than mixed into the mismatch rows.
func TestWriteDeltaPrefixVerifyReport_FailKeepsOutsideRetentionNoteSeparate(t *testing.T) {
	rep := testVerifyReport(
		[]deltaprefix.Mismatch{{MetricName: "missed_metric", Aggregate: 0, Base: 5}},
		map[string]float64{"m": 10},
		map[string]float64{"m": 10, "missed_metric": 5},
	)
	var buf bytes.Buffer
	if err := writeDeltaPrefixVerifyReport(&buf, rep, false); err != nil {
		t.Fatalf("writeDeltaPrefixVerifyReport: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "FAIL: 1 of") {
		t.Errorf("expected the ordinary FAIL summary line, got: %q", got)
	}
	if !strings.Contains(got, "missed_metric") {
		t.Errorf("expected the mismatch table to list missed_metric, got: %q", got)
	}
	if !strings.Contains(got, "NOTE:") {
		t.Errorf("expected the outside-retention NOTE to still appear alongside a real FAIL, got: %q", got)
	}
}

// TestWriteDeltaPrefixVerifyReport_NoNoteWhenNothingExcluded confirms an
// ordinary PASS/FAIL report — no outside-retention days at all — prints no
// NOTE section, so the common case's output is unchanged from before this
// fix.
func TestWriteDeltaPrefixVerifyReport_NoNoteWhenNothingExcluded(t *testing.T) {
	rep := deltaprefix.Diff(map[string]float64{"m": 10}, map[string]float64{"m": 10}, time.Now(), 0.01)
	var buf bytes.Buffer
	if err := writeDeltaPrefixVerifyReport(&buf, rep, false); err != nil {
		t.Fatalf("writeDeltaPrefixVerifyReport: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "NOTE:") {
		t.Errorf("expected no NOTE section with zero excluded days, got: %q", got)
	}
}

// TestWriteDeltaPrefixVerifyReport_JSONIncludesRetentionFields confirms the
// machine-readable --json form carries the new Retention /
// RetentionBoundary / OutsideRetentionDays fields, not just the text
// renderer.
func TestWriteDeltaPrefixVerifyReport_JSONIncludesRetentionFields(t *testing.T) {
	rep := testVerifyReport(nil, map[string]float64{"m": 10}, map[string]float64{"m": 10})
	var buf bytes.Buffer
	if err := writeDeltaPrefixVerifyReport(&buf, rep, true); err != nil {
		t.Fatalf("writeDeltaPrefixVerifyReport: %v", err)
	}
	var decoded deltaprefix.Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v (output: %s)", err, buf.String())
	}
	if decoded.Retention != rep.Retention {
		t.Errorf("decoded Retention = %v; want %v", decoded.Retention, rep.Retention)
	}
	if len(decoded.OutsideRetentionDays) != 1 {
		t.Errorf("decoded OutsideRetentionDays = %v; want exactly 1 entry", decoded.OutsideRetentionDays)
	}
}

// TestWriteOutsideRetentionWarning_BackfillCLI pins
// delta-prefix-backfill's own post-Backfill warning text (cerberus issue
// #2652): loud, names the table, explains WHY re-running cannot fix it,
// and lists every affected day.
func TestWriteOutsideRetentionWarning_BackfillCLI(t *testing.T) {
	var buf bytes.Buffer
	days := []time.Time{time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)}
	writeOutsideRetentionWarning(&buf, "otel_metrics_sum_delta_prefix", days)
	got := buf.String()
	if !strings.Contains(got, "WARNING") {
		t.Errorf("expected a WARNING marker, got: %q", got)
	}
	if !strings.Contains(got, "otel_metrics_sum_delta_prefix") {
		t.Errorf("expected the warning to name the target table, got: %q", got)
	}
	if !strings.Contains(got, "NOT fixable by re-running") {
		t.Errorf("expected the warning to explain re-running cannot fix this, got: %q", got)
	}
	for _, d := range days {
		if !strings.Contains(got, d.Format(time.DateOnly)) {
			t.Errorf("expected the warning to list day %s, got: %q", d.Format(time.DateOnly), got)
		}
	}
}
