package chsql

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestRegexMatcher_LiteralSetRendersMembership pins the emitted shape of a
// label matcher whose pattern is a finite set of literals: `=~` renders as
// IN over the set (equality for a one-element set) and `!~` as its
// negation, while a pattern with a metacharacter, a case-insensitive flag
// or an empty-width assertion keeps match().
func TestRegexMatcher_LiteralSetRendersMembership(t *testing.T) {
	t.Parallel()
	job := &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: "job"}}
	cases := []struct {
		name    string
		op      chplan.BinaryOp
		pattern string
		wantSQL string
		wantArg []any
	}{
		{"alternation", chplan.OpMatch, "api|web", "(`Attributes`[?] IN (?, ?))", []any{"job", "api", "web"}},
		{"negated_alternation", chplan.OpNotMatch, "api|web", "(`Attributes`[?] NOT IN (?, ?))", []any{"job", "api", "web"}},
		{"single_literal", chplan.OpMatch, "api", "(`Attributes`[?] = ?)", []any{"job", "api"}},
		{"negated_single_literal", chplan.OpNotMatch, "api", "(`Attributes`[?] != ?)", []any{"job", "api"}},
		{"empty_alternative", chplan.OpMatch, "api|", "(`Attributes`[?] IN (?, ?))", []any{"job", "api", ""}},
		{"empty_pattern", chplan.OpMatch, "", "(`Attributes`[?] = ?)", []any{"job", ""}},
		{"escaped_dot", chplan.OpMatch, `a\.b|c`, "(`Attributes`[?] IN (?, ?))", []any{"job", "a.b", "c"}},
		{"duplicates_collapse", chplan.OpMatch, "api|api", "(`Attributes`[?] = ?)", []any{"job", "api"}},
		// regexp/syntax factors a shared prefix into a class:
		// `api|apx` parses as `ap[ix]`.
		{"factored_prefix", chplan.OpMatch, "api|apx", "(`Attributes`[?] IN (?, ?))", []any{"job", "api", "apx"}},
		{"optional_suffix", chplan.OpMatch, "ab(?:c)?", "(`Attributes`[?] IN (?, ?))", []any{"job", "abc", "ab"}},
		// A bounded character class is a finite set too, as in
		// Prometheus's own set matcher.
		{
			"digit_class", chplan.OpMatch, `\d`, "(`Attributes`[?] IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?))",
			[]any{"job", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9"},
		},
		{
			"class_product", chplan.OpMatch, `[ab]-[12]`, "(`Attributes`[?] IN (?, ?, ?, ?))",
			[]any{"job", "a-1", "a-2", "b-1", "b-2"},
		},
		// The rows below keep match(): wantSQL is empty and the expected
		// rendering is the regex path's own, goRegexMatch.
		{"dot_stays_regex", chplan.OpMatch, "a.b|c", "", nil},
		{"star_stays_regex", chplan.OpMatch, "api.*", "", nil},
		{"plus_stays_regex", chplan.OpMatch, "a+", "", nil},
		{"digit_run_stays_regex", chplan.OpMatch, `\d+`, "", nil},
		{"oversized_class_product_stays_regex", chplan.OpMatch, `[a-z][a-z][a-z]`, "", nil},
		{"fold_case_stays_regex", chplan.OpMatch, "(?i)api|web", "", nil},
		{"scoped_fold_case_stays_regex", chplan.OpMatch, "api|(?i:web)", "", nil},
		{"user_anchor_stays_regex", chplan.OpMatch, "^api$", "", nil},
		{"replacement_char_stays_regex", chplan.OpMatch, "a\uFFFD|b", "", nil},
		{"negated_fold_case_stays_regex", chplan.OpNotMatch, "(?i)api", "", nil},
		// The anchored form `^(?:a)\Q)$` quotes the closing `)$`, so the
		// pattern ends without an end-of-text anchor and matches a prefix.
		{"quoted_end_anchor_stays_regex", chplan.OpMatch, `a)\Q`, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewBuilder()
			if err := b.Expr(&chplan.Binary{Op: tc.op, Left: job, Right: &chplan.LitString{V: tc.pattern}}); err != nil {
				t.Fatalf("Expr: %v", err)
			}
			wantSQL, want := tc.wantSQL, tc.wantArg
			if wantSQL == "" {
				anchored := "^(?:" + tc.pattern + ")$"
				m := goRegexMatch(func(fb *Builder) { _ = fb.Expr(job) }, Lit(anchored), regexReadsInvalidUTF8(anchored))
				if tc.op == chplan.OpNotMatch {
					m = Not(m)
				}
				wantSQL, want = Render(m)
			}
			if got := b.String(); got != wantSQL {
				t.Errorf("SQL = %q, want %q", got, wantSQL)
			}
			if got := b.Args(); !reflect.DeepEqual(got, want) {
				t.Errorf("args = %#v, want %#v", got, want)
			}
		})
	}
}

// regexSetAtoms are the pattern pieces the property below composes: plain
// and multi-byte literals, an escaped metacharacter, U+FFFD, a bounded
// class, and the constructs that must keep a pattern on match() (`.`,
// repetition, a case-insensitive group, an inner anchor).
var regexSetAtoms = []string{
	"a", "b", "ab", "é", `\.`, `\|`, "�", "[ab]", `\d`,
	".", "a*", "b+", "(?i:a)", "(?i)b", "^", "$", `\b`,
}

// regexSetValues are the subject values the property draws from besides
// the set's own members: the empty string an absent label reads as, valid
// strings near the atoms, and invalid UTF-8, which Go's regexp reads as
// U+FFFD.
var regexSetValues = []string{
	"", "a", "b", "ab", "aa", "A", "B", "é", "É", ".", "|", "a.", "x", "0", "7",
	"�", "\xff", "a\xff", "\xc3", "a|b", "ab.", "abab",
}

// TestAnchoredRegexLiteralSet_AgreesWithGoRegexp checks the rewrite against
// the reference engines' own matcher: Prometheus and Loki compile a label
// matcher as `^(?s:<pattern>)$` with Go's regexp. Whenever
// anchoredRegexLiteralSet accepts a generated pattern, membership in the
// returned set must equal that matcher's answer on every value drawn —
// empty, invalid UTF-8 and each member included — and the set must hold no
// duplicates.
func TestAnchoredRegexLiteralSet_AgreesWithGoRegexp(t *testing.T) {
	t.Parallel()
	accepted := 0
	rapid.Check(t, func(rt *rapid.T) {
		alts := rapid.SliceOfN(rapid.SliceOfN(rapid.SampledFrom(regexSetAtoms), 0, 3), 1, 4).Draw(rt, "alternatives")
		parts := make([]string, len(alts))
		for i, alt := range alts {
			parts[i] = strings.Join(alt, "")
		}
		pattern := strings.Join(parts, "|")
		if rapid.Bool().Draw(rt, "optionalGroup") {
			pattern = "(?:" + pattern + ")?"
		}
		reference, err := regexp.Compile("^(?s:" + pattern + ")$")
		if err != nil {
			rt.Fatalf("generated pattern %q does not compile: %v", pattern, err)
		}
		set, ok := anchoredRegexLiteralSet("^(?:" + pattern + ")$")
		if !ok {
			return
		}
		accepted++
		members := make(map[string]bool, len(set))
		for _, s := range set {
			if members[s] {
				rt.Fatalf("pattern %q: set %q repeats %q", pattern, set, s)
			}
			members[s] = true
		}
		// Each member is also probed in the spellings an engine could
		// confuse with it: case-flipped, and with U+FFFD written as an
		// invalid byte.
		values := slices.Clone(regexSetValues)
		for _, s := range set {
			values = append(values, s, strings.ToUpper(s), strings.ToLower(s), strings.ReplaceAll(s, "\uFFFD", "\xff"))
		}
		for _, v := range values {
			if got, want := members[v], reference.MatchString(v); got != want {
				rt.Fatalf("pattern %q, value %q: set membership %v, Go regexp %v (set %q)", pattern, v, got, want, set)
			}
		}
	})
	if accepted == 0 {
		t.Fatal("no generated pattern was rewritten to a literal set; the property checked nothing")
	}
}
