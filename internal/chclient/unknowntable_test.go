package chclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chUnknownTableException builds the typed driver exception ClickHouse
// raises for UNKNOWN_TABLE — the shape a metadata query naming a table
// this deployment never provisioned (or one dropped externally after
// boot; see #1949) surfaces.
func chUnknownTableException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    60,
		Name:    "UNKNOWN_TABLE",
		Message: "Table default.otel_metrics_histogram doesn't exist",
	}
}

// TestIsUnknownTable_Code60 — a typed *clickhouse.Exception carrying code
// 60 is recognised via errors.As, mirroring isMemoryLimitExceeded /
// IsQueryTimeout.
func TestIsUnknownTable_Code60(t *testing.T) {
	t.Parallel()

	if !IsUnknownTable(chUnknownTableException()) {
		t.Error("IsUnknownTable(code 60 exception) = false; want true")
	}
	// Still reachable through an fmt.Errorf %w wrap, matching how
	// classifyDriverErr's callers surface it (e.g. "chclient: query: %w").
	wrapped := fmt.Errorf("chclient: query: %w", chUnknownTableException())
	if !IsUnknownTable(wrapped) {
		t.Error("IsUnknownTable(wrapped code 60 exception) = false; want true")
	}
}

// TestIsUnknownTable_PassThrough — nil, non-exception errors, and
// exceptions with OTHER codes must not classify as UNKNOWN_TABLE:
// classification is typed-code-60-first, never loose string matching.
func TestIsUnknownTable_PassThrough(t *testing.T) {
	t.Parallel()

	if IsUnknownTable(nil) {
		t.Error("IsUnknownTable(nil) = true; want false")
	}

	otherCode := &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED", Message: "Memory limit exceeded"}
	if IsUnknownTable(otherCode) {
		t.Error("IsUnknownTable(code 241 exception) = true; want false (only code 60 matches)")
	}

	plain := errors.New("read: connection reset by peer")
	if IsUnknownTable(plain) {
		t.Error("IsUnknownTable(unrelated plain error) = true; want false")
	}
}

// TestIsUnknownTable_StringFallback — the narrow phrase fallback catches
// the rejection even when the driver wraps the exception opaquely enough
// to defeat errors.As, but must not fire on an UNKNOWN_DATABASE message
// (preflight's own "does not exist" phrasing) — the two are phrased
// differently by ClickHouse ("doesn't exist" vs "does not exist") and must
// never collide.
func TestIsUnknownTable_StringFallback(t *testing.T) {
	t.Parallel()

	tableMsg := errors.New("code: 60, message: Table default.otel_metrics_histogram doesn't exist. (UNKNOWN_TABLE)")
	if !IsUnknownTable(tableMsg) {
		t.Error("IsUnknownTable(opaque UNKNOWN_TABLE message) = false; want true")
	}

	dbMsg := errors.New("code: 81, message: Database otel does not exist. (UNKNOWN_DATABASE)")
	if IsUnknownTable(dbMsg) {
		t.Error("IsUnknownTable(UNKNOWN_DATABASE message) = true; want false (must not collide with 'does not exist')")
	}
}
