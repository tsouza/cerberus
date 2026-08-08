package qlcommon

import (
	"regexp/syntax"
	"testing"
)

// TestNullableGroups pins the per-operator nullability verdict that
// decides whether a shared capture-group name is expressible in SQL.
//
// The verdict is not cosmetic. [captureGroups.resolve] translates a
// shared name into `arrayFirst(x -> x != ”, …)` exactly when every
// carrier is NON-nullable, because only then does "first non-empty"
// coincide with Go's "first that took part in the match". Call a
// nullable group non-nullable and cerberus emits SQL that silently
// answers with a later carrier's capture where Prometheus answers with
// the empty string; call a non-nullable group nullable and a query
// Prometheus answers gets rejected. So each row below is one operator's
// contribution to that boundary.
//
// Every case drives the real entry point — a regex STRING — rather than
// a hand-built syntax tree, so the operator each one is claimed to
// exercise is the operator Go's parser actually produces for it. Several
// obvious spellings do NOT survive parsing: `(a|b)` is folded to a
// character class, so the alternation cases spell out multi-rune arms.
func TestNullableGroups(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		// want is the nullability of capture group 1.
		want bool
	}{
		// Single-width shapes: a group that takes part consumes at
		// least one rune, which is what makes the shared-name form
		// expressible at all.
		{"literal", "(a)", false},
		{"multi_rune_literal", "(ab)", false},
		{"char_class", "([a-z])", false},
		{"any_char", "(?s)(.)", false},
		{"any_char_not_nl", "(.)", false},

		// Zero-width shapes. The assertions match the empty string
		// subject to a condition on the surrounding text, which is
		// still a match of width zero.
		{"empty_match", "()", true},
		{"begin_line", "(?m)(^)", true},
		{"end_line", "(?m)($)", true},
		{"begin_text", `(\A)`, true},
		{"end_text", `(\z)`, true},
		{"word_boundary", `(\b)`, true},
		{"no_word_boundary", `(\B)`, true},

		// Repetition. Star and quest admit zero repetitions
		// unconditionally; plus and a min>0 repeat are nullable exactly
		// when their body is, which is why the two `plus_*` rows differ.
		{"star", "(a*)", true},
		{"quest", "(a?)", true},
		{"plus_of_non_nullable", "(a+)", false},
		{"plus_of_nullable", "((?:a?)+)", true},
		{"repeat_min_zero", "(a{0,2})", true},
		{"repeat_min_positive", "(a{2,3})", false},
		{"repeat_min_positive_unbounded", "(a{2,})", false},
		{"repeat_min_positive_of_nullable", "((?:a?){2,3})", true},

		// Concatenation is nullable only when EVERY part can
		// contribute nothing, so one non-nullable part settles it.
		{"concat_all_nullable", "(a?b?)", true},
		{"concat_one_non_nullable", "(a?bc?)", false},

		// Alternation is the mirror image: ONE nullable branch is
		// enough. Both arms are multi-rune so the parser leaves the
		// alternation standing instead of folding it to a char class.
		{"alternate_none_nullable", "(ab|cd)", false},
		{"alternate_one_nullable", "(ab|)", true},

		// A group wrapping another group defers to the inner one.
		{"nested_capture_non_nullable", "((a))", false},
		{"nested_capture_nullable", "((a?))", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nullableGroups(tc.regex)
			if got == nil {
				t.Fatalf("nullableGroups(%q) = nil, want a map — the regex must parse", tc.regex)
			}
			nullable, ok := got[1]
			if !ok {
				t.Fatalf("nullableGroups(%q) recorded no verdict for capture group 1; got %v",
					tc.regex, got)
			}
			if nullable != tc.want {
				t.Errorf("nullableGroups(%q)[1] = %v, want %v", tc.regex, nullable, tc.want)
			}
		})
	}
}

// TestNullableGroupsWalksEveryGroup checks that the walk records a
// verdict per group rather than only for the outermost or the last one.
// The indices have to line up with `Regexp.SubexpNames`, because
// [captureGroups] resolves a name to indices through SubexpNames and
// then looks those indices up in this map — an off-by-one between the
// two would read a neighbouring group's nullability and decide the
// expressibility question about the wrong group.
func TestNullableGroupsWalksEveryGroup(t *testing.T) {
	t.Parallel()

	// Group 1 wraps 2 and 3. Its own verdict is neither group's: the
	// concatenation is non-nullable because group 3 is, even though
	// group 2 — the one a "record the first group you meet" walk would
	// reach first — is nullable. So the three verdicts alternate, and a
	// walk that recorded only the outermost, only the innermost, or the
	// last one it saw would disagree with at least one row here.
	const regex = "((a?)(b))"

	got := nullableGroups(regex)
	want := map[int]bool{1: false, 2: true, 3: false}
	if len(got) != len(want) {
		t.Fatalf("nullableGroups(%q) recorded %d group(s), want %d: %v",
			regex, len(got), len(want), got)
	}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("nullableGroups(%q)[%d] = %v, want %v", regex, idx, got[idx], w)
		}
	}
}

// TestNullableGroupsUnparseableRegexIsConservative pins the nil return.
//
// A nil map reads as "no verdict for any group", and every consumer
// treats a missing verdict as nullable, so an unparseable regex keeps
// the rejection. That is the safe direction: the alternative — assuming
// non-nullable and emitting the `arrayFirst` form — would answer a query
// whose regex cerberus could not even analyse.
func TestNullableGroupsUnparseableRegexIsConservative(t *testing.T) {
	t.Parallel()

	for _, regex := range []string{"(", "a{2,1}", `[z-a]`, `(?P<n>a)(?P<`} {
		if got := nullableGroups(regex); got != nil {
			t.Errorf("nullableGroups(%q) = %v, want nil for an unparseable regex", regex, got)
		}
	}
}

// TestMatchesEmptyUnclassifiedOperatorIsNullable covers the switch's
// final fallthrough, which no regex string can reach: every operator
// Go's parser emits from a well-formed pattern is named by a case above
// it. [syntax.OpNoMatch] is constructed directly here for that reason.
//
// The arm is not dead weight. It is the guarantee that a FUTURE
// operator — one a newer Go regexp/syntax introduces, or one a
// simplification pass produces — defaults to nullable and therefore to
// rejection, instead of falling through to a verdict that would let
// cerberus emit the expressible form for a group it never analysed.
func TestMatchesEmptyUnclassifiedOperatorIsNullable(t *testing.T) {
	t.Parallel()

	if !matchesEmpty(&syntax.Regexp{Op: syntax.OpNoMatch}) {
		t.Error("matchesEmpty(OpNoMatch) = false, want true — an operator the switch does " +
			"not classify must read as nullable so the shared-name rejection is kept")
	}
}

// TestMatchesEmptyZeroRuneLiteral pins the one literal shape that IS
// nullable. `OpLiteral` normally carries at least one rune, and the
// length check is what keeps a zero-rune literal — reachable by
// constructing or simplifying rather than by parsing — from being read
// as "consumes a character" and wrongly making its group expressible.
func TestMatchesEmptyZeroRuneLiteral(t *testing.T) {
	t.Parallel()

	if !matchesEmpty(&syntax.Regexp{Op: syntax.OpLiteral}) {
		t.Error("matchesEmpty(zero-rune OpLiteral) = false, want true — it matches only " +
			"the empty string")
	}
}
