package qlcommon

import (
	"regexp"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestReplacementToCHSegmentsAboveCeiling pins the shape that used to be
// rejected outright: a replacement referencing a capture group above
// ClickHouse's `\9` substitution ceiling. `replaceRegexpOne` has no tenth
// slot, so the template is decomposed instead and the emitter indexes
// `extractGroups`, which returns every group as an Array(String) and has
// no ceiling at all.
//
// Reference Prometheus expands `$10` to the tenth group's text; the
// decomposition has to name exactly that group, in order, with the
// literal runs around it preserved.
func TestReplacementToCHSegmentsAboveCeiling(t *testing.T) {
	t.Parallel()

	lit := func(s string) chplan.LabelReplaceSegment {
		return chplan.LabelReplaceSegment{Literal: s, Group: chplan.NoCaptureGroup}
	}
	grp := func(n int) chplan.LabelReplaceSegment {
		return chplan.LabelReplaceSegment{Group: n}
	}

	cases := []struct {
		name  string
		in    string
		regex string
		want  []chplan.LabelReplaceSegment
	}{
		{"bare_tenth_group", "$10", tenCaptureRegex, []chplan.LabelReplaceSegment{grp(10)}},
		{"braced_tenth_group", "${10}", tenCaptureRegex, []chplan.LabelReplaceSegment{grp(10)}},
		{
			"literal_runs_around_the_reference",
			"pre-${10}-post",
			tenCaptureRegex,
			[]chplan.LabelReplaceSegment{lit("pre-"), grp(10), lit("-post")},
		},
		{
			// A high reference drags the whole template onto the segments
			// path, so the low groups alongside it must be carried too.
			"mixed_low_and_high_groups",
			"$1/$10",
			tenCaptureRegex,
			[]chplan.LabelReplaceSegment{grp(1), lit("/"), grp(10)},
		},
		{
			// `$0` is the whole match. The emitter reads it off the source
			// value rather than from extractGroups, which numbers from the
			// first real group.
			"whole_match_alongside_high_group",
			"$0|$10",
			tenCaptureRegex,
			[]chplan.LabelReplaceSegment{grp(chplan.WholeMatchGroup), lit("|"), grp(10)},
		},
		{
			// `$$` collapses to a literal `$`, and a backslash stays a
			// real backslash: segments carry DECODED literal text, because
			// the concat form binds it as a parameter and needs no
			// `replaceRegexpOne` escaping.
			"decoded_literal_text",
			`$$a\b$10`,
			tenCaptureRegex,
			[]chplan.LabelReplaceSegment{lit(`$a\b`), grp(10)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(tc.in, tc.regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q, %q): unexpected error: %v", tc.in, tc.regex, err)
			}
			if got.Template != "" {
				t.Fatalf("ReplacementToCH(%q, %q): want the segments form, got template %q",
					tc.in, tc.regex, got.Template)
			}
			if len(got.Segments) != len(tc.want) {
				t.Fatalf("ReplacementToCH(%q, %q): got %d segments %+v, want %d %+v",
					tc.in, tc.regex, len(got.Segments), got.Segments, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if !got.Segments[i].Equal(tc.want[i]) {
					t.Fatalf("ReplacementToCH(%q, %q): segment %d: got %+v, want %+v",
						tc.in, tc.regex, i, got.Segments[i], tc.want[i])
				}
			}
		})
	}
}

// TestReplacementToCHSegmentsSharedCaptureName pins the decomposition for
// a `$name` reference that several capture groups carry — the shape #1768
// reported cerberus rejecting outright.
//
// Go's ExpandString picks the first carrier that took part in the match.
// When no carrier's subpattern can match the empty string, "took part"
// and "captured something non-empty" are the same predicate, so the
// reference becomes a selection among the carriers' `extractGroups`
// subscripts. The decomposition records that as the first carrier in
// Group with the rest in Fallbacks, in regex order — the order is
// load-bearing, because Go picks the FIRST participant.
func TestReplacementToCHSegmentsSharedCaptureName(t *testing.T) {
	t.Parallel()

	lit := func(s string) chplan.LabelReplaceSegment {
		return chplan.LabelReplaceSegment{Literal: s, Group: chplan.NoCaptureGroup}
	}

	cases := []struct {
		name  string
		in    string
		regex string
		want  []chplan.LabelReplaceSegment
	}{
		{
			"two_carriers",
			"$dup",
			`(?P<dup>a)|(?P<dup>b)`,
			[]chplan.LabelReplaceSegment{{Group: 1, Fallbacks: []int{2}}},
		},
		{
			"three_carriers",
			"${dup}",
			`(?P<dup>a)|(?P<dup>b)|(?P<dup>c)`,
			[]chplan.LabelReplaceSegment{{Group: 1, Fallbacks: []int{2, 3}}},
		},
		{
			// Carriers need not be adjacent indices: an unrelated group
			// between them keeps its own index and is not a carrier.
			"carriers_straddling_an_unrelated_group",
			"$dup",
			`(?P<dup>a)|(?P<other>o)|(?P<dup>b)`,
			[]chplan.LabelReplaceSegment{{Group: 1, Fallbacks: []int{3}}},
		},
		{
			// Literal runs and single-group references coexist with the
			// shared-name selection in one decomposition.
			"mixed_with_literals_and_a_plain_group",
			"pre-$dup/$other",
			`(?P<dup>a)|(?P<other>o)|(?P<dup>b)`,
			[]chplan.LabelReplaceSegment{
				lit("pre-"),
				{Group: 1, Fallbacks: []int{3}},
				lit("/"),
				{Group: 2},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(tc.in, tc.regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q, %q): unexpected error: %v", tc.in, tc.regex, err)
			}
			// A selection among indices has no `\N` spelling, so the
			// whole template must take the segments path even though
			// every index here sits below CH's `\9` ceiling.
			if got.Template != "" {
				t.Fatalf("ReplacementToCH(%q, %q): want the segments form, got template %q",
					tc.in, tc.regex, got.Template)
			}
			if len(got.Segments) != len(tc.want) {
				t.Fatalf("ReplacementToCH(%q, %q): got %d segments %+v, want %d %+v",
					tc.in, tc.regex, len(got.Segments), got.Segments, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if !got.Segments[i].Equal(tc.want[i]) {
					t.Fatalf("ReplacementToCH(%q, %q): segment %d: got %+v, want %+v",
						tc.in, tc.regex, i, got.Segments[i], tc.want[i])
				}
			}
		})
	}
}

// TestSharedCaptureNameSegmentsAgreeWithExpandString is the differential
// that makes the #1768 narrowing trustworthy. The claim being narrowed is
// a semantic equivalence — "first carrier with a non-empty capture" ==
// "first carrier that took part" — and only running both against real
// matches can establish it.
//
// evaluate here implements the SQL side EXACTLY as the emitter spells it:
// `arrayFirst(x -> x != ”, [g[i], g[j], …])`, over the same
// `extractGroups` values ClickHouse would see (Go's FindStringSubmatch
// reports a non-participating group as "" too, which is precisely the
// conflation the rejection used to be about). The oracle is Go's own
// ExpandString.
//
// The source strings are chosen so the first carrier does NOT participate
// in some of them — that is the case a naive "always take the first
// carrier" translation would get wrong, and it must be covered or the
// differential proves nothing about the ordering.
func TestSharedCaptureNameSegmentsAgreeWithExpandString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		repl  string
		srcs  []string
	}{
		{
			// "a" matches via carrier 1; "b" matches via carrier 2 with
			// carrier 1 absent — the skip-the-first-carrier case.
			name:  "two_carriers",
			regex: `(?P<dup>a)|(?P<dup>b)`,
			repl:  "$dup",
			srcs:  []string{"a", "b"},
		},
		{
			name:  "three_carriers",
			regex: `(?P<dup>a)|(?P<dup>b)|(?P<dup>c)`,
			repl:  "v=$dup",
			srcs:  []string{"a", "b", "c"},
		},
		{
			// Both carriers participate at once. Go takes the first, and
			// so must the array search — here the two captures differ, so
			// picking the wrong one is visible.
			name:  "both_carriers_participate",
			regex: `(?P<dup>a)(?P<dup>b)`,
			repl:  "$dup",
			srcs:  []string{"ab"},
		},
		{
			// Prefixed branches: the carrier is not at the start of its
			// branch, so its index and its participation come apart from
			// the branch order in the source text.
			name:  "prefixed_branches",
			regex: `x(?P<dup>a)|y(?P<dup>b)`,
			repl:  "pre-$dup-post",
			srcs:  []string{"xa", "yb"},
		},
		{
			// A non-carrier group sits between the carriers, so the
			// carrier indices are 1 and 3 rather than 1 and 2.
			name:  "carriers_straddling_an_unrelated_group",
			regex: `(?P<dup>a)|(?P<other>o)|(?P<dup>b)`,
			repl:  "$dup/$other",
			srcs:  []string{"a", "o", "b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(tc.repl, tc.regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q, %q): unexpected error: %v", tc.repl, tc.regex, err)
			}
			re := regexp.MustCompile("^" + tc.regex + "$")
			for _, src := range tc.srcs {
				match := re.FindStringSubmatchIndex(src)
				if match == nil {
					t.Fatalf("regex %q does not match %q — the oracle needs a match", tc.regex, src)
				}
				want := string(re.ExpandString(nil, tc.repl, src, match))
				groups := re.FindStringSubmatch(src)

				evaluated := evaluateSegments(got.Segments, src, groups)
				if evaluated != want {
					t.Fatalf("regex %q repl %q src %q: segments %+v evaluate to %q; "+
						"Go's ExpandString gives %q",
						tc.regex, tc.repl, src, got.Segments, evaluated, want)
				}
			}
		})
	}
}

// evaluateSegments renders a decomposition the way the emitted SQL does:
// a literal run contributes its text, the whole-match group contributes
// the source value, a plain reference contributes its `extractGroups`
// subscript, and a shared-name reference contributes the FIRST of its
// carriers whose subscript is non-empty — CH's
// `arrayFirst(x -> x != ”, […])`, whose no-match result is the empty
// string.
func evaluateSegments(segments []chplan.LabelReplaceSegment, src string, groups []string) string {
	var out string
	for _, seg := range segments {
		switch seg.Group {
		case chplan.NoCaptureGroup:
			out += seg.Literal
			continue
		case chplan.WholeMatchGroup:
			out += src
			continue
		}
		if len(seg.Fallbacks) == 0 {
			out += groups[seg.Group]
			continue
		}
		// arrayFirst(x -> x != '', [...]) — and the empty string when no
		// candidate qualifies, which adds nothing.
		for _, idx := range append([]int{seg.Group}, seg.Fallbacks...) {
			if groups[idx] != "" {
				out += groups[idx]
				break
			}
		}
	}
	return out
}

// TestReplacementSegmentsAgreeWithExpandString is the differential that
// makes the decomposition trustworthy: for each template, the segments
// evaluated against a real match must equal what Go's ExpandString — the
// engine reference Prometheus and reference Loki both run `label_replace`
// through — produces for the same input.
//
// A decomposition that named the wrong group, dropped a literal run, or
// reordered the pieces would still be a valid []LabelReplaceSegment; only
// evaluating it against the oracle catches that.
func TestReplacementSegmentsAgreeWithExpandString(t *testing.T) {
	t.Parallel()

	const src = "abcdefghij"
	for _, repl := range []string{
		"$10",
		"${10}",
		"pre-${10}-post",
		"$1/$10",
		"$0|$10",
		`$$a\b$10`,
		"$10$9$8",
		"${10}x${1}",
	} {
		t.Run(repl, func(t *testing.T) {
			t.Parallel()
			got, err := ReplacementToCH(repl, tenCaptureRegex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q): unexpected error: %v", repl, err)
			}

			re := regexp.MustCompile("^" + tenCaptureRegex + "$")
			match := re.FindStringSubmatchIndex(src)
			if match == nil {
				t.Fatalf("regex %q does not match %q — the oracle needs a match", tenCaptureRegex, src)
			}
			want := string(re.ExpandString(nil, repl, src, match))

			var evaluated string
			for _, seg := range got.Segments {
				switch seg.Group {
				case chplan.NoCaptureGroup:
					evaluated += seg.Literal
				case chplan.WholeMatchGroup:
					evaluated += src
				default:
					evaluated += re.FindStringSubmatch(src)[seg.Group]
				}
			}
			if evaluated != want {
				t.Fatalf("segments for %q evaluate to %q; Go's ExpandString gives %q",
					repl, evaluated, want)
			}
		})
	}
}
