package promql_test

import (
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
)

// TestTryFoldScalar — table-driven check of constant folding against
// Prom's exact semantics for every shape the function claims to handle.
func TestTryFoldScalar(t *testing.T) {
	t.Parallel()

	cases := []struct {
		query string
		want  float64
		ok    bool
	}{
		// NumberLiteral.
		{"42", 42, true},
		{"0.5", 0.5, true},
		{"1e3", 1000, true},
		{"NaN", math.NaN(), true},
		{"Inf", math.Inf(1), true},
		{"+Inf", math.Inf(1), true},
		{"-Inf", math.Inf(-1), true},

		// Unary.
		{"-1", -1, true},
		{"--5", 5, true},
		{"+3", 3, true},

		// Paren.
		{"(1)", 1, true},
		{"((42))", 42, true},

		// Arithmetic.
		{"1+1", 2, true},
		{"2*3-1", 5, true},
		{"10/4", 2.5, true},
		{"10%3", 1, true},
		{"2^10", 1024, true},
		{"(1+2)*3", 9, true},
		{"1+2*3", 7, true}, // precedence
		{"(1+2)*(3+4)", 21, true},

		// Division / modulo by zero — Prom semantics.
		{"1/0", math.Inf(1), true},
		{"-1/0", math.Inf(-1), true},
		// {"0/0", math.NaN(), true},      // checked separately because NaN != NaN
		// {"1%0", math.NaN(), true},

		// Vector selector / call — not foldable.
		{"up", 0, false},
		{"rate(metric[5m])", 0, false},
		{"sum(metric)", 0, false},
		{"1 + up", 0, false},
		{"up + 1", 0, false},

		// Comparison ops with the `bool` modifier — fold to 1.0/0.0.
		// Bare scalar-scalar comparisons (no `bool`) are rejected by
		// the Prom parser before reaching the fold path, so the only
		// shape that ever lands here carries ReturnBool=true.
		{"1 == bool 1", 1, true},
		{"1 == bool 2", 0, true},
		{"1 != bool 2", 1, true},
		{"1 < bool 2", 1, true},
		{"2 < bool 1", 0, true},
		{"1 <= bool 1", 1, true},
		{"1 > bool 2", 0, true},
		{"1 >= bool 1", 1, true},
		{"(1 < bool 2) * 10", 10, true},
	}

	p := parser.NewParser(parser.Options{})
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			got, ok := promql.TryFoldScalar(expr)
			if ok != tc.ok {
				t.Fatalf("ok: got %v, want %v (value: %v)", ok, tc.ok, got)
			}
			if !ok {
				return
			}
			switch {
			case math.IsNaN(tc.want) && !math.IsNaN(got):
				t.Errorf("got %v; want NaN", got)
			case !math.IsNaN(tc.want) && got != tc.want:
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

// TestTryFoldScalar_NaNCases — separate because NaN != NaN under ==.
func TestTryFoldScalar_NaNCases(t *testing.T) {
	t.Parallel()

	cases := []string{"0/0", "1%0", "NaN+1", "1-NaN"}
	p := parser.NewParser(parser.Options{})
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("parse %q: %v", q, err)
			}
			got, ok := promql.TryFoldScalar(expr)
			if !ok {
				t.Fatalf("expected fold ok; got false")
			}
			if !math.IsNaN(got) {
				t.Errorf("got %v; want NaN", got)
			}
		})
	}
}
