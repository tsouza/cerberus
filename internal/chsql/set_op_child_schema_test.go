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
		Table: table,
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
