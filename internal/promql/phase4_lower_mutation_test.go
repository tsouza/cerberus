package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMutation_CloseNativeMatrixInput_NonColumnRefDeclines kills the
// INVERT_LOGICAL mutant on lower.go:closeNativeMatrixInput:
// `if !ok || ref.Name == ""` rewritten to `if !ok && ref.Name == ""`.
//
// A non-ColumnRef groupBy entry is a valid "cannot close this input" signal,
// and the original short-circuits on `!ok`, returning nil,false without
// reading `ref`. The mutant evaluates `ref.Name` on the nil *chplan.ColumnRef
// produced by the failed type assertion, panicking instead of declining.
func TestMutation_CloseNativeMatrixInput_NonColumnRefDeclines(t *testing.T) {
	t.Parallel()

	got, ok := closeNativeMatrixInput(
		nil,
		[]chplan.Expr{&chplan.LitInt{V: 1}},
		schema.DefaultOTelMetrics(),
	)
	if ok || got != nil {
		t.Fatalf("closeNativeMatrixInput(non-ColumnRef) = (%v, %v), want (nil, false)", got, ok)
	}
}
