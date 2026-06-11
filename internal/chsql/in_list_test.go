package chsql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestExprInList_Flat pins the rendered shape: a single parenthesised
// `IN` tuple with one positional placeholder per literal element. The
// flat tuple is the whole point of chplan.InList — the equivalent
// OR-chain nests one ClickHouse AST level per element and trips
// max_parser_depth (default 1000, code 306) around 1000 elements.
func TestExprInList_Flat(t *testing.T) {
	t.Parallel()

	b := &Builder{}
	err := b.Expr(&chplan.InList{
		Left: &chplan.ColumnRef{Name: "TraceId"},
		List: []chplan.Expr{
			&chplan.LitString{V: "a"},
			&chplan.LitString{V: "b"},
			&chplan.LitString{V: "c"},
		},
	})
	if err != nil {
		t.Fatalf("Expr: %v", err)
	}
	if got, want := b.String(), "(`TraceId` IN (?, ?, ?))"; got != want {
		t.Errorf("rendered SQL = %q, want %q", got, want)
	}
	if got, want := b.Args(), []any{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound args = %v, want %v", got, want)
	}
}

// TestExprInList_ConstantParenDepth asserts the rendered nesting depth
// does not scale with the element count — the regression contract the
// /api/search root-span lookup relies on for >1000-trace result sets.
func TestExprInList_ConstantParenDepth(t *testing.T) {
	t.Parallel()

	depthFor := func(n int) int {
		t.Helper()
		list := make([]chplan.Expr, 0, n)
		for i := range n {
			list = append(list, &chplan.LitInt{V: int64(i)})
		}
		b := &Builder{}
		if err := b.Expr(&chplan.InList{Left: &chplan.ColumnRef{Name: "c"}, List: list}); err != nil {
			t.Fatalf("Expr with %d elements: %v", n, err)
		}
		depth, maxDepth := 0, 0
		for _, ch := range b.String() {
			switch ch {
			case '(':
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
			case ')':
				depth--
			}
		}
		return maxDepth
	}
	if d2, d2000 := depthFor(2), depthFor(2000); d2000 != d2 {
		t.Errorf("paren depth scales with element count: depth(2000)=%d, depth(2)=%d", d2000, d2)
	}
}

// TestExprInList_Errors covers the two misuse shapes the emitter
// rejects synchronously: a nil left operand and an empty list (CH
// rejects `x IN ()` at parse time, so shipping it would only surface
// the failure later, at query execution).
func TestExprInList_Errors(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]*chplan.InList{
		"nil left":   {List: []chplan.Expr{&chplan.LitInt{V: 1}}},
		"empty list": {Left: &chplan.ColumnRef{Name: "c"}},
	} {
		b := &Builder{}
		err := b.Expr(in)
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
			continue
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: expected ErrUnsupported, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "InList") {
			t.Errorf("%s: error should name InList, got %v", name, err)
		}
	}
}
