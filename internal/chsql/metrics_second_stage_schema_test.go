package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestEmitMetricsSecondStageResolvesValueRole(t *testing.T) {
	t.Parallel()

	input := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "physical_value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	cases := []struct {
		name string
		plan *chplan.MetricsSecondStage
		want string
	}{
		{"topk", &chplan.MetricsSecondStage{Input: input, Op: chplan.SecondStageTopK, K: 3}, "ORDER BY `physical_value` DESC"},
		{"bottomk", &chplan.MetricsSecondStage{Input: input, Op: chplan.SecondStageBottomK, K: 3}, "ORDER BY `physical_value` LIMIT"},
		{"threshold", &chplan.MetricsSecondStage{Input: input, Op: chplan.SecondStageThreshold, ThresholdOp: chplan.OpGt, ThresholdValue: 2}, "`physical_value` >"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := chsql.Emit(context.Background(), tc.plan)
			if err != nil {
				t.Fatalf("Emit() error = %v", err)
			}
			if !strings.Contains(sql, tc.want) {
				t.Fatalf("Emit() SQL does not use child RoleValue column:\n%s", sql)
			}
		})
	}
}

func TestEmitMetricsSecondStageRejectsInvalidValueSchema(t *testing.T) {
	t.Parallel()

	project := func(columns ...chplan.Column) chplan.Node {
		projections := make([]chplan.Projection, len(columns))
		for i, column := range columns {
			projections[i] = chplan.Projection{Expr: &chplan.LitInt{V: int64(i)}, Alias: column.Name}
		}
		return &chplan.Project{Input: &chplan.Scan{Table: "otel_traces"}, Projections: projections, Roles: columns}
	}
	cases := []struct {
		name  string
		input chplan.Node
	}{
		{"open", &chplan.Scan{Table: "otel_traces"}},
		{"missing", project(chplan.Column{Name: "opaque"})},
		{"duplicate", project(
			chplan.Column{Name: "value_a", Role: chplan.RoleValue},
			chplan.Column{Name: "value_b", Role: chplan.RoleValue},
		)},
		{"unnamed", &chplan.MetricsAggregate{Op: chplan.MetricsOpRate, Inner: &chplan.Scan{Table: "otel_traces"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(context.Background(), &chplan.MetricsSecondStage{
				Input: tc.input,
				Op:    chplan.SecondStageTopK,
				K:     3,
			})
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit() error = %v, want ErrUnsupported", err)
			}
		})
	}
}
