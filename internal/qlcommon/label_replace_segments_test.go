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
