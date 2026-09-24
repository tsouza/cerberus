package qlcommon

import (
	"regexp"
	"testing"
)

// TestRegexAnchorsMaySplit pins the two ways a regex that fails to parse
// standalone can behave once [anchorRegex] wraps it, and the one way a
// regex that DOES parse standalone always behaves.
//
//   - `a)|(b` fails to compile alone (an unmatched `)`), but
//     `^(?s:a)|(b)$` compiles: the wrapper's own `(?s:` group closes on
//     the user's `)`, so `^` and `$` end up on separate alternation arms
//     instead of wrapping the whole pattern. This is the shape issue #3663
//     reported: a match can cover only a prefix or a suffix.
//   - `(.*` fails to compile alone AND once wrapped (`^(?s:(.*)$` is
//     itself unbalanced) — ClickHouse's own parser rejects this
//     regardless of which output form [chReplacement] picks, so it must
//     NOT be reported as split; doing so would needlessly move a query
//     that already fails at CH's parse stage onto the segments path.
//   - `a|b` compiles standalone, so the balanced-parens argument applies
//     directly: wrapping it can only nest the whole thing inside one more
//     non-capturing group, never split the anchors.
func TestRegexAnchorsMaySplit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		want  bool
	}{
		{"unbalanced_but_wraps_into_split_anchors", `a)|(b`, true},
		{"unbalanced_and_stays_unbalanced_wrapped", `(.*`, false},
		{"balanced_top_level_alternation", `a|b`, false},
		{"balanced_no_alternation", `host-(.*)`, false},
		{"empty_regex", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := regexAnchorsMaySplit(tc.regex); got != tc.want {
				t.Fatalf("regexAnchorsMaySplit(%q) = %v, want %v", tc.regex, got, tc.want)
			}
		})
	}
}

// TestReplacementToCHForcesSegmentsWhenAnchorsMaySplit is the #3663
// regression: a replacement whose every reference fits `replaceRegexpOne`'s
// `\0`-`\9` ceiling with no shared-name ambiguity used to always take the
// `Template` (substitution-string) form. That form substitutes the
// template for the MATCHED SPAN and keeps the rest of the source value
// around it — correct only when a match is guaranteed to span the whole
// value, which [anchorRegex]'s anchors do not guarantee for a regex like
// `a)|(b` (see TestRegexAnchorsMaySplit). This pins that such a regex now
// takes the segments form instead, which never splices anything but the
// template itself.
func TestReplacementToCHForcesSegmentsWhenAnchorsMaySplit(t *testing.T) {
	t.Parallel()

	got, err := ReplacementToCH("[$1]", `a)|(b`)
	if err != nil {
		t.Fatalf("ReplacementToCH: unexpected error: %v", err)
	}
	if got.Template != "" {
		t.Fatalf("ReplacementToCH(%q, %q): want the segments form (anchors may split), got template %q",
			"[$1]", `a)|(b`, got.Template)
	}
	if len(got.Segments) == 0 {
		t.Fatalf("ReplacementToCH(%q, %q): want a non-empty decomposition", "[$1]", `a)|(b`)
	}
}

// TestAnchorSplitRegexAgreesWithProduction is the differential that makes
// the #3663 fix trustworthy: it evaluates the SAME decomposition the
// emitter renders as SQL against the exact source values the issue's
// reproduction table pins, and checks the result against Go's own
// `ExpandString` — the engine reference Prometheus runs `label_replace`
// through. All three source values match through the FIRST alternation
// arm (`^(?s:a)`, RE2's leftmost-first semantics), which never populates
// group 1 (`(b)`), so `[$1]` must evaluate to the literal `[]` on every
// one — never the pre-fix `[]pi` / `[]-b` / `[]b`, which came from
// splicing the replacement into the matched SPAN instead of replacing the
// whole value.
func TestAnchorSplitRegexAgreesWithProduction(t *testing.T) {
	t.Parallel()

	const regex = `a)|(b`
	const repl = "[$1]"

	got, err := ReplacementToCH(repl, regex)
	if err != nil {
		t.Fatalf("ReplacementToCH: unexpected error: %v", err)
	}

	re := regexp.MustCompile(anchorRegex(regex))
	for _, src := range []string{"api", "a-b", "ab"} {
		match := re.FindStringSubmatchIndex(src)
		if match == nil {
			t.Fatalf("regex %q does not match %q — the oracle needs a match", regex, src)
		}
		want := string(re.ExpandString(nil, repl, src, match))
		if want != "[]" {
			t.Fatalf("test setup: Go's ExpandString(%q) against %q = %q, want the issue's pinned \"[]\"",
				repl, src, want)
		}

		evaluated := evaluateReplacement(got, regex, src)
		if evaluated != want {
			t.Fatalf("regex %q repl %q src %q: %+v evaluates to %q; Go's ExpandString gives %q",
				regex, repl, src, got, evaluated, want)
		}
	}
}
