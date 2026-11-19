package chplan

import "testing"

// TestReadsAnyColumn: a column named anywhere in the expression, nested in
// a call, counts; a string literal spelling the name does not, and neither
// does a column the set does not name.
func TestReadsAnyColumn(t *testing.T) {
	names := map[string]struct{}{"a": {}}
	nested := &Binary{Op: OpAnd, Left: &LitBool{V: true}, Right: &FuncCall{Fn: FnMapSort, Args: []Expr{&ColumnRef{Name: "a"}}}}
	cases := []struct {
		name string
		expr Expr
		want bool
	}{
		{name: "nested column", expr: nested, want: true},
		{name: "other column", expr: &ColumnRef{Name: "b"}, want: false},
		{name: "literal spelling the name", expr: &LitString{V: "a"}, want: false},
		{name: "nil", expr: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadsAnyColumn(tc.expr, names); got != tc.want {
				t.Errorf("ReadsAnyColumn = %v; want %v", got, tc.want)
			}
		})
	}
}
