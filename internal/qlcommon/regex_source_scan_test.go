package qlcommon

import (
	"strings"
	"testing"
)

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
		// An empty name is still a group opening as far as offsets go —
		// the `>` is found at distance zero, which is the boundary between
		// "found it" and "there is none". Whether Go's own parser then
		// accepts the name is its business, not the scanner's.
		{"empty_group_name", `(?P<>a)`, []string{"a"}, []int{1}},
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
		// An empty POSIX name (`::]` immediately) is the boundary where the
		// terminator search finds `:]` at offset 0 rather than failing to
		// find it at all — a construct this scanner still accepts, since
		// well-formedness of the POSIX name itself is `regexp/syntax`'s
		// business, not the offset scanner's.
		{"posix_class_empty_name", `[[::]](a)`, []string{"a"}, []int{1}},
		{"escaped_bracket_in_a_class", `[\]()](a)`, []string{"a"}, []int{1}},

		// A `\Q…\E` quoted run is skipped as one unit — including any `(`
		// inside it — and the scan must resume normally on whatever
		// follows, not stop at the run's end.
		{"quoted_run_then_group", `\Qfoo\E(a)`, []string{"a"}, []int{1}},

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

		// Each of these ends exactly where a guard is about to read the
		// next byte. They are the inputs that tell a bound that stops one
		// short from one that reads past the end.
		{"lone_open_paren", `(`},
		{"lone_backslash", `\`},
		{"lone_class_open", `[`},
		{"class_open_then_negation", `[^`},
		{"class_open_then_negated_bracket", `[^]`},
		{"truncated_named_prefix", `(?P`},
		{"class_ending_in_a_bracket", `[a[`},
		{"truncated_posix_open", `[[:`},
		{"truncated_posix_name", `[[:alpha`},
		{"class_ending_in_an_escape", `[a\`},
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
		{"carrier_with_no_probeable_ancestor", `(?:(?P<dup>a?)|y?)(?P<dup>b)`, []int{1}},
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

// TestArmsThatComputeNoOffsetStillConsumeAByte is the demonstration the
// single-advance reshape rests on, and it runs against the production
// scanners rather than a replica of them.
//
// Three of [scanSourceGroups]' arms — `|`, `)`, and every byte matching no
// case at all — write nothing to `next`. Under the shape they replaced,
// each carried its own `i++`, and an arm that omitted one spun on its byte
// forever; a progress guard existed to turn that spin into a decline. Here
// the omission is not a mistake that can be made: those arms ARE, in the
// old shape's terms, the arm that forgot to advance, and they consume a
// byte because the walker advances, not because they remembered to.
//
// The assertions are OFFSETS rather than a bare "it returned". A stalled
// cursor cannot produce them; neither can one that advanced by the wrong
// amount, which a guard on `i <= start` would have waved through.
func TestArmsThatComputeNoOffsetStillConsumeAByte(t *testing.T) {
	t.Parallel()

	t.Run("the_alternation_arm", func(t *testing.T) {
		t.Parallel()
		// `a`, `b` and `c` match no case; the three `|` take the arm that
		// records an offset and writes no cursor.
		groups, ok := scanSourceGroups(`a|b|c`)
		if !ok {
			t.Fatal("scanSourceGroups declined a pattern of ordinary bytes and bars")
		}
		want := []int{1, 3}
		got := groups[wholePatternGroup].alternations
		if len(got) != len(want) {
			t.Fatalf("alternations = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("alternations = %v, want %v — a cursor that moved by anything "+
					"other than one byte per arm cannot land on these", got, want)
			}
		}
	})

	t.Run("the_group_close_arm", func(t *testing.T) {
		t.Parallel()
		// The two `)` take the arm that closes a group and writes no cursor.
		groups, ok := scanSourceGroups(`(a)(b)`)
		if !ok {
			t.Fatal("scanSourceGroups declined two adjacent capture groups")
		}
		if len(groups) != 3 {
			t.Fatalf("scanned %d groups, want 3 (the whole pattern and two captures)",
				len(groups))
		}
		if groups[1].bodyEnd != 2 || groups[2].bodyEnd != 5 {
			t.Fatalf("group bodies end at %d and %d, want 2 and 5",
				groups[1].bodyEnd, groups[2].bodyEnd)
		}
	})

	t.Run("a_long_run_of_offset_free_arms", func(t *testing.T) {
		t.Parallel()
		// One byte per iteration has to hold across a long input, not just
		// on a short one: a cursor that advanced by two would record half
		// these offsets, and one that stalled would record none of them
		// because the scan would never return.
		const bars = 4096
		src := strings.Repeat(`|`, bars)
		groups, ok := scanSourceGroups(src)
		if !ok {
			t.Fatalf("scanSourceGroups declined %d bars", bars)
		}
		got := groups[wholePatternGroup].alternations
		if len(got) != bars {
			t.Fatalf("recorded %d alternations over %d bars", len(got), bars)
		}
		for i, at := range got {
			if at != i {
				t.Fatalf("alternation %d recorded at offset %d; the cursor did not move "+
					"exactly one byte per iteration", i, at)
			}
		}
	})

	t.Run("the_class_member_arm", func(t *testing.T) {
		t.Parallel()
		// Inside skipCharClass every ordinary member matches no case, so
		// the whole class body is scanned by the default advance alone.
		const members = 4096
		src := `[` + strings.Repeat(`a`, members) + `]`
		end, ok := skipCharClass(src, 0)
		if !ok {
			t.Fatalf("skipCharClass declined a class of %d ordinary members", members)
		}
		if end != len(src) {
			t.Fatalf("skipCharClass ended at %d, want %d", end, len(src))
		}
	})
}

// TestScannerHelpersAlwaysConsumeAByte pins the one thing the single-advance
// shape does NOT decide on its own.
//
// The walker guarantees that an arm writing no offset still consumes a
// byte. It cannot guarantee anything about the arms that DO write one:
// those take their offset from a helper, and a helper returning the offset
// it was handed would stall the walker exactly as the old shape's forgotten
// `i++` did. That is the helpers' own contract rather than the loop's, so
// it is asserted here directly, over the scanner corpus and over a sweep of
// every single byte, at every position each helper can legally be called
// at.
func TestScannerHelpersAlwaysConsumeAByte(t *testing.T) {
	t.Parallel()

	sources := append([]string{}, scannerSyntaxCorpus...)
	for _, src := range scannerSyntaxCorpus {
		sources = append(sources, anchorRegex(src))
	}
	for b := 0; b < 256; b++ {
		sources = append(
			sources,
			string([]byte{byte(b)}),
			`\`+string([]byte{byte(b)}),
			`[`+string([]byte{byte(b)})+`]`,
			`(`+string([]byte{byte(b)})+`)`,
			`[[:`+string([]byte{byte(b)})+`:]]`,
		)
	}

	calls := 0
	for _, src := range sources {
		for i := range len(src) {
			switch src[i] {
			case '\\':
				if i+1 < len(src) && src[i+1] == 'Q' {
					calls++
					if end := endOfQuotedRun(src, i); end <= i {
						t.Fatalf("endOfQuotedRun(%q, %d) = %d, which does not move the "+
							"cursor past the byte it was handed", src, i, end)
					}
				}
			case '[':
				if end, ok := skipCharClass(src, i); ok {
					calls++
					if end <= i {
						t.Fatalf("skipCharClass(%q, %d) = %d, which does not move the "+
							"cursor past the byte it was handed", src, i, end)
					}
				}
			case '(':
				if bodyStart, _, ok := classifyGroupOpen(src, i); ok {
					calls++
					if bodyStart <= i {
						t.Fatalf("classifyGroupOpen(%q, %d) = %d, which does not move the "+
							"cursor past the byte it was handed", src, i, bodyStart)
					}
				}
			}
		}
	}
	if calls == 0 {
		t.Fatal("no helper was called — this test would pass vacuously")
	}
	t.Logf("every one of %d helper calls returned an offset past its input", calls)
}
