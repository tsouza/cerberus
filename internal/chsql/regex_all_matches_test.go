package chsql

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMatchesEmpty pins the nullability analysis against Go's own answer:
// a pattern reported non-nullable must never produce an empty match on any
// input, and the patterns Go matches empty on some input are reported
// nullable.
func TestMatchesEmpty(t *testing.T) {
	cases := map[string]bool{
		`.*`: true, `x*`: true, `a|`: true, `(a)?`: true, `^`: true, `\b`: true,
		`(?:)`: true, `a*b*`: true, `(?P<n>x*)`: true, `^\s+|\s+$`: false,
		`a`: false, `.+`: false, `[0-9]`: false, `\ba\b`: false, `a+|b`: false,
	}
	inputs := append([]string{"ab", "abxa", "é", " a "}, regexPatternInputs...)
	for pattern, want := range cases {
		got, err := matchesEmpty(pattern)
		if err != nil {
			t.Fatalf("matchesEmpty(%q): %v", pattern, err)
		}
		if got != want {
			t.Errorf("matchesEmpty(%q) = %v, want %v", pattern, got, want)
		}
		if got {
			continue
		}
		re := regexp.MustCompile(pattern)
		for _, in := range inputs {
			for _, m := range re.FindAllStringIndex(in, -1) {
				if m[0] == m[1] {
					t.Errorf("%q reported non-nullable but matches empty in %q at %d", pattern, in, m[0])
				}
			}
		}
	}
}

// TestAllMatchesPatternGuard pins that an every-match function is emitted
// only with a literal pattern that cannot match the empty string, and that
// first-match functions are not constrained.
func TestAllMatchesPatternGuard(t *testing.T) {
	call := func(fn chplan.Fn, pattern chplan.Expr) *chplan.FuncCall {
		return &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}, pattern, &chplan.LitString{V: "_"}}}
	}
	for _, fn := range []chplan.Fn{chplan.FnRegexExtractAll, chplan.FnRegexExtractAllGroupsHorizontal, chplan.FnRegexReplaceAll} {
		if err := checkAllMatchesPattern(call(fn, &chplan.LitString{V: "x+"})); err != nil {
			t.Errorf("%s non-nullable pattern: %v", fn, err)
		}
		if err := checkAllMatchesPattern(call(fn, &chplan.InlineString{V: "[^a]"})); err != nil {
			t.Errorf("%s non-nullable inline pattern: %v", fn, err)
		}
		for _, bad := range []chplan.Expr{&chplan.LitString{V: "x*"}, &chplan.InlineString{V: ".*"}, &chplan.ColumnRef{Name: "p"}} {
			if err := checkAllMatchesPattern(call(fn, bad)); !errors.Is(err, ErrNullableAllMatchesPattern) {
				t.Errorf("%s pattern %#v: err = %v, want ErrNullableAllMatchesPattern", fn, bad, err)
			}
		}
	}
	for _, fn := range []chplan.Fn{chplan.FnRegexExtractFirst, chplan.FnRegexExtractGroup, chplan.FnRegexReplaceFirst} {
		if err := checkAllMatchesPattern(call(fn, &chplan.LitString{V: "x*"})); err != nil {
			t.Errorf("%s first-match function rejected: %v", fn, err)
		}
	}
}

// TestRegexExtractGroupReadsGoDefaultFlags pins that regexpExtract's pattern
// is emitted so that `.` does not match a newline, as under Go's defaults,
// and that other functions' patterns are left alone.
func TestRegexExtractGroupReadsGoDefaultFlags(t *testing.T) {
	call := func(fn chplan.Fn) *chplan.FuncCall {
		return &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}, &chplan.LitString{V: "(?P<m>.*)"}, &chplan.LitInt{V: 1}}}
	}
	got := withGoDefaultFlagsPattern(call(chplan.FnRegexExtractGroup)).Args[regexPatternArg].(*chplan.LitString).V
	want := regexp.MustCompile("(?P<m>.*)")
	re := regexp.MustCompile("(?s)" + got)
	for _, in := range []string{"a\nb", "\napi", "api"} {
		if g, w := re.FindStringSubmatch(in)[1], want.FindStringSubmatch(in)[1]; g != w {
			t.Errorf("respelled %q on %q: group %q, Go's default flags give %q", got, in, g, w)
		}
	}
	if f := call(chplan.FnRegexExtractFirst); withGoDefaultFlagsPattern(f) != f {
		t.Errorf("extract call was rewritten")
	}
}

// TestRegexPatternArgAbsent pins the calls that carry no pattern argument:
// an every-match function is refused, and regexpExtract's respelling leaves
// the call as it is.
func TestRegexPatternArgAbsent(t *testing.T) {
	haystackOnly := []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}
	err := checkAllMatchesPattern(&chplan.FuncCall{Fn: chplan.FnRegexExtractAll, Args: haystackOnly})
	if !errors.Is(err, ErrNullableAllMatchesPattern) || !strings.Contains(err.Error(), "has no pattern argument") {
		t.Errorf("extractAll with no pattern: err = %v; want ErrNullableAllMatchesPattern naming the missing argument", err)
	}
	f := &chplan.FuncCall{Fn: chplan.FnRegexExtractGroup, Args: haystackOnly}
	if got := withGoDefaultFlagsPattern(f); got != f {
		t.Errorf("regexpExtract with no pattern was rewritten to %#v", got)
	}
}

// TestBoundedProgIndex pins the conversion's range check at each edge: the
// first and last instruction convert, one past either end does not.
func TestBoundedProgIndex(t *testing.T) {
	const insts = 3
	for _, tc := range []struct {
		n, count int
		want     uint32
		ok       bool
	}{
		{0, insts, 0, true},
		{insts - 1, insts, insts - 1, true},
		{-1, insts, 0, false},
		{insts, insts, 0, false},
		{math.MaxUint32, math.MaxUint32 + 1, math.MaxUint32, true},
		{math.MaxUint32 + 1, math.MaxUint32 + 2, 0, false},
	} {
		got, err := boundedProgIndex(tc.n, tc.count)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("boundedProgIndex(%d, %d) = %d, %v; want %d, ok %v", tc.n, tc.count, got, err, tc.want, tc.ok)
		}
	}
}
