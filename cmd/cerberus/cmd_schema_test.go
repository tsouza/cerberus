package main

import (
	"bytes"
	"strings"
	"testing"
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
	for _, sc := range []string{"delta-prefix-backfill", "delta-prefix-verify"} {
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
	for _, name := range []string{"delta-prefix-backfill", "delta-prefix-verify"} {
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
