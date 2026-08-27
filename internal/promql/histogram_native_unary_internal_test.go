package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestUnaryOverExpHistogram_GuardHappyPath pins the ordinary, fully-enabled
// case: a real ExpHistogramTable, no metadataFullRange. This is the only
// input combination that can actually observe the guard's own comparison
// operator (`s.ExpHistogramTable == ""`) rather than a downstream,
// independently-gated recognizer masking it — see this file's other test
// for why the table-empty / metadataFullRange=true combinations cannot.
func TestUnaryOverExpHistogram_GuardHappyPath(t *testing.T) {
	t.Parallel()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()

	mustParse := func(q string) parser.Expr {
		e, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		return e
	}

	cases := []struct {
		name   string
		query  string
		wantOp parser.ItemType
	}{
		{"minus", "-latency_exp_hist", parser.SUB},
		{"plus", "+latency_exp_hist", parser.ADD},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			operand, op, ok := unaryOverExpHistogram(mustParse(tc.query), s, lowerCtx{})
			if !ok {
				t.Fatalf("unaryOverExpHistogram(%q) ok = false, want true", tc.query)
			}
			if op != tc.wantOp {
				t.Errorf("unaryOverExpHistogram(%q) op = %v, want %v", tc.query, op, tc.wantOp)
			}
			if operand == nil {
				t.Errorf("unaryOverExpHistogram(%q) operand = nil, want the wrapped expression", tc.query)
			}
		})
	}
}

// TestUnaryOverExpHistogram_RejectsNonAddSubUnary pins that the operator
// check only ever accepts SUB (`-`) and ADD (`+`) — PromQL's own parser
// never actually produces a unary `*`/`/`/etc, so this calls the
// recognizer directly with a hand-built AST node to exercise the branch a
// real query can't reach, pinning `u.Op != parser.SUB && u.Op != parser.ADD`
// as a genuine two-way AND: SUB alone must pass the first clause and MUL
// must fail neither is negated away.
func TestUnaryOverExpHistogram_RejectsNonAddSubUnary(t *testing.T) {
	t.Parallel()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()

	expr, err := p.ParseExpr("latency_exp_hist")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	bogus := &parser.UnaryExpr{Op: parser.MUL, Expr: expr}
	if _, _, ok := unaryOverExpHistogram(bogus, s, lowerCtx{}); ok {
		t.Fatalf("unaryOverExpHistogram(unary MUL) ok = true, want false (only SUB/ADD are unary ops)")
	}
}

// TestUnaryOverExpHistogram_NotUnaryRejects pins that a non-unary
// expression (no `+`/`-` prefix at all) is rejected outright.
func TestUnaryOverExpHistogram_NotUnaryRejects(t *testing.T) {
	t.Parallel()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()

	expr, err := p.ParseExpr("latency_exp_hist")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	if _, _, ok := unaryOverExpHistogram(expr, s, lowerCtx{}); ok {
		t.Fatalf("unaryOverExpHistogram(bare selector) ok = true, want false")
	}
}
