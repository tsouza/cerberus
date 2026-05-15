package promql

import "testing"

// nineCaptureRegex is a 9-group regex used as the "always-has-enough-
// captures" sentinel in the table-driven test below. Each case that
// references `$N` for any single-digit N expects the rewrite to succeed
// because this regex has all 9 groups.
const nineCaptureRegex = `(.)(.)(.)(.)(.)(.)(.)(.)(.)`

// TestPromReplacementToCH locks down the PromQL → ClickHouse replacement
// template rewrite that label_replace relies on. PromQL uses Go's
// `regexp.Regexp.ExpandString` syntax (`$1`, `${1}`, `$$`); CH's
// `replaceRegexpOne` uses backslash escapes (`\1`, `\\`). Without the
// rewrite, capture-group references are emitted as literal `$N` text and
// the substituted label value is wrong (e.g., `svc-$1` instead of
// `svc-prod`).
func TestPromReplacementToCH(t *testing.T) {
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
		// so `$1` (the PromQL backref) has no source — Prom's
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
			got := promReplacementToCH(tc.in, tc.regex)
			if got != tc.want {
				t.Fatalf("promReplacementToCH(%q, %q): got %q, want %q",
					tc.in, tc.regex, got, tc.want)
			}
		})
	}
}
