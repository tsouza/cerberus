package traceql_test

import (
	"context"
	"errors"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerScalarFilter_RejectsUnsupportedOperandShapes pins #1984, which
// withdrew the wider operand surface #1708 / #1981 had lowered: the
// scalar-filter operand surface cerberus accepts is exactly the surface
// reference Tempo accepts — a single aggregate on the left, a literal on
// the right. Every other shape is rejected by ast.Parse's validation, with
// Tempo's own message, so none reaches lowering: the Tempo head answers the
// parse-stage rejection with 400, as Tempo's query frontend does.
//
// `{}|0>0` is the input the retired weekly fuzz lane found (#324): a literal
// on the left once reached an unchecked type assertion in lowering.
func TestLowerScalarFilter_RejectsUnsupportedOperandShapes(t *testing.T) {
	t.Parallel()

	const (
		lhs = "invalid TraceQL query: scalar filter lhs of type (%v) not yet supported"
		rhs = "invalid TraceQL query: scalar filter rhs of type (%v) not yet supported"
	)
	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "arithmetic_between_aggregates_lhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) - min(duration) >= 0`,
			wantErr: lhs,
		},
		{
			name:    "nested_arithmetic_lhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) - min(duration) / avg(duration) >= 0.5`,
			wantErr: lhs,
		},
		{
			name:    "literal_lhs",
			query:   `{ resource.service.name = "frontend" } | 1 > 2`,
			wantErr: lhs,
		},
		{
			name:    "literal_lhs_fuzz_324",
			query:   `{}|0>0`,
			wantErr: lhs,
		},
		{
			name:    "aggregate_on_rhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) > avg(duration)`,
			wantErr: rhs,
		},
		{
			name:    "arithmetic_on_rhs",
			query:   `{ resource.service.name = "frontend" } | count() > max(duration) - min(duration)`,
			wantErr: rhs,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := tempo.Parse(tc.query)
			var verr *tempo.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse(%q) error = %T (%v), want *ast.ValidationError — reference Tempo rejects this "+
					"shape in Validate, so cerberus must reject it before lowering", tc.query, err, err)
			}
			if got := verr.Error(); got != tc.wantErr {
				t.Fatalf("Parse(%q) error = %q, want %q", tc.query, got, tc.wantErr)
			}
		})
	}
}

// TestLowerScalarFilter_AcceptsAggregateVersusLiteral is the other half of
// the boundary: the one shape reference Tempo DOES accept must still lower,
// and must still produce the Filter-over-Aggregate plan the search-envelope
// detection in internal/api/tempo depends on. Without this, the rejection
// test above would pass just as well if lowering rejected every scalar
// filter outright.
func TestLowerScalarFilter_AcceptsAggregateVersusLiteral(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, query := range []string{
		`{ resource.service.name = "frontend" } | count() > 0`,
		`{ resource.service.name = "frontend" } | max(duration) >= 0`,
		`{ resource.service.name = "frontend" } | avg(duration) < 5`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v — this is the shape reference Tempo accepts; it must keep lowering", query, err)
			}
			filter, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("Lower(%q) root = %T, want *chplan.Filter", query, plan)
			}
			if _, ok := filter.Input.(*chplan.Aggregate); !ok {
				t.Fatalf("Lower(%q) Filter.Input = %T, want *chplan.Aggregate — the accepted shape lowers to a "+
					"Filter directly over the lone Aggregate, with no intermediate Project", query, filter.Input)
			}
		})
	}
}
