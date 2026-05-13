package chplan_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEqual_MismatchedNodeTypes — Equal() across different concrete
// node types must always return false. Catches a class of bugs where
// a node's Equal forgets to type-assert and accidentally calls
// Equal() against itself.
func TestEqual_MismatchedNodeTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b chplan.Node
	}{
		{
			"Scan vs Filter",
			&chplan.Scan{Table: "t"},
			&chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: &chplan.LitBool{V: true}},
		},
		{
			"Filter vs Project",
			&chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: &chplan.LitBool{V: true}},
			&chplan.Project{Input: &chplan.Scan{Table: "t"}, Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "c"}}}},
		},
		{
			"Aggregate vs Limit",
			&chplan.Aggregate{Input: &chplan.Scan{Table: "t"}},
			&chplan.Limit{Input: &chplan.Scan{Table: "t"}, Count: 10},
		},
		{
			"RangeWindow vs Filter",
			&chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Range: time.Minute, TimestampColumn: "TimeUnix", ValueColumn: "Value"},
			&chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: &chplan.LitBool{V: true}},
		},
		{
			"VectorJoin vs StructuralJoin",
			&chplan.VectorJoin{
				Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
				Op: chplan.OpAdd, MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
				TimestampColumn: "TimeUnix", ValueColumn: "Value",
			},
			&chplan.StructuralJoin{
				Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
				Op:            chplan.StructuralChild,
				TraceIDColumn: "TraceId", SpanIDColumn: "SpanId", ParentSpanIDColumn: "ParentSpanId",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.a.Equal(tc.b) {
				t.Errorf("%T.Equal(%T): got true, want false", tc.a, tc.b)
			}
			if tc.b.Equal(tc.a) {
				t.Errorf("%T.Equal(%T) (reverse): got true, want false", tc.b, tc.a)
			}
		})
	}
}

// TestExprEqual_MismatchedTypes — same contract for Expr nodes.
// Distinct concrete types must never be Equal.
func TestExprEqual_MismatchedTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b chplan.Expr
	}{
		{
			"ColumnRef vs LitString",
			&chplan.ColumnRef{Name: "x"},
			&chplan.LitString{V: "x"},
		},
		{
			"LitInt vs LitFloat",
			&chplan.LitInt{V: 1},
			&chplan.LitFloat{V: 1.0},
		},
		{
			"LitString vs LitBool",
			&chplan.LitString{V: "true"},
			&chplan.LitBool{V: true},
		},
		{
			"Binary vs FuncCall",
			&chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 1}},
			&chplan.FuncCall{Name: "eq", Args: []chplan.Expr{&chplan.LitInt{V: 1}, &chplan.LitInt{V: 1}}},
		},
		{
			"MapAccess vs FieldAccess",
			&chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attrs"}, Key: &chplan.LitString{V: "k"}},
			&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "Attrs"}, Path: "k"},
		},
		{
			"FuncCall vs LineContent",
			&chplan.FuncCall{Name: "match", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}, &chplan.LitString{V: "x"}}},
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.a.Equal(tc.b) {
				t.Errorf("%T.Equal(%T): got true, want false", tc.a, tc.b)
			}
			if tc.b.Equal(tc.a) {
				t.Errorf("%T.Equal(%T) (reverse): got true, want false", tc.b, tc.a)
			}
		})
	}
}

// TestExprEqual_SameTypeDifferentValues — same concrete type, but
// observably different inner state, must return false.
func TestExprEqual_SameTypeDifferentValues(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		name string
		a, b chplan.Expr
	}{
		{"ColumnRef different name", &chplan.ColumnRef{Name: "a"}, &chplan.ColumnRef{Name: "b"}},
		{
			"ColumnRef different qualifier",
			&chplan.ColumnRef{Name: "c", Qualifier: "L"},
			&chplan.ColumnRef{Name: "c", Qualifier: "R"},
		},
		{
			"ColumnRef qualifier vs bare",
			&chplan.ColumnRef{Name: "c"},
			&chplan.ColumnRef{Name: "c", Qualifier: "R"},
		},
		{"LitString different value", &chplan.LitString{V: "a"}, &chplan.LitString{V: "b"}},
		{"LitInt different value", &chplan.LitInt{V: 1}, &chplan.LitInt{V: 2}},
		{"LitFloat different value", &chplan.LitFloat{V: 1.0}, &chplan.LitFloat{V: 2.0}},
		{"LitBool different value", &chplan.LitBool{V: true}, &chplan.LitBool{V: false}},
		{
			"Binary different op",
			&chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 1}},
			&chplan.Binary{Op: chplan.OpNe, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 1}},
		},
		{
			"Binary different children",
			&chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 1}},
			&chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
		},
		{
			"FuncCall different name",
			&chplan.FuncCall{Name: "abs", Args: []chplan.Expr{&chplan.LitInt{V: 1}}},
			&chplan.FuncCall{Name: "ceil", Args: []chplan.Expr{&chplan.LitInt{V: 1}}},
		},
		{
			"FuncCall different arg count",
			&chplan.FuncCall{Name: "f", Args: []chplan.Expr{&chplan.LitInt{V: 1}}},
			&chplan.FuncCall{Name: "f", Args: []chplan.Expr{&chplan.LitInt{V: 1}, &chplan.LitInt{V: 2}}},
		},
		{
			"MapAccess different key",
			&chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attrs"}, Key: &chplan.LitString{V: "a"}},
			&chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attrs"}, Key: &chplan.LitString{V: "b"}},
		},
		{
			"FieldAccess different path",
			&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "http.method"},
			&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "http.status_code"},
		},
		{
			"LineContent different pattern",
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "ERROR"},
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "WARN"},
		},
		{
			"LineContent different negation",
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x", Negated: false},
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x", Negated: true},
		},
		{
			"LineContent different regex flag",
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x", IsRegex: false},
			&chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x", IsRegex: true},
		},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.a.Equal(tc.b) {
				t.Errorf("Equal: identical type, different values — got true, want false")
			}
			if tc.b.Equal(tc.a) {
				t.Errorf("Equal reverse: got true, want false")
			}
		})
	}
}

// TestExprEqual_DeeplyNested verifies Equal recurses through deep
// trees correctly. A 4-level Binary that differs only in the deepest
// literal must return false.
func TestExprEqual_DeeplyNested(t *testing.T) {
	t.Parallel()

	build := func(deepest int64) chplan.Expr {
		return &chplan.Binary{
			Op:   chplan.OpAdd,
			Left: &chplan.LitInt{V: 1},
			Right: &chplan.Binary{
				Op:   chplan.OpAdd,
				Left: &chplan.LitInt{V: 2},
				Right: &chplan.Binary{
					Op:    chplan.OpAdd,
					Left:  &chplan.LitInt{V: 3},
					Right: &chplan.LitInt{V: deepest},
				},
			},
		}
	}
	if !build(99).Equal(build(99)) {
		t.Errorf("Equal: identical 4-deep trees should be Equal")
	}
	if build(99).Equal(build(100)) {
		t.Errorf("Equal: 4-deep trees differing only in deepest leaf should NOT be Equal")
	}
}

// TestOrderByEqual_Negatives — OrderBy Equal() must distinguish
// direction (ASC vs DESC) and key expression. Equal-length but
// different-direction keys must not compare equal.
func TestOrderByEqual_Negatives(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_traces"}
	tsCol := &chplan.ColumnRef{Name: "Timestamp"}

	a := &chplan.OrderBy{Input: scan, Keys: []chplan.OrderKey{{Expr: tsCol, Desc: true}}}
	b := &chplan.OrderBy{Input: scan, Keys: []chplan.OrderKey{{Expr: tsCol, Desc: false}}}
	if a.Equal(b) {
		t.Errorf("OrderBy DESC vs ASC should not be Equal")
	}
	if b.Equal(a) {
		t.Errorf("OrderBy Equal must be symmetric")
	}

	c := &chplan.OrderBy{Input: scan, Keys: []chplan.OrderKey{
		{Expr: tsCol, Desc: true},
		{Expr: &chplan.ColumnRef{Name: "Duration"}, Desc: false},
	}}
	if a.Equal(c) {
		t.Errorf("OrderBy with different key count should not be Equal")
	}
}
