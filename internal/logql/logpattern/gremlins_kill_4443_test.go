package logpattern

import "testing"

// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-logql-other-b leg (cerberus issue #4443), whose scope includes
// this package.

// TestLineFilterLeadingLiteralMustOccur pins that a line filter whose
// pattern opens with a literal rejects a line that does not contain it,
// whatever the captures after it would match.
func TestLineFilterLeadingLiteralMustOccur(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, line string
	}{
		{"foo<_>", "barbaz"},
		{"foo<_>bar", "xbar"},
		{"foo", "bar"},
	} {
		m, err := ParseLineFilter([]byte(tc.pattern))
		if err != nil {
			t.Fatalf("ParseLineFilter(%q): %v", tc.pattern, err)
		}
		if m.Test([]byte(tc.line)) {
			t.Errorf("ParseLineFilter(%q).Test(%q) = true, want false: the line does not contain the leading literal", tc.pattern, tc.line)
		}
	}
}
