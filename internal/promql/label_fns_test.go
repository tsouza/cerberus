package promql

import "testing"

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
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"literal_no_dollar", "hello", "hello"},
		{"single_digit_backref", "$1", `\1`},
		{"prefix_then_backref", "svc-$1", `svc-\1`},
		{"backref_zero_whole_match", "$0", `\0`},
		{"backref_nine", "$9", `\9`},
		{"dollar_dollar_literal", "$$", "$"},
		{"dollar_dollar_then_text", "$$x", "$x"},
		{"braced_single_digit", "${1}-suffix", `\1-suffix`},
		{"braced_multi_digit_preserved", "${10}", "${10}"},
		{"multi_digit_unbraced_preserved", "$10", "$10"},
		{"named_capture_preserved", "${name}", "${name}"},
		{"lone_dollar_at_end", "abc$", "abc$"},
		{"dollar_letter_preserved", "$x", "$x"},
		{"existing_backslash_escaped", `\1`, `\\1`},
		{"existing_backslash_and_backref", `\$1`, `\\\1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := promReplacementToCH(tc.in)
			if got != tc.want {
				t.Fatalf("promReplacementToCH(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
