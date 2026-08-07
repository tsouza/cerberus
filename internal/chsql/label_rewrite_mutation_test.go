package chsql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The label-rewrite renderers (exprLabelReplace, labelReplaceSubstitution,
// labelReplaceSegment, srcValue, exprLabelJoin) reach the emitter only through
// PromQL lowering, so for a long time nothing in this package rendered one and
// their mutants were reported NOT COVERED rather than killed. Coverage that
// arrives incidentally — a plan-shape test that happens to lower
// `label_replace(...)` and only asserts the outermost SELECT list — turns that
// exclusion into a survival: the mutants become COVERED and LIVED without a
// single new assertion about the SQL these functions actually produce.
//
// The tests below render each shape directly and pin the whole emitted string
// together with its bound arguments. A renderer that returns early, drops a
// recursion, or misplaces a separator changes that string, so every branch in
// these five functions is asserted rather than merely executed.

// attrsMap is the Map(String, String) input every label-rewrite expression
// takes in practice: the Attributes column of a Sample row.
func attrsMap() chplan.Expr { return &chplan.ColumnRef{Name: "Attributes"} }

// renderExpr renders e and fails the test if the builder rejects it.
func renderExpr(t *testing.T, e chplan.Expr) (string, []any) {
	t.Helper()

	b := NewBuilder()
	if err := b.Expr(e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args, err := b.Build()
	if err != nil {
		t.Fatalf("unexpected build error: %v", err)
	}
	return sql, args
}

// assertRender compares the rendered SQL and bound arguments against the
// expected pair, reporting both halves so a failure names what moved.
func assertRender(t *testing.T, gotSQL string, gotArgs []any, wantSQL string, wantArgs []any) {
	t.Helper()

	if gotSQL != wantSQL {
		t.Errorf("SQL mismatch\n got: %s\nwant: %s", gotSQL, wantSQL)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("args mismatch\n got: %#v\nwant: %#v", gotArgs, wantArgs)
	}
}

// TestMutation_ExprLabelReplace_TemplateForm pins the whole rendering of the
// `replaceRegexpOne` form — the shape taken when the replacement template
// carries no capture reference above CH's substitution ceiling, so
// chplan.LabelReplace describes the value with Replacement and leaves Segments
// empty.
//
// Kills every CONDITIONALS_NEGATION on the `err != nil` guards in
// exprLabelReplace (the four Map recursions and the substitution call),
// labelReplaceSubstitution and srcValue: negated to `err == nil`, each guard
// fires on the success path and returns nil early, truncating the SQL at that
// point. It also kills labelReplaceSubstitution's `len(l.Segments) == 0`
// guard, which under negation sends an empty decomposition down the concat
// path and renders `concat()`.
func TestMutation_ExprLabelReplace_TemplateForm(t *testing.T) {
	t.Parallel()

	sql, args := renderExpr(t, &chplan.LabelReplace{
		Map:              attrsMap(),
		Dst:              "dst",
		Replacement:      "v-$1",
		Src:              "src",
		Regex:            "(.*)",
		EmptyReplacement: "",
	})

	assertRender(
		t, sql, args,
		"mapFilter((k, v) -> v != '', if(match(`Attributes`[?], ?), "+
			"mapUpdate(`Attributes`, map(?, if(empty(`Attributes`[?]), ?, "+
			"replaceRegexpOne(`Attributes`[?], ?, ?)))), `Attributes`))",
		[]any{"src", "^(.*)$", "dst", "src", "", "src", "^(.*)$", "v-$1"},
	)
}

// TestMutation_ExprLabelReplace_SegmentForm pins the `concat` over
// `extractGroups` form, exercising all three segment kinds in one template:
// a literal run, the whole-match group read off the source value, and a
// capture group above the `\9` ceiling that the template path cannot express.
//
// Kills labelReplaceSegment's switch arms — a segment routed to the wrong arm
// renders a bound literal where an `extractGroups` subscript belongs, or vice
// versa — and the `err != nil` guard on its srcValue recursion.
func TestMutation_ExprLabelReplace_SegmentForm(t *testing.T) {
	t.Parallel()

	const groupAboveTemplateCeiling = 12

	sql, args := renderExpr(t, &chplan.LabelReplace{
		Map:              attrsMap(),
		Dst:              "dst",
		Src:              "src",
		Regex:            "(.*)",
		EmptyReplacement: "empty",
		Segments: []chplan.LabelReplaceSegment{
			{Literal: "pre-", Group: chplan.NoCaptureGroup},
			{Group: chplan.WholeMatchGroup},
			{Group: groupAboveTemplateCeiling},
		},
	})

	assertRender(
		t, sql, args,
		"mapFilter((k, v) -> v != '', if(match(`Attributes`[?], ?), "+
			"mapUpdate(`Attributes`, map(?, if(empty(`Attributes`[?]), ?, "+
			"concat(?, `Attributes`[?], extractGroups(`Attributes`[?], ?)[?])))), `Attributes`))",
		[]any{
			"src", "^(.*)$", "dst", "src", "empty",
			"pre-", "src", "src", "^(.*)$", int64(groupAboveTemplateCeiling),
		},
	)
}

// TestMutation_ExprLabelReplace_MapErrorPropagates pins that an unrenderable
// Map reaches the caller rather than rendering as silently truncated SQL.
// Paired with the shape assertions above so it cannot pass by rejecting every
// LabelReplace.
func TestMutation_ExprLabelReplace_MapErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.LabelReplace{
		Map:   errExpr(),
		Dst:   "dst",
		Src:   "src",
		Regex: ".*",
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("LabelReplace with an unrenderable Map must return ErrUnsupported, got %v", err)
	}
}

// TestMutation_ExprLabelJoin_MultiSourceSeparator pins the whole rendering of
// label_join over more than one source label. Two sources are the minimum that
// distinguishes the separator's position.
//
// Kills exprLabelJoin's `i > 0` guard under both mutators —
// CONDITIONALS_BOUNDARY (`i >= 0`) prefixes the array with a separator before
// the first element, CONDITIONALS_NEGATION (`i <= 0`) moves it there and drops
// the one between — and the CONDITIONALS_NEGATION on both `err != nil` guards,
// which return nil early on the success path and truncate the SQL.
func TestMutation_ExprLabelJoin_MultiSourceSeparator(t *testing.T) {
	t.Parallel()

	sql, args := renderExpr(t, &chplan.LabelJoin{
		Map:       attrsMap(),
		Dst:       "dst",
		Separator: "-",
		Srcs:      []string{"a", "b"},
	})

	assertRender(
		t, sql, args,
		"mapFilter((k, v) -> v != '', mapUpdate(`Attributes`, "+
			"map(?, arrayStringConcat([`Attributes`[?], `Attributes`[?]], ?))))",
		[]any{"dst", "a", "b", "-"},
	)

	if strings.Contains(sql, "[, ") {
		t.Errorf("separator must not precede the first source: %s", sql)
	}
}

// TestMutation_ExprLabelJoin_MapErrorPropagates pins that an unrenderable Map
// reaches the caller, for the same reason as the LabelReplace sibling.
func TestMutation_ExprLabelJoin_MapErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.LabelJoin{
		Map:  errExpr(),
		Dst:  "dst",
		Srcs: []string{"a"},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("LabelJoin with an unrenderable Map must return ErrUnsupported, got %v", err)
	}
}
