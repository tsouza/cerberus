package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestExpHistogramDroppingVectorBinop_RequiresExactlyOneHistogramSide kills
// the two INVERT_LOGICAL mutants on expHistogramDroppingVectorBinop's
//
//	switch {
//	case lhsHist && !rhsHist:
//		...
//	case rhsHist && !lhsHist:
//		...
//	}
//
// each `&&` rewritten to `||` independently. No existing untagged test
// exercised this switch at all before this one — the mutual-exclusivity
// entry in histogram_native_availability_test.go's recognizer table calls
// expHistogramDroppingVectorBinop but never with an input where exactly
// one side is histogram-valued and the op is a genuine drop shape, so
// gremlins reported both `&&`s NOT COVERED.
//
// The distinguishing input is the one BOTH mutants mis-handle the same
// way: two plain FLOAT vector operands, neither histogram-valued. Under
// the original `&&`, both cases require their own side histogram-valued,
// so with `lhsHist == rhsHist == false` neither fires and the function
// correctly reports `ok == false`. Under either mutated `||`, the
// `!<other side>` operand alone is enough (`false || true == true`), so
// the mutated case fires anyway and misrecognises an ordinary
// float-float binary expression as a histogram-drop shape.
func TestExpHistogramDroppingVectorBinop_RequiresExactlyOneHistogramSide(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{}

	t.Run("histogram on left recognised", func(t *testing.T) {
		t.Parallel()
		b := mustParse(t, "latency_exp_hist + up").(*parser.BinaryExpr)
		histSide, floatSide, ok := expHistogramDroppingVectorBinop(b, s, ctx)
		if !ok {
			t.Fatal("expHistogramDroppingVectorBinop(hist + float) = false, want true")
		}
		if histSide != b.LHS || floatSide != b.RHS {
			t.Fatalf("expHistogramDroppingVectorBinop(hist + float) = (%#v, %#v), want (b.LHS, b.RHS)", histSide, floatSide)
		}
	})

	t.Run("histogram on right recognised", func(t *testing.T) {
		t.Parallel()
		b := mustParse(t, "up + latency_exp_hist").(*parser.BinaryExpr)
		histSide, floatSide, ok := expHistogramDroppingVectorBinop(b, s, ctx)
		if !ok {
			t.Fatal("expHistogramDroppingVectorBinop(float + hist) = false, want true")
		}
		if histSide != b.RHS || floatSide != b.LHS {
			t.Fatalf("expHistogramDroppingVectorBinop(float + hist) = (%#v, %#v), want (b.RHS, b.LHS)", histSide, floatSide)
		}
	})

	t.Run("neither side histogram rejected", func(t *testing.T) {
		t.Parallel()
		b := mustParse(t, "up + num_cpus").(*parser.BinaryExpr)
		if _, _, ok := expHistogramDroppingVectorBinop(b, s, ctx); ok {
			t.Fatal("expHistogramDroppingVectorBinop(float + float) = true, want false: neither operand is histogram-valued")
		}
	})
}
