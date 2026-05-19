package logql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// emitArgs lowers q against s and returns the bound positional args
// that the chsql emitter recorded. Mirrors emitSQL but exposes args
// instead of the SQL text — used by the typed-form rename tests to
// pin the canonical `<id>` / `<id>_extracted` / `<src>` binding order
// without coupling to the surrounding SQL skeleton.
func emitArgs(t *testing.T, q string, s schema.Logs) []any {
	t.Helper()
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", q, err)
	}
	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return args
}

// TestLogfmtConflictRename_BareForm pins that bare `| logfmt` emits a
// `mapApply` wrapper around the extracted-key map so colliding keys
// (extracted == stream label) get the `_extracted` suffix at SQL
// runtime. Without the wrapper the prior `mapConcat(prev, extracted)`
// shape let extracted keys silently override stream-selector labels —
// the inverse of Loki's documented contract.
func TestLogfmtConflictRename_BareForm(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	sql := emitSQL(t, `{level="error"} | logfmt | level_extracted="warn"`, s)

	// The rename happens inside a mapApply that wraps
	// extractKeyValuePairs. The lambda body emits a tuple(<rename>, v).
	wantFragments := []string{
		"mapApply((k, v) -> tuple(",
		"if(mapContains(`ResourceAttributes`, k), concat(k, ?), k), v)",
		"extractKeyValuePairs(`Body`",
	}
	for _, want := range wantFragments {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing expected fragment %q\nfull SQL: %s", want, sql)
		}
	}

	// Sanity: the prior naive shape no longer appears — i.e., the raw
	// `mapConcat(`ResourceAttributes`, extractKeyValuePairs(...))` with
	// no rename wrapper is gone.
	if strings.Contains(sql, "mapConcat(`ResourceAttributes`, extractKeyValuePairs(") {
		t.Errorf("unexpected unmediated mapConcat(...) — the rename wrapper is missing\nfull SQL: %s", sql)
	}
}

// TestLogfmtConflictRename_TypedForm pins that typed `| logfmt foo="..."`
// lowering wraps each destination identifier in an `if(mapContains(...))`
// that resolves at query time to either the bare identifier or the
// `_extracted`-suffixed form. The typed-form rename is per-key instead
// of `mapApply` because the destination identifier set is known
// statically at SQL-emit time.
func TestLogfmtConflictRename_TypedForm(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	sql := emitSQL(t, `{level="error"} | logfmt level="severity" | level_extracted="warn"`, s)

	// The destination identifier ('level') is wrapped in an
	// if(mapContains(ResourceAttributes, ?), ?, ?) call. The three
	// placeholders bind: the stream-membership check key, the suffixed
	// rename, and the bare identifier.
	want := "map(if(mapContains(`ResourceAttributes`, ?), ?, ?), extractKeyValuePairs(`Body`"
	if !strings.Contains(sql, want) {
		t.Errorf("SQL missing typed-form rename wrapper %q\nfull SQL: %s", want, sql)
	}

	// The prior unmediated shape — `map(?, extractKeyValuePairs(...)[?])`
	// without the `if(mapContains(...))` rename — should be gone.
	if strings.Contains(sql, "map(?, extractKeyValuePairs(") {
		t.Errorf("unexpected unmediated map(?, extractKeyValuePairs(...)) shape\nfull SQL: %s", sql)
	}
}

// TestLogfmtConflictRename_BindsDuplicateSuffix pins that the rename
// binds Loki's `_extracted` suffix as a bound argument (not embedded
// in the SQL text). The suffix is the canonical Loki magic string and
// must match `loglib.duplicateSuffix` (unexported upstream, mirrored
// in cerberus as `logql.duplicateSuffix`).
func TestLogfmtConflictRename_BindsDuplicateSuffix(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	args := emitArgs(t, `{job="api"} | logfmt | level="error"`, s)

	var found bool
	for _, a := range args {
		if v, ok := a.(string); ok && v == "_extracted" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected `_extracted` suffix bound as an arg; got args=%v", args)
	}
}

// TestLogfmtConflictRename_TypedBindsBothNames pins that the typed-form
// rename binds BOTH `<id>` and `<id>_extracted` strings — the
// `if(mapContains(ResourceAttributes, '<id>'), '<id>_extracted', '<id>')`
// shape needs all three at runtime. A downstream label filter is
// required so the lowering threads the parser stage's labelsExpr into
// the predicate (otherwise the unused parser stage is elided).
// Verifies the rename targets the destination identifier, not the
// source key.
func TestLogfmtConflictRename_TypedBindsBothNames(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	args := emitArgs(t, `{job="api"} | logfmt level="severity" | level="error"`, s)

	var sawLevel, sawLevelExtracted, sawSeverity bool
	for _, a := range args {
		switch v := a.(type) {
		case string:
			switch v {
			case "level":
				sawLevel = true
			case "level_extracted":
				sawLevelExtracted = true
			case "severity":
				sawSeverity = true
			}
		}
	}
	if !sawLevel {
		t.Errorf("missing `level` (destination identifier) in args=%v", args)
	}
	if !sawLevelExtracted {
		t.Errorf("missing `level_extracted` (suffixed destination) in args=%v", args)
	}
	if !sawSeverity {
		t.Errorf("missing `severity` (source key) in args=%v", args)
	}
}
