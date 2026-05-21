package logql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestIncludeLabelsFromBinop pins the surface of [includeLabelsFromBinop]:
// the helper returns the labels declared inside `group_left(...)` /
// `group_right(...)` of a binop's VectorMatching, and returns an empty
// (non-nil) slice when no Include list is present.
//
// Pre-threading work for #393 (LogQL include-labels through aggregation
// lowering).
func TestIncludeLabelsFromBinop(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "bare op has no include labels",
			query: `sum(rate({app="api"}[1m])) + sum(rate({app="db"}[1m]))`,
			want:  []string{},
		},
		{
			name:  "group_left single label",
			query: `sum by (svc) (rate({app="api"}[1m])) * on (svc) group_left(env) sum by (svc, env) (rate({app="meta"}[1m]))`,
			want:  []string{"env"},
		},
		{
			name:  "group_left multiple labels",
			query: `sum by (svc) (rate({app="api"}[1m])) * on (svc) group_left(env, region) sum by (svc, env, region) (rate({app="meta"}[1m]))`,
			want:  []string{"env", "region"},
		},
		{
			name:  "group_right with ignoring",
			query: `sum by (svc, level) (rate({app="meta"}[1m])) * ignoring(level) group_right(svc) sum by (svc) (rate({app="api"}[1m]))`,
			want:  []string{"svc"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			binop, ok := expr.(*syntax.BinOpExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q): expected *syntax.BinOpExpr, got %T", tc.query, expr)
			}

			got := includeLabelsFromBinop(binop)
			if got == nil {
				t.Fatalf("includeLabelsFromBinop returned nil slice; want non-nil")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("includeLabelsFromBinop = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIncludeLabelsFromBinopNil guards the defensive nil-input path so
// future call sites can pass a possibly-nil binop without panicking.
func TestIncludeLabelsFromBinopNil(t *testing.T) {
	t.Parallel()

	got := includeLabelsFromBinop(nil)
	if got == nil {
		t.Fatalf("includeLabelsFromBinop(nil) returned nil; want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("includeLabelsFromBinop(nil) = %v, want empty slice", got)
	}
}

// TestLowerBinaryRejectsNilLeg pins the defensive guard at the top of
// [lowerBinary]: when *either* the LHS (SampleExpr) or the RHS leg of a
// BinOpExpr is nil — a shape the upstream parser shouldn't normally
// hand us — the helper returns a clear error instead of dereferencing
// the nil leg and panicking.
//
// The guard reads `if b.SampleExpr == nil || b.RHS == nil`; a single-leg
// nil must trip it. Parse a real binop first so the rest of the struct
// (Op, Opts) stays parser-valid, then drop one leg to nil and confirm
// the helper rejects it.
func TestLowerBinaryRejectsNilLeg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*syntax.BinOpExpr)
	}{
		{
			name: "nil RHS",
			mut:  func(b *syntax.BinOpExpr) { b.RHS = nil },
		},
		{
			name: "nil LHS (SampleExpr)",
			mut:  func(b *syntax.BinOpExpr) { b.SampleExpr = nil },
		},
	}

	s := schema.DefaultOTelLogs()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(`rate({app="api"}[1m]) + rate({app="db"}[1m])`)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			binop, ok := expr.(*syntax.BinOpExpr)
			if !ok {
				t.Fatalf("expected *syntax.BinOpExpr, got %T", expr)
			}
			tc.mut(binop)

			_, err = lowerBinary(binop, s, lowerCtx{})
			if err == nil {
				t.Fatalf("lowerBinary with %s: want error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "nil leg") {
				t.Fatalf("lowerBinary with %s: error %q does not mention 'nil leg'", tc.name, err.Error())
			}
		})
	}
}

// TestVectorMatchingFromOptsEmptyLabels pins the no-op assignment of
// `match.Labels` when the parser hands us a VectorMatching that carries
// an empty MatchingLabels slice (e.g. a bare `on()` / `ignoring()`
// modifier, or no modifier at all). Both legs of the previously-guarded
// branch — `len(vm.MatchingLabels) > 0` true vs false — collapse to the
// same observable: `match.Labels == nil`, because `append([]string(nil),
// nil...)` and `append([]string(nil), []string{}...)` both return nil.
// The guard was therefore equivalent under append's nil-input semantics
// and was removed in favour of an unconditional clear-copy that also
// guarantees the caller never aliases the parser's slice. This test
// pins the post-refactor invariant so a future re-introduction of the
// guard cannot silently land a CONDITIONALS_BOUNDARY-equivalent shape.
func TestVectorMatchingFromOptsEmptyLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		vm   *syntax.VectorMatching
	}{
		{
			name: "nil VectorMatching",
			vm:   nil,
		},
		{
			name: "nil MatchingLabels slice",
			vm:   &syntax.VectorMatching{Card: syntax.CardOneToOne, MatchingLabels: nil},
		},
		{
			name: "empty MatchingLabels slice",
			vm:   &syntax.VectorMatching{Card: syntax.CardOneToOne, MatchingLabels: []string{}},
		},
		{
			name: "on() with empty MatchingLabels",
			vm:   &syntax.VectorMatching{Card: syntax.CardOneToOne, On: true, MatchingLabels: []string{}},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, match, _, err := vectorMatchingFromOpts(tc.vm)
			if err != nil {
				t.Fatalf("vectorMatchingFromOpts: %v", err)
			}
			if match.Labels != nil {
				t.Fatalf("match.Labels = %#v, want nil for empty MatchingLabels", match.Labels)
			}
		})
	}
}

// TestVectorMatchingFromOptsCopiesLabels pins the non-aliasing guarantee
// of the clear-copy assignment: when MatchingLabels carries entries,
// `match.Labels` MUST be a fresh slice with the same contents — not an
// alias of the parser's slice. Mutating one MUST NOT propagate to the
// other. Pins INVERT_LOOPCTRL / INCREMENT_DECREMENT mutations on the
// copy semantics as well as the equivalent CONDITIONALS_BOUNDARY mutant
// that was retired from the source.
func TestVectorMatchingFromOptsCopiesLabels(t *testing.T) {
	t.Parallel()

	src := []string{"svc", "env"}
	vm := &syntax.VectorMatching{Card: syntax.CardOneToOne, MatchingLabels: src}

	_, match, _, err := vectorMatchingFromOpts(vm)
	if err != nil {
		t.Fatalf("vectorMatchingFromOpts: %v", err)
	}
	if !reflect.DeepEqual(match.Labels, src) {
		t.Fatalf("match.Labels = %v, want %v", match.Labels, src)
	}
	// Mutate the source. If match.Labels were aliasing, this would
	// bleed into the copy.
	src[0] = "MUTATED"
	if match.Labels[0] != "svc" {
		t.Fatalf("match.Labels[0] = %q, want %q — slice aliasing leaked", match.Labels[0], "svc")
	}
}

// TestLowerVectorScalarBoolModifierGate pins the
// `isComparison(op) && returnBool` gate inside [lowerVectorScalar]. The
// guard determines whether the comparison's Bool-typed result is
// re-wrapped in `toFloat64(...)` before flowing into Value.
//
// An INVERT_LOGICAL mutant flips `&&` to `||`, causing two divergent
// behaviours:
//
//   - non-comparison op + returnBool == true: original keeps Value as
//     the raw Binary node; mutant wraps it in toFloat64. The Project's
//     Value projection differs by node type.
//   - comparison op + returnBool == false: the second guard
//     (`isComparison(op) && !returnBool`) short-circuits to a Filter
//     return, so the two mutants converge — this case can't tell them
//     apart, but the non-comparison case above already does.
//
// We exercise the non-comparison branch directly to surface the mutant.
// LogQL syntax doesn't permit `bool` on non-comparison ops, but the
// lowering function accepts any (op, returnBool) pair, so we call it
// with `op=OpAdd, returnBool=true` to pin the gate. The resulting
// Project's Value projection MUST be a `*chplan.Binary` — the mutant
// would wrap it in a `toFloat64` *chplan.FuncCall instead.
func TestLowerVectorScalarBoolModifierGate(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// `rate({app="api"}[1m])` lowers to a RangeWindow — the inner
	// shape `projectValueOverLogInner` recognises and projects to
	// (ResourceAttributes, Value).
	vecExpr, err := syntax.ParseExpr(`rate({app="api"}[1m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	node, err := lowerVectorScalar(vecExpr, s, chplan.OpAdd, 5, false /*scalarOnLeft*/, true /*returnBool*/, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerVectorScalar: %v", err)
	}

	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("lowerVectorScalar returned %T, want *chplan.Project", node)
	}

	// Find the Value projection. projectValueOverLogInner places
	// it under the `Value` alias (rangeAggSynthValueColumn).
	var valueExpr chplan.Expr
	for _, p := range proj.Projections {
		if p.Alias == rangeAggSynthValueColumn {
			valueExpr = p.Expr
			break
		}
	}
	if valueExpr == nil {
		t.Fatalf("lowerVectorScalar: Project has no `Value` projection (alias=%q)", rangeAggSynthValueColumn)
	}

	// Non-comparison op with returnBool=true: the gate is false in
	// the original (because `isComparison(OpAdd)` is false), so
	// Value stays as the raw Binary node. The mutant `||` flips the
	// gate to true and re-wraps Value in `toFloat64(...)`.
	if _, ok := valueExpr.(*chplan.Binary); !ok {
		t.Fatalf("lowerVectorScalar: Value projection is %T, want *chplan.Binary — the && gate was inverted (toFloat64 wrap leaked through on a non-comparison op)", valueExpr)
	}
}
