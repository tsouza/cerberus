package logpattern

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// This file exists because internal/logql/logpattern had ZERO default-tag
// coverage: its only test file (pattern_agpl_test.go) is an A/B oracle
// gated behind the `agpl_oracle` build tag, so `go test ./...` measured
// the package at 0.0% and no gate observed a regression in the tokeniser,
// the validation rules, or the two matching routines. These tests pin the
// behaviour directly through the package's own API, so the package stands
// on its own when the AGPL oracle is not available.

// --- New: validation rules -------------------------------------------------

func TestNew_AcceptsNamedCaptures(t *testing.T) {
	m, err := New(`level=<lvl> msg=<msg>`)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if got, want := strings.Join(m.Names(), ","), "lvl,msg"; got != want {
		t.Errorf("Names() = %q, want %q", got, want)
	}
}

func TestNew_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantErr error  // errors.Is target; nil means "match on text only"
		wantMsg string // substring of the error text; empty means "unchecked"
	}{
		{
			name:    "empty pattern",
			pattern: "",
			wantErr: errEmptyPattern,
		},
		{
			name:    "no capture at all",
			pattern: "just literal text",
			wantErr: ErrNoCapture,
		},
		{
			name:    "only unnamed captures counts as no capture",
			pattern: "<_> foo <_>",
			wantErr: ErrNoCapture,
		},
		{
			name:    "consecutive captures",
			pattern: "<a><b>",
			wantErr: ErrInvalidExpr,
			wantMsg: "found consecutive capture '<a><b>'",
		},
		{
			name:    "duplicate capture name",
			pattern: "<a> x <a>",
			wantErr: ErrInvalidExpr,
			wantMsg: "duplicate capture name (a)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(tc.pattern)
			if err == nil {
				t.Fatalf("New(%q) = %+v, want an error", tc.pattern, m)
			}
			if m != nil {
				t.Errorf("New(%q) returned a non-nil Matcher alongside the error", tc.pattern)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("New(%q) error = %v, want errors.Is(_, %v)", tc.pattern, err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("New(%q) error = %q, want it to contain %q", tc.pattern, err, tc.wantMsg)
			}
		})
	}
}

// --- parse: what is and is not a capture -----------------------------------

func TestParseLiterals_TokenisesCapturesAndLiterals(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			name:    "alternating literals and captures",
			pattern: "<a>foo<b>bar",
			want:    []string{"foo", "bar"},
		},
		{
			// Every rune between two captures merges into ONE literal
			// node, so a multi-rune run is not split per rune.
			name:    "adjacent runes merge into one literal",
			pattern: "<a> - <b>",
			want:    []string{" - "},
		},
		{
			// A '<' that does not open a valid identifier capture is an
			// ordinary literal rune and merges with its neighbours.
			name:    "malformed captures stay literal",
			pattern: "<><1a><a-b><abc",
			want:    []string{"<><1a><a-b><abc"},
		},
		{
			name:    "digits are valid capture continuations",
			pattern: "<a1_b>x",
			want:    []string{"x"},
		},
		{
			name:    "no literals at all",
			pattern: "<a>",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLiterals(tc.pattern)
			if err != nil {
				t.Fatalf("ParseLiterals(%q): unexpected error: %v", tc.pattern, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseLiterals(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Errorf("ParseLiterals(%q)[%d] = %q, want %q", tc.pattern, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseLiterals_EmptyPatternIsRejected(t *testing.T) {
	_, err := ParseLiterals("")
	if !errors.Is(err, errEmptyPattern) {
		t.Errorf("ParseLiterals(\"\") error = %v, want errEmptyPattern", err)
	}
}

func TestParseLiterals_InvalidUTF8BecomesReplacementRune(t *testing.T) {
	// An invalid byte decodes to utf8.RuneError and re-encodes to the
	// replacement character, matching upstream's rune round-trip.
	got, err := ParseLiterals("a\xffb")
	if err != nil {
		t.Fatalf("ParseLiterals: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ParseLiterals returned %d literal runs, want 1", len(got))
	}
	if want := "a\uFFFDb"; string(got[0]) != want {
		t.Errorf("literal = %q, want %q", got[0], want)
	}
}

// --- ParseLineFilter: unnamed captures only --------------------------------

func TestParseLineFilter(t *testing.T) {
	m, err := ParseLineFilter([]byte("<_>foo<_>"))
	if err != nil {
		t.Fatalf("ParseLineFilter: unexpected error: %v", err)
	}
	if got := m.Names(); len(got) != 0 {
		t.Errorf("Names() = %q, want none (a line filter carries no named captures)", got)
	}
}

func TestParseLineFilter_EmptyInputYieldsAnEmptyMatcher(t *testing.T) {
	// The empty line filter is legal (upstream returns an empty matcher
	// rather than the grammar's empty-pattern error) and matches nothing.
	m, err := ParseLineFilter(nil)
	if err != nil {
		t.Fatalf("ParseLineFilter(nil): unexpected error: %v", err)
	}
	if got := m.Matches([]byte("anything")); got != nil {
		t.Errorf("Matches on an empty matcher = %q, want nil", got)
	}
	if !m.Test(nil) {
		t.Error("Test(nil) on an empty matcher = false, want true (both sides empty)")
	}
	if m.Test([]byte("anything")) {
		t.Error("Test(\"anything\") on an empty matcher = true, want false")
	}
}

func TestParseLineFilter_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantErr error
	}{
		{
			name:    "named capture is not allowed",
			pattern: "<foo> bar",
			wantErr: ErrCaptureNotAllowed,
		},
		{
			name:    "consecutive captures",
			pattern: "<_><_>",
			wantErr: ErrInvalidExpr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseLineFilter([]byte(tc.pattern))
			if err == nil {
				t.Fatalf("ParseLineFilter(%q) = %+v, want an error", tc.pattern, m)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseLineFilter(%q) error = %v, want errors.Is(_, %v)", tc.pattern, err, tc.wantErr)
			}
		})
	}
}

// --- Matches: named-capture extraction -------------------------------------

func TestMatcher_Matches(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		line    string
		want    []string // nil means "no match"
	}{
		{
			name:    "leading literal anchors the match",
			pattern: "level=<lvl> msg=<msg>",
			line:    "level=info msg=hello world",
			want:    []string{"info", "hello world"},
		},
		{
			name:    "leading literal must match at offset 0",
			pattern: "level=<lvl>",
			line:    "ts=1 level=info",
			want:    nil,
		},
		{
			name:    "unnamed captures are skipped, not returned",
			pattern: "<_> user=<user>",
			line:    "ts=1 user=bob",
			want:    []string{"bob"},
		},
		{
			name:    "a trailing capture takes the rest of the line",
			pattern: "<all>",
			line:    "everything",
			want:    []string{"everything"},
		},
		{
			name:    "a capture ending at a literal stops there",
			pattern: "<a> end",
			line:    "x end",
			want:    []string{"x"},
		},
		{
			name:    "a missing trailing literal leaves the rest captured",
			pattern: "<a> end",
			line:    "x oops",
			want:    []string{"x oops"},
		},
		{
			name:    "empty line never matches",
			pattern: "<a>",
			line:    "",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(tc.pattern)
			if err != nil {
				t.Fatalf("New(%q): unexpected error: %v", tc.pattern, err)
			}
			got := m.Matches([]byte(tc.line))
			if tc.want == nil {
				if got != nil {
					t.Fatalf("Matches(%q) = %q, want nil", tc.line, got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Matches(%q) = %q, want %q", tc.line, got, tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Errorf("Matches(%q)[%d] = %q, want %q", tc.line, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMatcher_MatchesReusesTheCaptureBuffer(t *testing.T) {
	// The returned slice is documented as invalidated by the next call:
	// the second Matches must reuse the same backing array rather than
	// growing it, so a caller that stashes the slice sees the new values.
	m, err := New("k=<v>")
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	first := m.Matches([]byte("k=one"))
	if len(first) != 1 || string(first[0]) != "one" {
		t.Fatalf("first Matches = %q, want [one]", first)
	}
	second := m.Matches([]byte("k=two"))
	if len(second) != 1 || string(second[0]) != "two" {
		t.Fatalf("second Matches = %q, want [two]", second)
	}
	if &first[0] != &second[0] {
		t.Error("second Matches allocated a fresh capture slice; the buffer should be reused")
	}
}

func TestMatcher_MatchesOnALiteralOnlyPattern(t *testing.T) {
	// ParseLineFilter accepts a capture-free pattern, and Matches on one
	// has nothing to extract once the leading literal is consumed.
	m, err := ParseLineFilter([]byte("literal"))
	if err != nil {
		t.Fatalf("ParseLineFilter: unexpected error: %v", err)
	}
	if got := m.Matches([]byte("literal tail")); got != nil {
		t.Errorf("Matches = %q, want nil (no captures to extract)", got)
	}
}

// --- Test: filter semantics -------------------------------------------------

func TestMatcher_Test(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		line    string
		want    bool
	}{
		{
			name:    "captures on both ends need non-empty remainders",
			pattern: "<_>foo<_>",
			line:    "xfooy",
			want:    true,
		},
		{
			name:    "a leading capture may not match empty",
			pattern: "<_>foo<_>",
			line:    "fooy",
			want:    false,
		},
		{
			name:    "a trailing capture may not match empty",
			pattern: "<_>foo<_>",
			line:    "xfoo",
			want:    false,
		},
		{
			name:    "missing literal fails",
			pattern: "<_>foo<_>",
			line:    "xbary",
			want:    false,
		},
		{
			name:    "a pattern ending in a literal must end the line",
			pattern: "<_>foo",
			line:    "xfoo",
			want:    true,
		},
		{
			name:    "trailing text after a final literal fails",
			pattern: "<_>foo",
			line:    "xfooy",
			want:    false,
		},
		{
			name:    "a leading literal is matched wherever it first occurs",
			pattern: "foo<_>",
			line:    "barfoobaz",
			want:    true,
		},
		{
			name:    "empty line never matches a non-empty pattern",
			pattern: "<_>foo",
			line:    "",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseLineFilter([]byte(tc.pattern))
			if err != nil {
				t.Fatalf("ParseLineFilter(%q): unexpected error: %v", tc.pattern, err)
			}
			if got := m.Test([]byte(tc.line)); got != tc.want {
				t.Errorf("Test(%q) with pattern %q = %v, want %v", tc.line, tc.pattern, got, tc.want)
			}
		})
	}
}

// --- node rendering ---------------------------------------------------------

func TestNodeString(t *testing.T) {
	// The node Stringers back the validation error messages, which name
	// the offending capture(s) verbatim.
	if got, want := capture("foo").String(), "<foo>"; got != want {
		t.Errorf("capture.String() = %q, want %q", got, want)
	}
	if got, want := literals([]byte("a b")).String(), "a b"; got != want {
		t.Errorf("literals.String() = %q, want %q", got, want)
	}
	if !capture("_").isUnnamed() {
		t.Error(`capture("_").isUnnamed() = false, want true`)
	}
	if capture("x").isUnnamed() {
		t.Error(`capture("x").isUnnamed() = true, want false`)
	}
}

func TestParse_NodeShape(t *testing.T) {
	// White-box: the tokeniser must emit strictly alternating literal
	// runs and captures, in source order.
	e, err := parse([]byte("a<b>c<_>"))
	if err != nil {
		t.Fatalf("parse: unexpected error: %v", err)
	}
	want := []node{literals([]byte("a")), capture("b"), literals([]byte("c")), capture("_")}
	if len(e) != len(want) {
		t.Fatalf("parse produced %d nodes (%v), want %d (%v)", len(e), e, len(want), want)
	}
	for i := range want {
		switch w := want[i].(type) {
		case literals:
			got, ok := e[i].(literals)
			if !ok {
				t.Fatalf("node %d is %T, want literals", i, e[i])
			}
			if !bytes.Equal(got, w) {
				t.Errorf("node %d = %q, want %q", i, got, w)
			}
		case capture:
			got, ok := e[i].(capture)
			if !ok {
				t.Fatalf("node %d is %T, want capture", i, e[i])
			}
			if got != w {
				t.Errorf("node %d = %q, want %q", i, got, w)
			}
		}
	}
}
