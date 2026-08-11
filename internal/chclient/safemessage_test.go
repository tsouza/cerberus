package chclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// noSupertypeException reproduces the exception ClickHouse raises for
// NO_COMMON_TYPE (code 386) — the one issue #2033 observed reaching an
// HTTP response body verbatim.
func noSupertypeException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    386,
		Message: "There is no supertype for types UInt64, String because some of them are String/FixedString/Enum and some of them are not: while executing 'FUNCTION greater(Duration, SpanAttributes[...])'",
	}
}

// TestSafeMessageDropsServerExceptionText pins that no ClickHouse error
// code, type name or SQL fragment survives into the rendered message,
// while the cerberus-authored stage markers around it do.
func TestSafeMessageDropsServerExceptionText(t *testing.T) {
	err := fmt.Errorf("engine: execute: chclient: query: %w", noSupertypeException())

	got := SafeMessage(err)

	for _, leak := range []string{"386", "supertype", "UInt64", "FUNCTION greater", "code:"} {
		if strings.Contains(got, leak) {
			t.Errorf("SafeMessage leaked %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, "engine: execute: chclient: query:") {
		t.Errorf("SafeMessage dropped the cerberus stage markers: %q", got)
	}
	if !strings.Contains(got, serverExceptionPlaceholder) {
		t.Errorf("SafeMessage = %q, want it to name the substitution", got)
	}
}

// TestSafeMessageKeepsCerberusAuthoredText pins the other direction: a
// message cerberus wrote itself is what the caller acts on, so it must
// survive untouched — including a classified failure that carries the
// exception for errors.As but renders its own wording.
func TestSafeMessageKeepsCerberusAuthoredText(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"plain cerberus error", errors.New("traceql: query is not a metrics pipeline")},
		{"memory limit", &MemoryLimitError{Limit: 1073741824, Cause: &clickhouse.Exception{
			Code: chCodeMemoryLimitExceeded, Message: "Memory limit (total) exceeded: would use 2.12 GiB",
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := SafeMessage(tc.err), tc.err.Error(); got != want {
				t.Errorf("SafeMessage = %q, want %q unchanged", got, want)
			}
		})
	}
}

// TestSafeMessageNilError pins the degenerate input rather than leaving it
// to a nil dereference at a call site on an error path.
func TestSafeMessageNilError(t *testing.T) {
	if got := SafeMessage(nil); got != "" {
		t.Errorf("SafeMessage(nil) = %q, want empty", got)
	}
}
