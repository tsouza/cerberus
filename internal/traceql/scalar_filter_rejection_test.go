package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerScalarFilter_RejectsUnsupportedOperandShapes pins #1984: the
// scalar-filter operand surface cerberus accepts is exactly the surface
// reference Tempo accepts — a single aggregate on the left, a literal on
// the right (pkg/traceql/ast_validate.go's two switches).
//
// #1708 / #1981 briefly widened cerberus past that, lowering arithmetic
// between aggregates and an aggregate on the RHS. Reference Tempo answers
// 400 for every query below, so cerberus answering them made it a strict
// super-set on a shape no Tempo-targeting client can send. These cases are
// the cerberus half of the parity claim; the reference half is asserted
// live, on every compat run, by the rejection-parity catalogue entries for
// the two error sites (test/rejection-parity/, class=rejection — the driver
// requires BOTH backends to reject).
//
// Every query here PARSES — cerberus's TraceQL grammar accepts the shape,
// exactly as upstream's does — so each case proves the rejection lives in
// lowering and is reachable from the wire, not smuggled in at parse time.
func TestLowerScalarFilter_RejectsUnsupportedOperandShapes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "arithmetic_between_aggregates_lhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) - min(duration) >= 0`,
			wantErr: "scalar filter lhs of type",
		},
		{
			name:    "nested_arithmetic_lhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) - min(duration) / avg(duration) >= 0.5`,
			wantErr: "scalar filter lhs of type",
		},
		{
			name:    "literal_lhs",
			query:   `{ resource.service.name = "frontend" } | 1 > 2`,
			wantErr: "scalar filter lhs of type",
		},
		{
			name:    "aggregate_on_rhs",
			query:   `{ resource.service.name = "frontend" } | max(duration) > avg(duration)`,
			wantErr: "scalar filter rhs of type",
		},
		{
			name:    "arithmetic_on_rhs",
			query:   `{ resource.service.name = "frontend" } | count() > max(duration) - min(duration)`,
			wantErr: "scalar filter rhs of type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v — the shape must PARSE for this rejection to be a lowering "+
					"rejection reachable from the wire", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q) succeeded (plan %T) — reference Tempo answers 400 for this shape, so "+
					"accepting it makes cerberus a strict super-set of the reference (#1984)", tc.query, plan)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Lower(%q) error = %q, want it to contain %q", tc.query, err.Error(), tc.wantErr)
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
