package qlcommon

import (
	"strings"
	"testing"
)

// nineCaptureRegex is a 9-group regex used as the "always-has-enough-
// captures" sentinel in the table-driven test below. Each case that
// references `$N` for any single-digit N expects the rewrite to succeed
// because this regex has all 9 groups.
const nineCaptureRegex = `(.)(.)(.)(.)(.)(.)(.)(.)(.)`

// tenCaptureRegex has one group MORE than ClickHouse's `\9`
// substitution ceiling, so `$10` against it names a group that really
// exists and really cannot be expressed — the one shape
// [ReplacementToCH] rejects instead of translating.
const tenCaptureRegex = `(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)`

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
		{"braced_with_trailing_text", "xy${1}z", nineCaptureRegex, `xy\1z`},
		{"lone_dollar_at_end", "abc$", nineCaptureRegex, "abc$"},
		// Every case below is an ExpandString shape a single-digit scan
		// gets wrong. The `want` column is the CH template whose
		// substitution reproduces what Go's ExpandString — the engine
		// PromQL's and LogQL's label_replace both run the replacement
		// through — actually yields; each was read off ExpandString
		// rather than reasoned about. Go takes the LONGEST run of name
		// characters after the `$`, so all of these are ONE reference,
		// and each binds to nothing against a 9-group regex, which
		// ExpandString renders as the empty string.
		{"multi_digit_above_group_count", "$10", nineCaptureRegex, ""},
		{"braced_multi_digit_above_group_count", "${10}", nineCaptureRegex, ""},
		{"multi_digit_nineteen", "$19", nineCaptureRegex, ""},
		{"unknown_named_capture_braced", "${name}", nineCaptureRegex, ""},
		{"unknown_named_capture_bare", "$x", nineCaptureRegex, ""},
		{"unknown_named_capture_underscore", "$_", nineCaptureRegex, ""},
		// `$1x` is a reference to a group NAMED `1x`, not group 1
		// followed by a literal `x` — the digit-scan reading emitted
		// `\1x`, which substitutes a real capture and is the
		// silently-wrong label the issue describes.
		{"digit_then_letter_is_a_name", "$1x", nineCaptureRegex, ""},
		// Leading zeros disqualify a reference from being an index, so
		// `$01` is likewise a NAME lookup that finds nothing.
		{"leading_zero_is_a_name", "$01", nineCaptureRegex, ""},
		// Overflowing index — Go's parse loop gives up and treats it as
		// a name, which no group carries.
		{"index_overflow_is_a_name", "$99999999999999999999", nineCaptureRegex, ""},
		{"literal_context_around_dropped_ref", "a-$x-b", nineCaptureRegex, "a--b"},
		// `$` followed by a byte that can't start a name is not a
		// reference at all; ExpandString emits the `$` verbatim.
		{"dollar_then_non_name_byte", "$-", nineCaptureRegex, "$-"},
		// Named captures resolve to the index of the group carrying the
		// name — the whole point of the rewrite.
		{"named_capture_resolves", "$svc", `(?P<svc>.*)`, `\1`},
		{"named_capture_braced_resolves", "${svc}-x", `(?P<svc>.*)`, `\1-x`},
		{"named_capture_second_group", "$svc", `(a)(?P<svc>.*)`, `\2`},
		{"named_group_also_reachable_by_index", "$1", `(?P<svc>.*)`, `\1`},
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
		// Boundary guards on [extractRef]'s brace handling and on the
		// capture-count gate. Each row is calibrated so the original
		// produces the listed `want` and a boundary-mutated variant
		// produces an observably different string (or panics), which is
		// what lets the mutation tool prove the bounds are load-bearing.
		//
		//  * `braced_zero_with_caps` / `braced_nine_with_caps` pin the
		//    `ref.num <= g.count` capture-count gate at both ends of
		//    the single-digit range against a regex with exactly 9
		//    groups: `< g.count` drops `\9`, and mishandling index 0
		//    (the whole match, always valid) drops `\0`.
		{"braced_zero_with_caps", "${0}", nineCaptureRegex, `\0`},
		{"braced_nine_with_caps", "${9}", nineCaptureRegex, `\9`},
		//  * `braced_unclosed_before_text` pins the missing-closing-
		//    brace rejection in extractRef. Dropping that check reads
		//    `1` as a reference and emits `\1x` instead of the verbatim
		//    text ExpandString produces.
		{"braced_unclosed_before_text", "${1x", nineCaptureRegex, "${1x"},
		//  * `braced_truncated_digit` pins the same rejection at
		//    end-of-string, where a missing bounds check reads past the
		//    end of the template.
		{"braced_truncated_digit", "${1", nineCaptureRegex, "${1"},
		//  * `empty_braces` pins the zero-length-name rejection: `${}`
		//    carries no name, so it is not a reference.
		{"empty_braces", "${}", nineCaptureRegex, "${}"},
		//  * `brace_width_accounting` pins the `width += len("{}")`
		//    term. If the braces aren't counted, the loop resumes
		//    inside the reference and replays `}` as literal text.
		{"brace_width_accounting", "${9}tail", nineCaptureRegex, `\9tail`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(tc.in, tc.regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q, %q): unexpected error: %v", tc.in, tc.regex, err)
			}
			if len(got.Segments) != 0 {
				t.Fatalf("ReplacementToCH(%q, %q): want the replaceRegexpOne form, got segments %+v",
					tc.in, tc.regex, got.Segments)
			}
			if got.Template != tc.want {
				t.Fatalf("ReplacementToCH(%q, %q): got %q, want %q",
					tc.in, tc.regex, got.Template, tc.want)
			}
		})
	}
}

// TestReplacementToCHRejectsInexpressibleBackrefs pins the one shape that
// Go's ExpandString resolves to a real capture but no ClickHouse
// expression can reproduce: a name several capture groups share where at
// least one of them can match the EMPTY STRING. ExpandString picks the
// first carrier that TOOK PART in the row's match, and participation is
// not observable from SQL — `extractGroups` reports a group that did not
// participate and a group that matched the empty string identically, as
// `”`. A nullable carrier is exactly the case where those two states can
// both occur, so the choice is unrecoverable.
//
// Carriers that CANNOT match the empty string are a different matter, and
// are translated rather than rejected — see
// TestReplacementToCHSegmentsSharedCaptureName. That is why every case
// here has a nullable carrier: without one the guard does not fire, so a
// case whose carriers are all non-nullable would pass this test for the
// wrong reason.
//
// It used to be emitted as literal template text — a 200 carrying a label
// whose value was the template rather than an expansion of it, which is
// the silently-wrong output issue #1490 reported. Rejecting at lowering
// makes the failure loud instead.
func TestReplacementToCHRejectsInexpressibleBackrefs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		regex    string
		wantErrs []string
	}{
		{
			// Carrier 1 is `a?`, which matches the empty string: it can
			// take part AND report `''`, which is also what a carrier that
			// took no part reports.
			"nullable_first_carrier",
			"$dup",
			`(?P<dup>a?)|(?P<dup>b)`,
			[]string{`"dup"`, "2 capture groups", "empty string", "capture group 1"},
		},
		{
			// Nullability anywhere in the carrier set is disqualifying,
			// not just at the front: Go picks the FIRST participant, so a
			// later nullable carrier that matched empty must not be
			// skipped over by a "first non-empty" search either.
			"nullable_later_carrier",
			"$dup",
			`(?P<dup>a)|(?P<dup>b*)`,
			[]string{`"dup"`, "2 capture groups", "empty string", "capture group 2"},
		},
		{
			// Nullability is a property of the whole subpattern, not of
			// its outermost operator: `x*y*` is a concat of two nullable
			// parts and so is itself nullable.
			"nullable_via_concatenation",
			"$dup",
			`(?P<dup>x*y*)|(?P<dup>b)`,
			[]string{`"dup"`, "2 capture groups", "empty string"},
		},
		{
			// One nullable branch makes the alternation nullable.
			"nullable_via_alternation_branch",
			"$dup",
			`(?P<dup>ab|)|(?P<dup>c)`,
			[]string{`"dup"`, "2 capture groups", "empty string"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(tc.in, tc.regex)
			if err == nil {
				t.Fatalf("ReplacementToCH(%q, %q): want error, got %+v", tc.in, tc.regex, got)
			}
			if got.Template != "" || got.Segments != nil {
				t.Fatalf("ReplacementToCH(%q, %q): want a zero result alongside the error, got %+v",
					tc.in, tc.regex, got)
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("ReplacementToCH(%q, %q): error %q does not name %q",
						tc.in, tc.regex, err, want)
				}
			}
		})
	}
}

// TestReplacementToCHIndexBelowCeilingSurvivesLargeRegex guards the
// rejection above from over-firing: a regex with more than nine groups
// is fine as long as the replacement doesn't reach past `\9`.
func TestReplacementToCHIndexBelowCeilingSurvivesLargeRegex(t *testing.T) {
	t.Parallel()
	got, err := ReplacementToCH("$9", tenCaptureRegex)
	if err != nil {
		t.Fatalf("ReplacementToCH($9, tenCaptureRegex): unexpected error: %v", err)
	}
	if len(got.Segments) != 0 {
		t.Fatalf("ReplacementToCH($9, tenCaptureRegex): want the replaceRegexpOne form, got segments %+v", got.Segments)
	}
	if want := `\9`; got.Template != want {
		t.Fatalf("ReplacementToCH($9, tenCaptureRegex): got %q, want %q", got.Template, want)
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
		// Multi-digit indices and named captures resolve to "" like any
		// other reference: against an empty match a group that took
		// part expanded to "", and one that binds to nothing expands to
		// "" as well, so the two cases are indistinguishable here.
		{"braced_multi_digit", "${10}", ""},
		{"multi_digit_unbraced", "$10", ""},
		{"named_capture", "${name}", ""},
		{"named_capture_bare", "$svc", ""},
		{"digit_then_letter_is_a_name", "$1x", ""},
		{"leading_zero_is_a_name", "$01", ""},
		// Edge cases that ExpandString handles literally: a `$` that
		// starts no reference stays a `$`.
		{"lone_dollar_at_end", "abc$", "abc$"},
		{"dollar_then_non_name_byte", "$-", "$-"},
		{"empty_braces", "${}", "${}"},
		{"braced_unclosed_before_text", "${1x", "${1x"},
		// Literal backslashes pass through unchanged — only `$`-prefixed
		// metacharacters are interpreted in the Go regex replacement
		// template the QLs feed us.
		{"existing_backslash_preserved", `\1`, `\1`},
		// Boundary guards mirroring the ReplacementToCH set, calibrated
		// for the empty-captures resolver (every recognised reference
		// collapses to "" instead of to `\N`).
		//
		//  * `multi_digit_unbraced_nine` pins that the reference runs to
		//    the END of the digit run. A splitter that stopped after one
		//    digit would leave the trailing `9` in the output.
		{"multi_digit_unbraced_nine", "$19", ""},
		{"braced_zero", "${0}", ""},
		{"braced_nine", "${9}", ""},
		//  * `braced_non_digit_inner` pins that a braced NAME is a
		//    reference too, not literal text.
		{"braced_non_digit_inner", "${x}", ""},
		//  * `braced_truncated_digit` pins the missing-closing-brace
		//    rejection, including the bounds check that a mutant would
		//    read past.
		{"braced_truncated_digit", "${1", "${1"},
		//  * `braced_with_trailing_text` pins the reference's width
		//    accounting. The literal prefix `xy` shifts the `${1}` to
		//    offset 2; if the consumed-byte count is short, the next
		//    iteration lands inside the brace block, replays the suffix
		//    as plain text and emits `xy1}z` instead of `xyz`.
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
