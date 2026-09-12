package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func searchLimitSchemaNode(columns []string, roles []chplan.Column) *chplan.SearchTraceLimit {
	return &chplan.SearchTraceLimit{
		Input:      &chplan.Scan{Table: "custom_traces", Columns: columns, Roles: roles},
		TraceLimit: 7,
	}
}

func TestEmitSearchTraceLimitRejectsInvalidInputSchemas(t *testing.T) {
	valid := []chplan.Column{
		{Name: "custom_trace", Role: chplan.RoleTraceID},
		{Name: "custom_time", Role: chplan.RoleTimestamp},
	}
	cases := []struct {
		name string
		node *chplan.SearchTraceLimit
	}{
		{"nil input", &chplan.SearchTraceLimit{TraceLimit: 7}},
		{"open schema", &chplan.SearchTraceLimit{Input: &chplan.Scan{Table: "custom_traces", Roles: valid}, TraceLimit: 7}},
		{"duplicate trace role", searchLimitSchemaNode([]string{"trace_a", "trace_b", "time"}, []chplan.Column{
			{Name: "trace_a", Role: chplan.RoleTraceID}, {Name: "trace_b", Role: chplan.RoleTraceID}, {Name: "time", Role: chplan.RoleTimestamp},
		})},
		{"duplicate timestamp role", searchLimitSchemaNode([]string{"trace", "time_a", "time_b"}, []chplan.Column{
			{Name: "trace", Role: chplan.RoleTraceID}, {Name: "time_a", Role: chplan.RoleTimestamp}, {Name: "time_b", Role: chplan.RoleTimestamp},
		})},
		{"unnamed trace role", searchLimitSchemaNode([]string{"", "time"}, []chplan.Column{
			{Name: "", Role: chplan.RoleTraceID}, {Name: "time", Role: chplan.RoleTimestamp},
		})},
		{"shared role name", searchLimitSchemaNode([]string{"identity", "identity"}, []chplan.Column{
			{Name: "identity", Role: chplan.RoleTraceID}, {Name: "identity", Role: chplan.RoleTimestamp},
		})},
		{"same-name conflict", searchLimitSchemaNode([]string{"trace", "time", "trace"}, []chplan.Column{
			{Name: "trace", Role: chplan.RoleTraceID}, {Name: "time", Role: chplan.RoleTimestamp}, {Name: "trace"},
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Emit(context.Background(), tc.node)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Emit error = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestEmitSearchTraceLimitUsesCustomPhysicalRoleNames(t *testing.T) {
	node := searchLimitSchemaNode([]string{"custom_trace", "custom_time"}, []chplan.Column{
		{Name: "custom_trace", Role: chplan.RoleTraceID},
		{Name: "custom_time", Role: chplan.RoleTimestamp},
	})
	sqlText, _, err := Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"SELECT `custom_trace`",
		"GROUP BY `custom_trace`",
		"ORDER BY min(`custom_time`) DESC, `custom_trace` LIMIT 7",
		"WHERE `custom_trace` GLOBAL IN",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("SQL missing %q: %s", want, sqlText)
		}
	}
}
