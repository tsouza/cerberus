package qlcommon

import "testing"

// TestScanSourceGroupsLocatesGroups pins the offsets the rewrite inserts
// at. Every case is a source whose group boundaries a naive
// paren-counting scan would get wrong — an escaped parenthesis, a
// parenthesis inside a character class, a class whose first member is the
// `]` that would otherwise close it, a POSIX name carrying its own `]`,
// and the `(?…)` spellings Go accepts.
//
// A wrong offset here does not fail loudly: it brackets a different span,
// and the probe then answers a different question than the one asked. So
// the assertion is on the exact substring each group's body spans.
func TestScanSourceGroupsLocatesGroups(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		// bodies is the body substring of each group, in source order.
		bodies []string
		// captures is the capture index of each group, 0 when it does not
		// capture.
		captures []int
	}{
		{"plain_group", `a(b)c`, []string{"b"}, []int{1}},
		{"non_capturing", `a(?:b)c`, []string{"b"}, []int{0}},
		{"named", `a(?P<n>b)c`, []string{"b"}, []int{1}},
		{"named_short_spelling", `a(?<n>b)c`, []string{"b"}, []int{1}},
		{"nested", `((a)(b))`, []string{"(a)(b)", "a", "b"}, []int{1, 2, 3}},
		{"mixed_capturing_and_not", `(?:(a))`, []string{"(a)", "a"}, []int{0, 1}},

		// An escaped parenthesis is literal text, not a group.
		{"escaped_parens", `\((a)\)`, []string{"a"}, []int{1}},
		{"escaped_backslash_then_group", `\\(a)`, []string{"a"}, []int{1}},

		// Parentheses inside a character class are members of it.
		{"parens_in_a_class", `[()](a)`, []string{"a"}, []int{1}},
		{"leading_bracket_member", `[]()](a)`, []string{"a"}, []int{1}},
		{"negated_leading_bracket_member", `[^]()](a)`, []string{"a"}, []int{1}},
		{"posix_class", `[[:alpha:]](a)`, []string{"a"}, []int{1}},
		{"escaped_bracket_in_a_class", `[\]()](a)`, []string{"a"}, []int{1}},

		// A SCOPED flag group carries its setting with it, so wrapping it
		// cannot move where the setting applies.
		{"flag_group", `(?i:a)(b)`, []string{"a", "b"}, []int{0, 1}},
		{"flag_group_with_minus", `(?i-s:a)(b)`, []string{"a", "b"}, []int{0, 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			groups, ok := scanSourceGroups(tc.src)
			if !ok {
				t.Fatalf("scanSourceGroups(%q) declined; want it to scan", tc.src)
			}
			// Element 0 is the virtual whole-pattern group.
			real := groups[1:]
			if len(real) != len(tc.bodies) {
				t.Fatalf("scanSourceGroups(%q) found %d groups, want %d: %+v",
					tc.src, len(real), len(tc.bodies), real)
			}
			for i, g := range real {
				if body := tc.src[g.bodyStart:g.bodyEnd]; body != tc.bodies[i] {
					t.Errorf("scanSourceGroups(%q) group %d spans %q, want %q",
						tc.src, i, body, tc.bodies[i])
				}
				if g.capIndex != tc.captures[i] {
					t.Errorf("scanSourceGroups(%q) group %d has capture index %d, want %d",
						tc.src, i, g.capIndex, tc.captures[i])
				}
			}
		})
	}
}

// TestScanSourceGroupsDeclines pins the conservatism the rewrite's
// soundness rests on. The scanner computes byte offsets that another
// function then inserts parentheses at, so a construct it cannot classify
// must make it decline outright — guessing an offset would bracket a span
// nobody chose, and the probe would answer for the wrong thing while
// still compiling.
func TestScanSourceGroupsDeclines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"unclosed_group", `(a`},
		{"unopened_group", `a)`},
		{"trailing_backslash", `a\`},
		{"unterminated_class", `[abc`},
		{"unterminated_posix_class", `[[:alpha`},
		{"trailing_backslash_in_a_class", `[a\`},
		{"truncated_named_group", `(?P<n`},
		{"truncated_short_named_group", `(?<n`},
		{"named_group_without_bracket", `(?Pn>a)`},
		{"unknown_group_construct", `(?=a)`},
		{"truncated_flag_group", `(?i`},
		{"bare_question_group", `(?`},

		// A bare flag SETTING reaches to the end of its enclosing group,
		// across alternation branches. Wrapping a branch would confine it
		// and change which strings the pattern accepts, so a pattern
		// carrying one is never rewritten.
		{"bare_flag_setting", `(?i)(a)`},
		{"bare_flag_setting_with_minus", `(?i-s)(a)`},
		{"bare_flag_setting_inside_a_group", `(?:(?i)a|b)`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := scanSourceGroups(tc.src); ok {
				t.Errorf("scanSourceGroups(%q) scanned it; want a decline, because an offset "+
					"guessed for a construct this does not model would silently bracket the "+
					"wrong span", tc.src)
			}
		})
	}
}

// TestPlanCaptureProbesDeclines pins the planner's own fail-closed paths:
// a pattern it cannot rewrite must yield no plan rather than a partial
// one, because every index in a decomposition is numbered against the
// rewrite and a half-applied one would renumber without answering.
func TestPlanCaptureProbesDeclines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		need  []int
	}{
		{"nothing_to_probe", `(?P<dup>a)|(?P<dup>b)`, nil},
		{"carrier_with_no_probeable_ancestor", `(?:(?P<dup>a?)|y)(?P<dup>b)`, []int{1}},
		{"carrier_alone_under_a_quest", `(?:(?P<dup>a?))?(?P<dup>b)`, []int{1}},
		{"unparseable_regex", `(?P<dup>a`, []int{1}},
		{"index_past_the_group_count", `(?P<dup>a?)(?P<dup>b)`, []int{9}},
		// A pattern already carrying a probe name would make the rewrite
		// unreadable back off SubexpNames.
		{"probe_name_collision", `(?:x(?P<` + probeNamePrefix + `9>a?))?(?P<dup>b)`, []int{1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if plan, ok := planCaptureProbes(tc.regex, tc.need); ok {
				t.Errorf("planCaptureProbes(%q, %v) planned %+v; want a decline",
					tc.regex, tc.need, plan)
			}
		})
	}
}
