package qlcommon

import (
	"regexp"
	"testing"
)

// TestAnchorRegexMatchesReferenceAnchoring pins anchorRegex against
// reference Prometheus's own anchoring form
// (`promql/functions.go`: `"^(?s:" + regexStr + ")$"`) for the two
// independent behaviour differences a bare `"^" + regex + "$"` gets
// wrong (#1951):
//
//   - alternation binds looser than anchoring, so `^a|b$` parses as
//     `(^a)|(b$)` rather than `^(a|b)$` — the anchors bind only to the
//     first/last arm of a top-level alternation unless a group gives
//     them one thing to bind around;
//   - without the `s` flag, `.` does not match a newline.
//
// Each case's `want` is reference Prometheus's verdict (checked here
// directly against Go's `regexp` package, the engine both cerberus and
// reference Prometheus run these regexes through), and the test also
// confirms the OLD bare-anchor form disagrees with it — so every row
// here would have failed before the fix and pins the corrected
// behaviour now.
func TestAnchorRegexMatchesReferenceAnchoring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		src   string
		want  bool
	}{
		{"alternation_first_arm_no_longer_escapes_anchor", "a|b", "ax", false},
		{"alternation_second_arm_no_longer_escapes_anchor", "a|b", "xb", false},
		{"dot_matches_newline_under_s_flag", ".*", "a\nb", true},
		{"alternation_prefix_no_longer_escapes_anchor", "foo|bar", "foobaz", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			anchored := anchorRegex(tc.regex)
			re, err := regexp.Compile(anchored)
			if err != nil {
				t.Fatalf("regexp.Compile(anchorRegex(%q)) = %q: unexpected error: %v",
					tc.regex, anchored, err)
			}
			if got := re.MatchString(tc.src); got != tc.want {
				t.Errorf("anchorRegex(%q) = %q; MatchString(%q) = %v, want %v (reference Prometheus verdict)",
					tc.regex, anchored, tc.src, got, tc.want)
			}

			// The old, buggy anchoring must disagree with the reference on
			// this exact case — otherwise the row does not actually pin
			// the fix.
			naive := "^" + tc.regex + "$"
			naiveRe := regexp.MustCompile(naive)
			if got := naiveRe.MatchString(tc.src); got == tc.want {
				t.Errorf("case %q does not exercise the bug: naive anchoring %q agrees with the "+
					"reference verdict %v for src %q — this row pins nothing", tc.name, naive, tc.want, tc.src)
			}
		})
	}
}

// TestAnchorRegexIsNonCapturing pins that anchorRegex's `(?s:...)`
// wrapper is non-capturing: since it introduces no new capture group,
// wrapping the whole regex must not shift any existing group's index,
// which is what [newCaptureGroups] relies on to keep
// [captureGroups.byName] and [captureGroups.nullable] aligned with the
// `Regexp.SubexpNames` indices the emitter's `extractGroups` subscripts
// address.
func TestAnchorRegexIsNonCapturing(t *testing.T) {
	t.Parallel()

	const regex = `(a)(b)|(c)`
	unanchored := regexp.MustCompile(regex)
	anchored := regexp.MustCompile(anchorRegex(regex))

	if got, want := anchored.NumSubexp(), unanchored.NumSubexp(); got != want {
		t.Fatalf("anchorRegex(%q): NumSubexp = %d, want %d (unchanged from the unanchored regex)",
			regex, got, want)
	}

	unanchoredNames := unanchored.SubexpNames()
	anchoredNames := anchored.SubexpNames()
	if len(anchoredNames) != len(unanchoredNames) {
		t.Fatalf("anchorRegex(%q): SubexpNames = %v, want %v", regex, anchoredNames, unanchoredNames)
	}
	for i := range unanchoredNames {
		if anchoredNames[i] != unanchoredNames[i] {
			t.Errorf("anchorRegex(%q): SubexpNames[%d] = %q, want %q — the (?s:...) wrapper must not "+
				"shift capture-group indices", regex, i, anchoredNames[i], unanchoredNames[i])
		}
	}
}

// TestNewCaptureGroupsIndicesSurviveAnchorWrap is the same guarantee as
// TestAnchorRegexIsNonCapturing, but driven through newCaptureGroups
// itself — the real entry point [captureGroups.resolve] and the
// shared-capture-group-name nullability logic (#1768/#1956) depend on —
// rather than through regexp.Compile directly.
func TestNewCaptureGroupsIndicesSurviveAnchorWrap(t *testing.T) {
	t.Parallel()

	g := newCaptureGroups(`(a)(b)|(c)`)

	const wantCount = 3
	if g.count != wantCount {
		t.Fatalf("newCaptureGroups: count = %d, want %d", g.count, wantCount)
	}

	// None of the three groups is named, so byName must stay empty — a
	// shift bug would not surface here directly, but a crash or a
	// spuriously populated byName would indicate the wrapper broke
	// SubexpNames parsing.
	if len(g.byName) != 0 {
		t.Fatalf("newCaptureGroups: byName = %v, want empty (no named groups in the input regex)", g.byName)
	}

	// Group 3 (`(c)`) is a single-rune literal — non-nullable; group 1
	// (`(a)`) likewise. Both must be classified by index, not shifted by
	// the anchor wrapper.
	for _, idx := range []int{1, 2, 3} {
		nullable, ok := g.nullable[idx]
		if !ok {
			t.Fatalf("newCaptureGroups: nullable[%d] missing; got %v", idx, g.nullable)
		}
		if nullable {
			t.Errorf("newCaptureGroups: nullable[%d] = true, want false — %q is a single-rune literal",
				idx, "(a)(b)|(c)")
		}
	}
}
