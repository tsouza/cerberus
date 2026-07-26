package lsyntax

import (
	"errors"
	"testing"
)

// ParseError.Error() switches its rendering on whether BOTH line and col
// are zero. Each of the two fields must be checked independently: these
// cases pin "only line is zero" and "only col is zero" so a mutation on
// either half of the `&&`, or a flip to `||`, changes which format gets
// picked.
func TestParseErrorFormatting(t *testing.T) {
	cases := []struct {
		name string
		err  ParseError
		want string
	}{
		{
			name: "both zero: no positional prefix",
			err:  ParseError{msg: "z"},
			want: "parse error : z",
		},
		{
			name: "col zero, line set: positional prefix",
			err:  ParseError{msg: "x", line: 5, col: 0},
			want: "parse error at line 5, col 0: x",
		},
		{
			name: "line zero, col set: positional prefix",
			err:  ParseError{msg: "y", line: 0, col: 7},
			want: "parse error at line 0, col 7: y",
		},
		{
			name: "both set: positional prefix",
			err:  ParseError{msg: "w", line: 2, col: 3},
			want: "parse error at line 2, col 3: w",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseErrorIs(t *testing.T) {
	var err error = ParseError{msg: "boom"}
	if !errors.Is(err, ErrParse) {
		t.Error("ParseError should satisfy errors.Is(err, ErrParse)")
	}
}
