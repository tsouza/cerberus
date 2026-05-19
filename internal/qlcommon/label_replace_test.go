package qlcommon

import "testing"

// nineCaptureRegex is a 9-group regex used as the "always-has-enough-
// captures" sentinel in the table-driven test below. Each case that
// references `$N` for any single-digit N expects the rewrite to succeed
// because this regex has all 9 groups.
const nineCaptureRegex = `(.)(.)(.)(.)(.)(.)(.)(.)(.)`

// TestReplacementToCH locks down the QL → ClickHouse replacement
// template rewrite that PromQL + LogQL `label_replace` both rely on.
// Both QLs use Go's `regexp.Regexp.ExpandString` syntax (`$1`,
// `${1}`, `$$`); CH's `replaceRegexpOne` uses backslash escapes
// (`\1`, `\\`). Without the rewrite, capture-group references are
// emitted as literal `$N` text and the substituted label value is
// wrong (e.g., `svc-$1` instead of `svc-prod`).
func TestReplacementToCH(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		in    string
		regex string
		want  string
	}{
		{"empty", "", nineCaptureRegex, ""},
		{"literal_no_dollar", "hello", nineCaptureRegex, "hello"},
		{"single_digit_backref", "$1", nineCaptureRegex, `\1`},
		{"prefix_then_backref", "svc-$1", nineCaptureRegex, `svc-\1`},
		{"backref_zero_whole_match", "$0", nineCaptureRegex, `\0`},
		{"backref_nine", "$9", nineCaptureRegex, `\9`},
		{"dollar_dollar_literal", "$$", nineCaptureRegex, "$"},
		{"dollar_dollar_then_text", "$$x", nineCaptureRegex, "$x"},
		{"braced_single_digit", "${1}-suffix", nineCaptureRegex, `\1-suffix`},
		{"braced_multi_digit_preserved", "${10}", nineCaptureRegex, "${10}"},
		{"multi_digit_unbraced_preserved", "$10", nineCaptureRegex, "$10"},
		{"named_capture_preserved", "${name}", nineCaptureRegex, "${name}"},
		{"lone_dollar_at_end", "abc$", nineCaptureRegex, "abc$"},
		{"dollar_letter_preserved", "$x", nineCaptureRegex, "$x"},
		{"existing_backslash_escaped", `\1`, nineCaptureRegex, `\\1`},
		{"existing_backslash_and_backref", `\$1`, nineCaptureRegex, `\\\1`},
		// Out-of-range backrefs drop. The regex has 0 capture groups,
		// so `$1` (the QL backref) has no source — Prom/Loki's
		// ExpandString substitutes the empty string. CH's
		// replaceRegexpOne rejects `\1` against a 0-group regex at
		// SQL-parse time even when match() short-circuits the call,
		// so the rewrite trims the backref entirely. The expected
		// output preserves the literal context around the dropped
		// substitution.
		{"out_of_range_backref_no_groups", "value-$1", "non-matching-regex", "value-"},
		{"out_of_range_braced_no_groups", "value-${1}", "non-matching-regex", "value-"},
		// Partially-out-of-range: regex has 1 group, replacement
		// references $1 (OK) and $2 (out of range). $1 survives, $2
		// drops.
		{"partial_out_of_range", "$1-$2", "foo(bar)", `\1-`},
		// Invalid regex — Compile fails so the rewrite falls back to
		// allowing all single-digit backrefs (CH's own parse stage
		// will surface the regex error to the client). The replacement
		// translation is unchanged from the always-allowed shape.
		{"invalid_regex_passthrough", "$1", "(.*", `\1`},
		// Targeted mutation-kill cases pinning the gremlins CONDITIONALS_
		// BOUNDARY / INVERT_LOGICAL / ARITHMETIC_BASE / REMOVE_SELF_
		// ASSIGNMENTS mutants on the multi-digit-preserve and `${N}`
		// branches at lines 105 / 122 / 124 in `label_replace.go`. Each
		// row below is calibrated so the original implementation
		// produces the listed `want`, and at least one boundary-mutated
		// variant produces an observably different string (or panics).
		// Treat these as low-level guards — the user-facing semantics
		// are already covered above; these only widen the discriminator
		// set the mutation tool can use to prove the boundaries are
		// load-bearing.
		//
		//  * `multi_digit_unbraced_nine` pins the `<= '9'` upper bound
		//    of the inner multi-digit lookahead at line 105:65. Char
		//    at i+2 is exactly '9', so the boundary mutant `< '9'`
		//    misses the branch and emits the single-digit form `\19`
		//    instead of the verbatim `$19`.
		{"multi_digit_unbraced_nine", "$19", nineCaptureRegex, "$19"},
		//  * `braced_zero_with_caps` pins the `>= '0'` lower bound of
		//    the braced digit check at line 122:42. Char at i+2 is
		//    exactly '0', so the boundary mutant `> '0'` skips the
		//    branch and falls through to write the literal `${0}`
		//    instead of the canonical `\0`.
		{"braced_zero_with_caps", "${0}", nineCaptureRegex, `\0`},
		//  * `braced_nine_with_caps` pins both the `<= '9'` upper
		//    bound of the braced digit check at line 122:65 AND the
		//    `n <= allowed` capture-count gate at line 124:20. Char at
		//    i+2 is exactly '9' and the regex has exactly 9 capture
		//    groups, so both boundary mutants (`< '9'` and `< allowed`)
		//    diverge from the canonical `\9`.
		{"braced_nine_with_caps", "${9}", nineCaptureRegex, `\9`},
		//  * `braced_non_digit_inner` pins both INVERT_LOGICAL mutants
		//    on the chained `&&` at line 122:26 / 122:49. The char at
		//    i+2 is a non-digit (`x`), so the original short-circuits
		//    on `'x' <= '9'` and falls through to the literal-write
		//    path. Either `&&` → `||` mutation upgrades the partial
		//    truth into a full match, enters the consuming branch and
		//    drops the entire `${x}` from the output.
		{"braced_non_digit_inner", "${x}", nineCaptureRegex, "${x}"},
		//  * `braced_truncated_digit` pins the ARITHMETIC_BASE mutant
		//    on `i+3` at line 122:8 AND the CONDITIONALS_BOUNDARY
		//    mutant on `<` at line 122:11. The input is one byte short
		//    of a closing `}`, so the original short-circuits on
		//    `i+3 < len` (3 < 3 == false). The `i-3` mutant evaluates
		//    `-3 < 3` as true and continues into the OOB `escaped[i+3]`
		//    read; the `<=` mutant evaluates `3 <= 3` as true and does
		//    the same. Both surface as test-failing panics.
		{"braced_truncated_digit", "${1", nineCaptureRegex, "${1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ReplacementToCH(tc.in, tc.regex)
			if got != tc.want {
				t.Fatalf("ReplacementToCH(%q, %q): got %q, want %q",
					tc.in, tc.regex, got, tc.want)
			}
		})
	}
}

// TestEmptyCapturesReplacement locks the build-time pre-computation of
// the "all captures bind to empty" replacement template — the value a
// PromQL/LogQL `label_replace` should emit when the source label is
// absent (read as empty from the input map) AND the regex matches that
// empty string. The whole reason this helper exists is that CH ≤ 24.8's
// `replaceRegexpOne` silently passes the empty input through (returning
// `""` instead of substituting the replacement); the emit-time
// `if(empty(src), <emptyResult>, replaceRegexpOne(…))` short-circuit
// uses this helper to compute `<emptyResult>` ahead of time.
func TestEmptyCapturesReplacement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"literal_no_dollar", "hello", "hello"},
		// Single-digit numbered captures resolve to "" — the canonical
		// shape from the compat-residual diff
		// (`label_replace(m, dst, "value-$1", "nonexistent-src", "(.*)")`
		// → `dst="value-"`).
		{"single_digit_backref", "$1", ""},
		{"prefix_then_backref", "value-$1", "value-"},
		{"backref_zero_whole_match", "$0", ""},
		{"two_single_digit_refs", "$1-$2", "-"},
		{"backref_nine", "$9", ""},
		// `$$` collapses to a literal `$` (mirrors ExpandString).
		{"dollar_dollar_literal", "$$", "$"},
		{"dollar_dollar_then_text", "$$x", "$x"},
		// Braced single-digit form behaves the same as bare `$N`.
		{"braced_single_digit", "${1}-suffix", "-suffix"},
		// Multi-digit indices and named captures are preserved verbatim
		// — same opt-out as `ReplacementToCH`.
		{"braced_multi_digit_preserved", "${10}", "${10}"},
		{"multi_digit_unbraced_preserved", "$10", "$10"},
		{"named_capture_preserved", "${name}", "${name}"},
		// Edge cases that ExpandString handles literally.
		{"lone_dollar_at_end", "abc$", "abc$"},
		{"dollar_letter_preserved", "$x", "$x"},
		// Literal backslashes pass through unchanged — only `$`-prefixed
		// metacharacters are interpreted in the Go regex replacement
		// template the QLs feed us.
		{"existing_backslash_preserved", `\1`, `\1`},
		// Targeted mutation-kill cases pinning the gremlins CONDITIONALS_
		// BOUNDARY / INVERT_LOGICAL / ARITHMETIC_BASE / REMOVE_SELF_
		// ASSIGNMENTS mutants on the multi-digit-preserve and `${N}`
		// branches at lines 195 / 205 / 206 in `label_replace.go`. The
		// mirror of the ReplacementToCH cases above, recalibrated for
		// the empty-captures resolver (numbered captures collapse to
		// "" instead of `\N`, so the original on the consuming branch
		// returns the empty string).
		//
		//  * `multi_digit_unbraced_nine` pins the `<= '9'` upper bound
		//    at line 195:56. Char at i+2 is '9'; the boundary mutant
		//    `< '9'` falls into the single-digit-collapse branch and
		//    emits the trailing `9` alone instead of the verbatim
		//    `$19`.
		{"multi_digit_unbraced_nine", "$19", "$19"},
		//  * `braced_zero` pins the `>= '0'` lower bound at line 205:36.
		//    Char at i+2 is '0'; the boundary mutant `> '0'` skips the
		//    consuming branch and writes the literal `${0}` instead of
		//    the empty result the original produces.
		{"braced_zero", "${0}", ""},
		//  * `braced_nine` pins the `<= '9'` upper bound at line 205:56.
		//    Char at i+2 is '9'; the boundary mutant `< '9'` skips the
		//    consuming branch and writes the literal `${9}` instead of
		//    the empty result.
		{"braced_nine", "${9}", ""},
		//  * `braced_non_digit_inner` pins both INVERT_LOGICAL mutants
		//    on the chained `&&` at line 205:23 / 205:43. Char at i+2
		//    is non-digit; either `&&` → `||` swap upgrades the partial
		//    truth into a full match and erases the brace block.
		{"braced_non_digit_inner", "${x}", "${x}"},
		//  * `braced_truncated_digit` pins ARITHMETIC_BASE on `i+3` at
		//    line 205:8 AND CONDITIONALS_BOUNDARY on `<` at line 205:11.
		//    Same mechanism as the ReplacementToCH twin: the OOB
		//    `repl[i+3]` read panics under either mutant.
		{"braced_truncated_digit", "${1", "${1"},
		//  * `braced_with_trailing_text` pins the REMOVE_SELF_ASSIGNMENTS
		//    mutant at line 206:7 (`i += 3` → `i = 3`). The literal
		//    prefix `xy` shifts the `${1}` to offset 2, so the original
		//    advances `i` to 5 (consuming the brace block) and lands
		//    on `z`; the mutant resets `i` to 3, which lands on the
		//    digit `1` inside the brace block, replays the suffix as
		//    plain text and emits `xy1}z` instead of `xyz`.
		{"braced_with_trailing_text", "xy${1}z", "xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EmptyCapturesReplacement(tc.in)
			if got != tc.want {
				t.Fatalf("EmptyCapturesReplacement(%q): got %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}
