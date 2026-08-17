package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestIsSupportedHistogramMatchCardinality pins the cardinality gate
// [applyVectorMatchToHistogramOperand]'s callers rely on: one-to-one
// (nil vm, or vm.Card explicitly CardOneToOne — covering both default
// and on()/ignoring() matching) is supported; group_left()/group_right()
// (CardManyToOne / CardOneToMany) is not.
func TestIsSupportedHistogramMatchCardinality(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		vm   *parser.VectorMatching
		want bool
	}{
		{name: "nil (default matching)", vm: nil, want: true},
		{name: "one-to-one, no modifier", vm: &parser.VectorMatching{Card: parser.CardOneToOne}, want: true},
		{name: "one-to-one with on()", vm: &parser.VectorMatching{Card: parser.CardOneToOne, On: true, MatchingLabels: []string{"job"}}, want: true},
		{name: "many-to-one (group_left)", vm: &parser.VectorMatching{Card: parser.CardManyToOne}, want: false},
		{name: "one-to-many (group_right)", vm: &parser.VectorMatching{Card: parser.CardOneToMany}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSupportedHistogramMatchCardinality(tc.vm); got != tc.want {
				t.Fatalf("isSupportedHistogramMatchCardinality(%#v) = %v, want %v", tc.vm, got, tc.want)
			}
		})
	}
}

// TestApplyVectorMatchToHistogramOperand_DefaultMatchingIsNoop pins that
// default matching (isDefaultMatching(vm) == true) returns hp UNCHANGED
// — no extra Aggregate stage, since hp already carries one row per
// full-Attributes key.
func TestApplyVectorMatchToHistogramOperand_DefaultMatchingIsNoop(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	hp := &chplan.HistogramProjection{Input: &chplan.Scan{Table: s.ExpHistogramTable}}
	ctx := lowerCtx{}

	for _, vm := range []*parser.VectorMatching{nil, {Card: parser.CardOneToOne}} {
		got := applyVectorMatchToHistogramOperand(hp, vm, s, ctx)
		if got != hp {
			t.Fatalf("applyVectorMatchToHistogramOperand(%#v) = %#v, want the unchanged input hp", vm, got)
		}
	}
}

// TestApplyVectorMatchToHistogramOperand_AddsAmbiguityGuard pins the
// shape a non-default (on()/ignoring()) match key builds: an extra
// chplan.Aggregate stage over hp, grouped by the reduced match key, with
// a non-nil Having guard — [histogramMatchAmbiguityGuardExpr]'s runtime
// "many-to-many matching not allowed" throwIf.
func TestApplyVectorMatchToHistogramOperand_AddsAmbiguityGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	hp := &chplan.HistogramProjection{Input: &chplan.Scan{Table: s.ExpHistogramTable}}
	vm := &parser.VectorMatching{Card: parser.CardOneToOne, On: true, MatchingLabels: []string{"service"}}

	prepHP := applyVectorMatchToHistogramOperand(hp, vm, s, lowerCtx{})

	reshape, ok := prepHP.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("prepared HistogramProjection.Input is %T, want *chplan.Project", prepHP.Input)
	}
	agg, ok := reshape.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("reshape.Input is %T, want *chplan.Aggregate", reshape.Input)
	}
	if agg.Input != chplan.Node(hp) {
		t.Fatalf("Aggregate.Input = %#v, want the original hp", agg.Input)
	}
	if agg.Having == nil {
		t.Fatal("Aggregate.Having is nil, want the ambiguity guard")
	}
	having, ok := agg.Having.(*chplan.Binary)
	if !ok || having.Op != chplan.OpEq {
		t.Fatalf("Aggregate.Having = %#v, want an Eq Binary (throwIf(...) = 0)", agg.Having)
	}
	call, ok := having.Left.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnThrowIf {
		t.Fatalf("Aggregate.Having.Left = %#v, want a throwIf FuncCall", having.Left)
	}
	if !agg.DropEmptyOnNoGroup {
		t.Fatal("Aggregate.DropEmptyOnNoGroup = false, want true so an empty operand answers empty, not one all-any() row")
	}
}

// TestHistogramMatchAmbiguityGuardExpr pins the guard's exact shape:
// `throwIf(uniqExact(mapSort(<attrsCol>)) > 1, ManyToManyMatchMessage) =
// 0` — the same ManyToManyMatchMessage chsql's VectorJoin plants, so a
// histogram binop's ambiguous match classifies through the identical
// HTTP error path.
func TestHistogramMatchAmbiguityGuardExpr(t *testing.T) {
	t.Parallel()

	got := histogramMatchAmbiguityGuardExpr("Attributes")

	eq, ok := got.(*chplan.Binary)
	if !ok || eq.Op != chplan.OpEq {
		t.Fatalf("guard = %#v, want an Eq Binary", got)
	}
	zero, ok := eq.Right.(*chplan.LitInt)
	if !ok || zero.V != 0 {
		t.Fatalf("guard.Right = %#v, want LitInt(0)", eq.Right)
	}
	throwIf, ok := eq.Left.(*chplan.FuncCall)
	if !ok || throwIf.Fn != chplan.FnThrowIf || len(throwIf.Args) != 2 {
		t.Fatalf("guard.Left = %#v, want a 2-arg throwIf FuncCall", eq.Left)
	}
	msg, ok := throwIf.Args[1].(*chplan.InlineString)
	if !ok || msg.V != chplan.ManyToManyMatchMessage {
		t.Fatalf("throwIf message = %#v, want InlineString(%q)", throwIf.Args[1], chplan.ManyToManyMatchMessage)
	}
	gt, ok := throwIf.Args[0].(*chplan.Binary)
	if !ok || gt.Op != chplan.OpGt {
		t.Fatalf("throwIf condition = %#v, want a Gt Binary", throwIf.Args[0])
	}
	uniq, ok := gt.Left.(*chplan.FuncCall)
	if !ok || uniq.Fn != chplan.FnUniqExact || len(uniq.Args) != 1 {
		t.Fatalf("guard's distinctness check = %#v, want a 1-arg uniqExact FuncCall", gt.Left)
	}
	canon, ok := uniq.Args[0].(*chplan.FuncCall)
	if !ok || canon.Fn != chplan.FnMapSort {
		t.Fatalf("uniqExact argument = %#v, want a mapSort-wrapped (canonicalised) Attributes reference", uniq.Args[0])
	}
}
