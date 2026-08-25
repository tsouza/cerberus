package promql

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestShadowResolveFloatArmChecked_RejectsReturnBool pins the ReturnBool
// guard shadowResolveFloatArmChecked (histogram_native_mixed_or_aggregate.go)
// centralises for scalar()/sort()/sort_desc()/the date-component
// functions — a pre-release audit finding's DRY cleanup, consolidating
// what used to be an identical 3-line guard duplicated at each of those
// composers' own call sites.
//
// PromQL's grammar rejects `bool` on a non-comparison operator (`a or
// bool b` is a parse error, verified directly), so this exercises the
// unexported helper against a hand-built AST rather than a parsed query —
// the same "defensive guard against an unreachable-through-any-parseable-
// query state" contract this codebase applies elsewhere (e.g.
// assertValueShapedInput, histogram_shape_guard.go). A future 4th
// composer skipping this guard on a synthetic/rewritten BinaryExpr would
// otherwise silently mis-lower rather than error.
func TestShadowResolveFloatArmChecked_RejectsReturnBool(t *testing.T) {
	t.Parallel()

	b := &parser.BinaryExpr{
		Op:         parser.LOR,
		ReturnBool: true,
		LHS:        &parser.VectorSelector{Name: "demo_latency_exp_hist"},
		RHS:        &parser.VectorSelector{Name: "demo_latency_exp_hist"},
	}

	_, err := shadowResolveFloatArmChecked(b, schema.DefaultOTelMetrics(), lowerCtx{})
	if err == nil {
		t.Fatal("shadowResolveFloatArmChecked(ReturnBool=true): expected an error, got success")
	}
	const wantErrSubstring = "'bool' modifier is only allowed on comparison binary ops"
	if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Fatalf("shadowResolveFloatArmChecked(ReturnBool=true) error = %q, want it to contain %q", err.Error(), wantErrSubstring)
	}
}
