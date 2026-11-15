package logql

import (
	"slices"
	"testing"
)

// TestParsePatternLineFilter_Structure pins the literal-run structure the
// `|>` / `!>` lowering takes from a pattern: its literal runs in order,
// whether the first element is a literal (exempt from the must-gap rule),
// and whether the last is a `<_>` (the remainder must then be non-empty).
// A literal that is a prefix or a suffix of `<_>` is ordinary text, and a
// valid encoding of U+FFFD is a literal rune.
func TestParsePatternLineFilter_Structure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		want    patternLineFilter
	}{
		{"<_>><_>", patternLineFilter{Literals: []string{">"}, EndsWithCapture: true}},
		{"a<_>_><_>", patternLineFilter{Literals: []string{"a", "_>"}, FirstIsLiteral: true, EndsWithCapture: true}},
		{"<_><<_>", patternLineFilter{Literals: []string{"<"}, EndsWithCapture: true}},
		{"a\xef\xbf\xbd<_>", patternLineFilter{Literals: []string{"a�"}, FirstIsLiteral: true, EndsWithCapture: true}},
		{"<_>", patternLineFilter{Literals: []string{}, EndsWithCapture: true}},
	}
	for _, c := range cases {
		got, err := parsePatternLineFilter(c.pattern)
		if err != nil {
			t.Fatalf("parsePatternLineFilter(%q): %v", c.pattern, err)
		}
		if !slices.Equal(got.Literals, c.want.Literals) || got.FirstIsLiteral != c.want.FirstIsLiteral || got.EndsWithCapture != c.want.EndsWithCapture {
			t.Errorf("parsePatternLineFilter(%q) = %+v, want %+v", c.pattern, got, c.want)
		}
	}
}
