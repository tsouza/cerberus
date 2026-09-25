package chplan

import (
	"reflect"
	"testing"
)

// TestOuterGroupNamesAliasFallback pins the boundary and empty-alias
// branches of OuterGroupNames: the name falls back to `g<i>` when either
// the alias slice runs out OR the alias is empty, and no keys yield nil.
func TestOuterGroupNamesAliasFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		groupBy []Expr
		aliases []string
		want    []string
	}{
		{
			name:    "all aliases present",
			groupBy: []Expr{&ColumnRef{Name: "A"}, &ColumnRef{Name: "B"}},
			aliases: []string{"a", "b"},
			want:    []string{"a", "b"},
		},
		{
			name:    "aliases slice shorter than groupBy",
			groupBy: []Expr{&ColumnRef{Name: "A"}, &ColumnRef{Name: "B"}},
			aliases: []string{"a"},
			want:    []string{"a", "g1"},
		},
		{
			name:    "empty alias entry → fallback",
			groupBy: []Expr{&ColumnRef{Name: "A"}, &ColumnRef{Name: "B"}},
			aliases: []string{"", "b"},
			want:    []string{"g0", "b"},
		},
		{
			name:    "nil groupBy → nil result",
			groupBy: nil,
			aliases: []string{"a"},
			want:    nil,
		},
		{
			name:    "empty groupBy → nil result",
			groupBy: []Expr{},
			aliases: []string{"a"},
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := OuterGroupNames(c.groupBy, c.aliases)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("OuterGroupNames(%v, %v) = %v, want %v", c.groupBy, c.aliases, got, c.want)
			}
		})
	}
}
