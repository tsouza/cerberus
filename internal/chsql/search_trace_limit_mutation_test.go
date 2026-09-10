package chsql

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmitSearchTraceLimit_RequiresBothColumnNames pins that the top-N
// trace restriction rejects a plan missing EITHER column name.
//
// Kills the INVERT_LOGICAL mutant of
// search_trace_limit.go:`if n.TraceIDColumn == "" || n.TimestampColumn == ""`.
// The mutant reads `... == "" && ... == ""`, so a plan naming only one of the
// two is accepted and emits a ranking subquery over a nameless column —
// `ORDER BY min(“) DESC` for a missing TimestampColumn, or a
// “ “ GLOBAL IN (…)“ restriction for a missing TraceIDColumn. Both are
// invalid SQL that no fail-closed guard would then catch, which is why the
// guard is an OR. The one-sided cases are what discriminate the two spellings;
// the both-unset and both-present cases agree under either, and are asserted
// beside them so the test states the whole contract.
//
// search_trace_limit.go's other guards (a non-positive TraceLimit, the two
// subqueryFrag error returns) are already killed by fail_closed_guards_test.go
// and the scan-window suites; this file exists for the one mutant they leave.
func TestEmitSearchTraceLimit_RequiresBothColumnNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		traceID   string
		timestamp string
		wantErr   bool
	}{
		{name: "both present", traceID: "TraceId", timestamp: "Timestamp", wantErr: false},
		{name: "timestamp unset", traceID: "TraceId", timestamp: "", wantErr: true},
		{name: "trace id unset", traceID: "", timestamp: "Timestamp", wantErr: true},
		{name: "both unset", traceID: "", timestamp: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := &chplan.SearchTraceLimit{
				Input:           &chplan.Scan{Table: "otel_traces"},
				TraceIDColumn:   tc.traceID,
				TimestampColumn: tc.timestamp,
				TraceLimit:      7,
			}
			sql, _, err := Emit(context.Background(), n)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("Emit err = %v, want ErrUnsupported (SQL: %s)", err, sql)
				}
				return
			}
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
		})
	}
}
