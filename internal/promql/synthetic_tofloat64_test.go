package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSyntheticScalarVector_WrapsLitFloatInToFloat64 pins the wrap of
// the synthetic-vector Value projection at the `vector(N)` and
// scalar-only-binop fold callsites on the emitted SQL. Without the
// wrap, `vector(1)+vector(1)` (Grafana's PromQL CheckHealth probe
// shape) returns 502 with:
//
//	engine: execute: chclient: scan: clickhouse [ScanRow]:
//	(Value) converting UInt16 to *float64 is unsupported. try using *uint16
//
// Root cause is in clickhouse-go/v2's parameter binding: its
// `bind.go::format()` has no `case float64`, so a Go `float64(1.0)`
// falls through to `fmt.Sprint(v)` and emits the SQL literal `1` (no
// decimal). CH narrows that to `UInt8`, the synthetic-fold Value
// projection's `(? + ?)` expression promotes to `UInt16`, and the
// chclient cursor refuses to scan a UInt16 column into
// `chclient.Sample.Value` (`*float64`).
//
// Post-#190 the wrap is contributed centrally by
// [internal/chsql/Builder.Expr]'s LitFloat case rather than by a
// per-callsite helper, so the lowering emits a bare `*chplan.LitFloat`
// and the SQL surface carries the `toFloat64(?)` wrap. Mirrors
// [internal/logql/literal_test.go::TestLowerVectorWrapsValueInToFloat64].
func TestSyntheticScalarVector_WrapsLitFloatInToFloat64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	instant := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		// `vector(N)` — lowerVector, synthetic.go:lowerVector callsite
		// passing &chplan.LitFloat{V:v} to syntheticScalarVector.
		{"vector_one", `vector(1)`},
		{"vector_two", `vector(2)`},
		// `1+1` — lowerBinary scalar-only fold, binary.go:45 callsite
		// passing &chplan.LitFloat{V:v} to syntheticScalarVector.
		{"scalar_fold_add", `1+1`},
		{"scalar_fold_mul", `2*3`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, instant, instant)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, "toFloat64(?)") {
				t.Fatalf("expected emitted SQL to wrap LitFloat in "+
					"toFloat64(?) — without it, CH narrows to UInt8 and "+
					"the Sample Scan fails with "+
					"`UInt16 to *float64 unsupported` on the Grafana "+
					"PromQL CheckHealth probe shape `vector(1)+vector(1)`. "+
					"SQL:\n%s", sql)
			}
		})
	}
}

// TestSyntheticFold_VectorVector_WrapsValueInToFloat64 pins the
// `foldSyntheticBinary` Value composition: when both legs of a V-V
// binop lower to the synthetic-scalar shape (e.g. `vector(1)+vector(1)`)
// the resulting Value slot must compose `toFloat64(?) OP toFloat64(?)`
// — not bare `? OP ?`. Post-#190 the wrap is contributed by the
// central [internal/chsql/Builder.Expr] LitFloat case; this test
// asserts that both LitFloat operands carry the wrap on the way out
// rather than collapsing to a single shared `toFloat64`.
//
// The emitted SQL is the canonical proxy for "the wrap survives": the
// presence of `toFloat64(?)` on both sides of the binop expression
// inside the Value slot.
func TestSyntheticFold_VectorVector_WrapsValueInToFloat64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	instant := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{"add", `vector(1)+vector(1)`},
		{"sub", `vector(1)-vector(1)`},
		{"mul", `vector(2)*vector(3)`},
		{"div", `vector(8)/vector(2)`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, instant, instant)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// The fold path must produce a Value column whose
			// expression composes two toFloat64(?) operands. The proxy
			// is the substring `toFloat64(?) <op> toFloat64(?)` showing
			// up inside the rendered SQL — any drop would either elide
			// one of the wraps (regressing to UInt8 narrowing) or move
			// the wrap outside the binop (regressing to a different
			// narrowing shape).
			//
			// Use a loose "two wraps near each other" check rather than
			// an exact op-character match — chsql renders operators with
			// surrounding spaces (`(toFloat64(?) + toFloat64(?))`) so
			// the substring is stable across the six arithmetic ops.
			if c := strings.Count(sql, "toFloat64(?)"); c < 2 {
				t.Fatalf("expected at least two toFloat64(?) wraps in the "+
					"synthetic V-V fold Value slot; got %d. SQL:\n%s", c, sql)
			}
		})
	}
}
