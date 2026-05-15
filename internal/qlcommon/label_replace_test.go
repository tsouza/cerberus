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
