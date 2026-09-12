package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func setOpSchemaScan(table, traceID, spanID string) *chplan.Scan {
	return &chplan.Scan{
		Table:   table,
		Columns: []string{traceID, spanID},
		Roles: []chplan.Column{
			{Name: traceID, Role: chplan.RoleTraceID},
			{Name: spanID, Role: chplan.RoleSpanID},
		},
	}
}

func TestSetOperationResolvesEachChildIdentitySchema(t *testing.T) {
	left := setOpSchemaScan("left_spans", "left_trace", "left_span")
	left.Columns = []string{"left_trace", "left_span"}
	right := setOpSchemaScan("right_spans", "right_trace", "right_span")
	right.Columns = []string{"right_trace", "right_span"}
	plan := &chplan.SetOperation{
		Left:          left,
		Right:         right,
		Op:            chplan.SetUnion,
		TraceIDColumn: "left_trace",
		SpanIDColumn:  "left_span",
	}

	sqlText, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"SELECT `left_trace`, `left_span` FROM `left_spans`",
		"SELECT `right_trace`, `right_span` FROM `right_spans`",
		"LIMIT 1 BY `left_trace`, `left_span`",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("SQL missing %q: %s", want, sqlText)
		}
	}
}

func TestSetOperationRejectsMissingChildIdentityRole(t *testing.T) {
	right := setOpSchemaScan("right_spans", "right_trace", "right_span")
	right.Roles = right.Roles[:1]
	plan := &chplan.SetOperation{
		Left:          setOpSchemaScan("left_spans", "left_trace", "left_span"),
		Right:         right,
		Op:            chplan.SetUnion,
		TraceIDColumn: "left_trace",
		SpanIDColumn:  "left_span",
	}

	_, _, err := chsql.Emit(context.Background(), plan)
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Fatalf("Emit error = %v, want ErrUnsupported", err)
	}
}

func TestSetOperationRejectsPositionallyMisalignedChildIdentityRoles(t *testing.T) {
	left := setOpSchemaScan("left_spans", "left_trace", "left_span")
	right := setOpSchemaScan("right_spans", "right_trace", "right_span")
	right.Columns = []string{"right_span", "right_trace"}
	right.Roles = []chplan.Column{
		{Name: "right_span", Role: chplan.RoleSpanID},
		{Name: "right_trace", Role: chplan.RoleTraceID},
	}

	_, _, err := chsql.Emit(context.Background(), &chplan.SetOperation{
		Left: left, Right: right, Op: chplan.SetUnion,
		TraceIDColumn: "left_trace", SpanIDColumn: "left_span",
	})
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Fatalf("Emit error = %v, want ErrUnsupported", err)
	}
}

func TestSetOperationRejectsDifferentChildSchemaWidths(t *testing.T) {
	left := setOpSchemaScan("left_spans", "left_trace", "left_span")
	right := setOpSchemaScan("right_spans", "right_trace", "right_span")
	right.Columns = append(right.Columns, "extra")
	right.Roles = append(right.Roles, chplan.Column{Name: "extra", Role: chplan.RoleAttributes})

	_, _, err := chsql.Emit(context.Background(), &chplan.SetOperation{
		Left: left, Right: right, Op: chplan.SetUnion,
		TraceIDColumn: "left_trace", SpanIDColumn: "left_span",
	})
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Fatalf("Emit error = %v, want ErrUnsupported", err)
	}
}

func TestSetOperationRejectsOutputIdentityNotAliasedByLeftSchema(t *testing.T) {
	for name, identity := range map[string][2]string{
		"trace": {"declared_trace", "left_span"},
		"span":  {"left_trace", "declared_span"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := chsql.Emit(context.Background(), &chplan.SetOperation{
				Left:  setOpSchemaScan("left_spans", "left_trace", "left_span"),
				Right: setOpSchemaScan("right_spans", "right_trace", "right_span"),
				Op:    chplan.SetUnion, TraceIDColumn: identity[0], SpanIDColumn: identity[1],
			})
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit error = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestSetOperationRejectsAmbiguousChildIdentitySchema(t *testing.T) {
	closed := func(columns []string, roles []chplan.Column) *chplan.Scan {
		return &chplan.Scan{Table: "spans", Columns: columns, Roles: roles}
	}
	valid := func() *chplan.Scan { return setOpSchemaScan("spans", "trace", "span") }
	tests := map[string]*chplan.Scan{
		"open":                   {Table: "spans", Roles: []chplan.Column{{Name: "trace", Role: chplan.RoleTraceID}, {Name: "span", Role: chplan.RoleSpanID}}},
		"duplicate trace":        closed([]string{"trace_a", "trace_b", "span"}, []chplan.Column{{Name: "trace_a", Role: chplan.RoleTraceID}, {Name: "trace_b", Role: chplan.RoleTraceID}, {Name: "span", Role: chplan.RoleSpanID}}),
		"duplicate span":         closed([]string{"trace", "span_a", "span_b"}, []chplan.Column{{Name: "trace", Role: chplan.RoleTraceID}, {Name: "span_a", Role: chplan.RoleSpanID}, {Name: "span_b", Role: chplan.RoleSpanID}}),
		"unnamed trace":          closed([]string{"", "span"}, []chplan.Column{{Role: chplan.RoleTraceID}, {Name: "span", Role: chplan.RoleSpanID}}),
		"unnamed span":           closed([]string{"trace", ""}, []chplan.Column{{Name: "trace", Role: chplan.RoleTraceID}, {Role: chplan.RoleSpanID}}),
		"shared identity name":   closed([]string{"identity", "identity"}, []chplan.Column{{Name: "identity", Role: chplan.RoleTraceID}, {Name: "identity", Role: chplan.RoleSpanID}}),
		"conflicting trace name": closed([]string{"trace", "trace", "span"}, []chplan.Column{{Name: "trace", Role: chplan.RoleTraceID}, {Name: "trace", Role: chplan.RoleAttributes}, {Name: "span", Role: chplan.RoleSpanID}}),
	}
	for name, malformed := range tests {
		for _, side := range []string{"left", "right"} {
			t.Run(name+"/"+side, func(t *testing.T) {
				left, right := chplan.Node(valid()), chplan.Node(valid())
				if side == "left" {
					left = malformed
				} else {
					right = malformed
				}
				_, _, err := chsql.Emit(context.Background(), &chplan.SetOperation{
					Left: left, Right: right, Op: chplan.SetUnion,
					TraceIDColumn: "trace", SpanIDColumn: "span",
				})
				if !errors.Is(err, chsql.ErrUnsupported) {
					t.Fatalf("Emit error = %v, want ErrUnsupported", err)
				}
			})
		}
	}
}

func TestSetOperationRejectsNilChild(t *testing.T) {
	for _, side := range []string{"left", "right"} {
		t.Run(side, func(t *testing.T) {
			left, right := chplan.Node(setOpSchemaScan("spans", "trace", "span")), chplan.Node(setOpSchemaScan("spans", "trace", "span"))
			if side == "left" {
				left = nil
			} else {
				right = nil
			}
			_, _, err := chsql.Emit(context.Background(), &chplan.SetOperation{
				Left: left, Right: right, Op: chplan.SetUnion,
				TraceIDColumn: "trace", SpanIDColumn: "span",
			})
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit error = %v, want ErrUnsupported", err)
			}
		})
	}
}
