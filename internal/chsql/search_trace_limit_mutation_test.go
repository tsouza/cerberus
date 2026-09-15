package chsql

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmitSearchTraceLimit_RequiresBothInputRoles pins that both named child-schema roles are required.
func TestEmitSearchTraceLimit_RequiresBothInputRoles(t *testing.T) {
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
				Input:      &chplan.Scan{Table: "otel_traces", Columns: []string{tc.traceID, tc.timestamp}, Roles: []chplan.Column{{Name: tc.traceID, Role: chplan.RoleTraceID}, {Name: tc.timestamp, Role: chplan.RoleTimestamp}}},
				TraceLimit: 7,
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
