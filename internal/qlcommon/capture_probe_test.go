package qlcommon

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestReplacementToCHProbesCarrierParticipation pins the rewrite itself:
// which span each probe wraps, and which group ends up answering for which
// carrier. The differentials prove the ANSWERS are right; this pins the
// MECHANISM, so a rewrite that happened to reach the same answers by
// probing a different span would still be visible in review.
//
// Every regex here was rejected outright before probes existed.
func TestReplacementToCHProbesCarrierParticipation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		regex      string
		wantProbed string
		wantSegs   []chplan.LabelReplaceSegment
	}{
		{
			// The first branch is nullable and has no positive probe. The
			// alternation is mandatory and its only sibling is non-empty, so
			// that sibling's empty probe proves this branch participated.
			name:       "mandatory_alternation_negative_sibling_probe",
			regex:      `(?:(?P<dup>a?)|(?P<dup>b))(?P<dup>c)`,
			wantProbed: `(?:(?P<dup>a?)|(?P<cerberusprobe0>(?P<dup>b)))(?P<dup>c)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 1, Fallbacks: []int{3, 4}, NegativeProbes: [][]int{{2}, nil, nil}},
			},
		},
		{
			// The witness issue #1956 was narrowed to. Carrier 1 is
			// nullable and sits under a quest, but the quest's body holds
			// a mandatory "x", so wrapping the body reports whether the
			// quest was entered — which is whether carrier 1 took part.
			name:       "quest_body_with_mandatory_text",
			regex:      `(?:x(?P<dup>a?))?(?P<dup>b)`,
			wantProbed: `(?:(?P<cerberusprobe0>x(?P<dup>a?)))?(?P<dup>b)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 2, Fallbacks: []int{3}, Probes: []int{1, 3}},
			},
		},
		{
			// A star rather than a quest. The probe reports the LAST pass,
			// and so does the carrier, so "took part at all" still agrees.
			name:       "star_body_with_mandatory_text",
			regex:      `(?:x(?P<dup>a?))*(?P<dup>b)`,
			wantProbed: `(?:(?P<cerberusprobe0>x(?P<dup>a?)))*(?P<dup>b)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 2, Fallbacks: []int{3}, Probes: []int{1, 3}},
			},
		},
		{
			// The mandatory text is an outer group's, not the carrier's
			// own parent's, so the walk has to climb past a nullable span
			// before it finds one that answers.
			name:       "mandatory_text_an_ancestor_out",
			regex:      `(?:(?:(?P<dup>a?))x)?(?P<dup>b)`,
			wantProbed: `(?:(?P<cerberusprobe0>(?:(?P<dup>a?))x))?(?P<dup>b)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 2, Fallbacks: []int{3}, Probes: []int{1, 3}},
			},
		},
		{
			// Only the BRANCH holding the carrier may be probed: the whole
			// alternation body would put a skippable node above it.
			name:       "alternation_branch_with_mandatory_text",
			regex:      `(?:x(?P<dup>a?)|y(?P<dup>b))?(?P<dup>c)`,
			wantProbed: `(?:(?P<cerberusprobe0>x(?P<dup>a?))|y(?P<dup>b))?(?P<dup>c)`,
			wantSegs: []chplan.LabelReplaceSegment{
				// Carrier 2 is non-nullable, so it answers for itself;
				// only carrier 1 needed a probe.
				{Group: 2, Fallbacks: []int{3, 4}, Probes: []int{1, 3, 4}},
			},
		},
		{
			// Carrier 1 gets a POSITIVE probe (so Probes is populated and
			// kept, not nilled), carrier 2 gets a NEGATIVE sibling probe, and
			// carrier 3 is a trailing unconditional carrier that answers for
			// itself. This chain is the one that pins the loop in
			// [captureGroups.expressibleCarriers] actually reaches every
			// searched carrier rather than stopping once it resolves
			// carrier 2's negative probe: carrier 3's own natural index in
			// Probes only lands there if its iteration runs at all.
			name:       "positive_then_negative_then_trailing_carrier",
			regex:      `(?:x(?P<dup>a?))?(?:(?P<dup>b?)|c)(?P<dup>d)`,
			wantProbed: `(?:(?P<cerberusprobe1>x(?P<dup>a?)))?(?:(?P<dup>b?)|(?P<cerberusprobe0>c))(?P<dup>d)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{
					Group:          2,
					Fallbacks:      []int{3, 5},
					Probes:         []int{1, 3, 5},
					NegativeProbes: [][]int{nil, {4}, nil},
				},
			},
		},
		{
			// Carrier 1 needs TWO negative-sibling probes (a three-way
			// alternation with two non-empty companion branches), which is
			// what makes the synthetic request numbering in
			// [planCaptureProbes] (`nextRequest--`) actually count down
			// across more than one request rather than being assigned once
			// and never observed again. The exact probe NAMES land in
			// [ProbedRegex] in descending-request order — cerberusprobe0
			// on the LAST-discovered sibling, cerberusprobe1 on the first —
			// so asserting the rewritten string pins the direction, not
			// just the final index values each name resolves to.
			name:  "carrier_with_two_negative_sibling_probes",
			regex: `(?:(?P<dup>a?)|(?P<dup>b)|(?P<dup>c))(?P<dup>d)`,
			wantProbed: `(?:(?P<dup>a?)|(?P<cerberusprobe1>(?P<dup>b))|(?P<cerberusprobe0>(?P<dup>c))` +
				`)(?P<dup>d)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 1, Fallbacks: []int{3, 5, 6}, NegativeProbes: [][]int{{2, 4}, nil, nil, nil}},
			},
		},
		{
			// Two independent carriers each get their own probe, and the
			// numbering they report is the REWRITTEN one throughout.
			name:       "two_carriers_two_probes",
			regex:      `(?:x(?P<dup>a?))?(?:y(?P<dup>b?))?(?P<dup>c)`,
			wantProbed: `(?:(?P<cerberusprobe0>x(?P<dup>a?)))?(?:(?P<cerberusprobe1>y(?P<dup>b?)))?(?P<dup>c)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 2, Fallbacks: []int{4, 5}, Probes: []int{1, 3, 5}},
			},
		},
		{
			// The ambiguity sits at carrier 2; carrier 1 is non-nullable
			// and keeps answering for itself.
			name:       "probe_after_a_clear_first_carrier",
			regex:      `(?P<dup>a)|(?:x(?P<dup>b?))?(?P<dup>c)`,
			wantProbed: `(?P<dup>a)|(?:(?P<cerberusprobe0>x(?P<dup>b?)))?(?P<dup>c)`,
			wantSegs: []chplan.LabelReplaceSegment{
				{Group: 1, Fallbacks: []int{3, 4}, Probes: []int{1, 2, 4}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReplacementToCH("$dup", tc.regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q): unexpected error: %v", tc.regex, err)
			}
			if got.ProbedRegex != tc.wantProbed {
				t.Fatalf("ReplacementToCH(%q).ProbedRegex = %q, want %q",
					tc.regex, got.ProbedRegex, tc.wantProbed)
			}
			if len(got.Segments) != len(tc.wantSegs) {
				t.Fatalf("ReplacementToCH(%q): got %d segments %+v, want %d",
					tc.regex, len(got.Segments), got.Segments, len(tc.wantSegs))
			}
			for i := range tc.wantSegs {
				if !got.Segments[i].Equal(tc.wantSegs[i]) {
					t.Fatalf("ReplacementToCH(%q): segment %d = %+v, want %+v",
						tc.regex, i, got.Segments[i], tc.wantSegs[i])
				}
			}
		})
	}
}

// TestNegativeSiblingSpans pins the narrow structural proof required before
// an empty sibling probe may stand in for a nullable carrier's participation.
func TestNegativeSiblingSpans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		src        string
		want       []string
		wantUsable bool
	}{
		{
			name:       "mandatory_fork_with_non_empty_sibling",
			src:        `(?:(?P<dup>a?)|(?P<dup>b))(?P<dup>c)`,
			want:       []string{`(?P<dup>b)`},
			wantUsable: true,
		},
		{
			name:       "innermost_fork_wins",
			src:        `(?:(?:(?P<dup>a?)|(?P<dup>b))|(?P<dup>c))(?P<dup>d)`,
			want:       []string{`(?P<dup>b)`},
			wantUsable: true,
		},
		{
			// The carrier's immediate parent is a plain non-capturing
			// wrapper, not the alternation itself, so finding the fork's
			// enclosing alternation has to walk up PAST that wrapper —
			// exercising the loop as a real multi-step walk rather than one
			// that stops on its first check.
			name:       "fork_ancestor_one_level_up",
			src:        `(?:(?:(?P<dup>a?))|(?P<dup>b))(?P<dup>c)`,
			want:       []string{`(?P<dup>b)`},
			wantUsable: true,
		},
		{
			name: "no_fork",
			src:  `(?P<dup>a?)(?P<dup>b)`,
		},
		{
			name: "nullable_sibling",
			src:  `(?:(?P<dup>a?)|(?P<dup>b?))(?P<dup>c)`,
		},
		{
			name: "repeated_fork",
			src:  `(?:(?P<dup>a?)|(?P<dup>b))*(?P<dup>c)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			groups, ok := scanSourceGroups(tc.src)
			if !ok {
				t.Fatalf("scanSourceGroups(%q) declined", tc.src)
			}
			at := groupIndexOf(groups, 1)
			spans, usable := negativeSiblingSpans(tc.src, groups, at)
			if usable != tc.wantUsable {
				t.Fatalf("negativeSiblingSpans(%q) usable = %v, want %v", tc.src, usable, tc.wantUsable)
			}
			if len(spans) != len(tc.want) {
				t.Fatalf("negativeSiblingSpans(%q) returned %d spans, want %d", tc.src, len(spans), len(tc.want))
			}
			for i, span := range spans {
				if got := tc.src[span.start:span.end]; got != tc.want[i] {
					t.Errorf("negativeSiblingSpans(%q) span %d = %q, want %q", tc.src, i, got, tc.want[i])
				}
			}
		})
	}
}

// TestProbeRewritePreservesMatching is the safety property the whole
// rewrite rests on, and the one a differential over ANSWERS cannot reach:
// the pattern cerberus sends to ClickHouse must accept exactly the strings
// the user's pattern accepts, and must capture exactly the same text into
// every group the user wrote.
//
// A rewrite that quietly changed the language would still agree with
// ExpandString on the inputs both happen to match, and would silently drop
// or admit rows on the ones they disagree about. So this compares the two
// compiled patterns SPAN BY SPAN over every corpus regex the rewrite fires
// on, rather than comparing the label value they produce.
func TestProbeRewritePreservesMatching(t *testing.T) {
	t.Parallel()

	inputs := stringsUpTo("abxy", 3)
	rewritten := 0

	for _, regex := range participationCorpus() {
		got, err := ReplacementToCH("$dup", regex)
		if err != nil || got.ProbedRegex == "" {
			continue
		}
		rewritten++

		original := regexp.MustCompile(anchorRegex(regex))
		probed := regexp.MustCompile(anchorRegex(got.ProbedRegex))
		positions := originalGroupPositions(t, original, probed, regex)

		for _, src := range inputs {
			before := original.FindStringSubmatchIndex(src)
			after := probed.FindStringSubmatchIndex(src)
			if (before == nil) != (after == nil) {
				t.Fatalf("regex %q rewritten to %q: %q matches %v before and %v after — "+
					"the rewrite changed which strings the pattern accepts",
					regex, got.ProbedRegex, src, before != nil, after != nil)
			}
			if before == nil {
				continue
			}
			for group, at := range positions {
				if before[2*group] != after[2*at] || before[2*group+1] != after[2*at+1] {
					t.Fatalf("regex %q rewritten to %q: on %q capture group %d spans "+
						"[%d,%d) before and [%d,%d) after — the rewrite moved a capture",
						regex, got.ProbedRegex, src, group,
						before[2*group], before[2*group+1], after[2*at], after[2*at+1])
				}
			}
		}
	}

	if rewritten == 0 {
		t.Fatal("no corpus regex was rewritten — this test would pass vacuously")
	}
	t.Logf("probe rewrite preserved matching on %d corpus regexes, %d inputs each",
		rewritten, len(inputs))
}

// originalGroupPositions maps each of the original pattern's capture-group
// indices to the index the same group holds in the rewritten one.
func originalGroupPositions(t *testing.T, original, probed *regexp.Regexp, regex string) []int {
	t.Helper()

	var positions []int
	for at, name := range probed.SubexpNames() {
		if strings.HasPrefix(name, probeNamePrefix) {
			continue
		}
		positions = append(positions, at)
	}
	if want := len(original.SubexpNames()); len(positions) != want {
		t.Fatalf("regex %q: rewritten pattern has %d non-probe groups, want %d",
			regex, len(positions), want)
	}
	return positions
}

// TestProbeSpanInvariantsHold checks the two properties every chosen probe
// span must have, directly, on every span the corpus makes the planner
// choose. The differentials show the answers come out right; this shows
// they come out right for the stated REASON, so a span that happened to
// work on the corpus but violated the invariant would be caught here
// rather than by whichever query first fell outside the corpus.
func TestProbeSpanInvariantsHold(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, regex := range participationCorpus() {
		anchored := anchorRegex(regex)
		groups, ok := scanSourceGroups(anchored)
		if !ok {
			continue
		}
		for _, g := range groups {
			if !g.capturing {
				continue
			}
			span, ok := probeSpanFor(anchored, groups, groupIndexOf(groups, g.capIndex))
			if !ok {
				continue
			}
			checked++
			body := anchored[span.start:span.end]
			parsed, err := syntax.Parse(body, syntax.Perl)
			if err != nil {
				t.Fatalf("regex %q: probe span %q does not parse: %v", regex, body, err)
			}
			if matchesEmpty(parsed) {
				t.Fatalf("regex %q: probe span %q can match the empty string, so its "+
					"emptiness cannot report participation", regex, body)
			}
			shape, known := captureShapes(body)[captureOrdinalIn(groups, span, g.open)]
			if !known || !shape.unconditional {
				t.Fatalf("regex %q: probe span %q does not pass through capture group %d on "+
					"every match, so entering it does not imply the carrier took part",
					regex, body, g.capIndex)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no probe span was chosen over the corpus — this test would pass vacuously")
	}
	t.Logf("probe span invariants held for %d chosen spans", checked)
}

func groupIndexOf(groups []sourceGroup, capIndex int) int {
	for i, g := range groups {
		if g.capturing && g.capIndex == capIndex {
			return i
		}
	}
	return 0
}

// TestWeakenedProbeChoiceIsCaughtByTheCorpus is the adversarial pass over
// this file's own reasoning. Each subtest reproduces the planner with one
// of its two span tests removed, and requires the corpus to produce a
// concrete disagreement with Go — so the corpus is known to be sensitive
// to each test individually, rather than merely passing while both hold.
//
// Without this, a corpus that happened to contain no regex distinguishing
// "the nearest ancestor" from "the nearest NON-NULLABLE ancestor" would
// give the same green, and the narrower reasoning would be unsupported.
func TestWeakenedProbeChoiceIsCaughtByTheCorpus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// weakened picks a probe span with one invariant dropped.
		weakened func(src string, groups []sourceGroup, at int) (sourceSpan, bool)
	}{
		{
			// Probing the nearest ancestor whatever it is: a nullable span
			// reports empty even when it was entered, so a carrier that
			// took part reads as absent.
			name: "ignores_that_the_span_must_not_match_empty",
			weakened: func(_ string, groups []sourceGroup, at int) (sourceSpan, bool) {
				spans := candidateSpans(groups, at)
				if len(spans) == 0 {
					return sourceSpan{}, false
				}
				return spans[0], true
			},
		},
		{
			// Probing the nearest non-nullable ancestor without requiring
			// every match of it to reach the carrier: the span can then be
			// entered by a match that skips the carrier, so a carrier that
			// took no part reads as present.
			name: "ignores_that_every_match_must_reach_the_carrier",
			weakened: func(src string, groups []sourceGroup, at int) (sourceSpan, bool) {
				for _, span := range candidateSpans(groups, at) {
					parsed, err := syntax.Parse(src[span.start:span.end], syntax.Perl)
					if err != nil {
						return sourceSpan{}, false
					}
					if !matchesEmpty(parsed) {
						return span, true
					}
				}
				return sourceSpan{}, false
			},
		},
	}

	inputs := stringsUpTo("abxy", 3)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, regex := range participationCorpus() {
				if witness := weakenedProbeDisagreement(regex, tc.weakened, inputs); witness != "" {
					t.Logf("corpus catches the weakening: %s", witness)
					return
				}
			}
			t.Error("no corpus regex disagreed with Go once this span test was dropped — " +
				"the corpus cannot tell the planner's reasoning from a weaker one, so the " +
				"differentials are not evidence for it")
		})
	}
}

// weakenedProbeDisagreement builds the plan a weakened span choice would
// produce and returns the first input on which it answers differently from
// Go's ExpandString, or "" when it never does.
func weakenedProbeDisagreement(
	regex string,
	spanFor func(src string, groups []sourceGroup, at int) (sourceSpan, bool),
	inputs []string,
) string {
	anchored := anchorRegex(regex)
	original, err := regexp.Compile(anchored)
	if err != nil {
		return ""
	}
	groups, ok := scanSourceGroups(anchored)
	if !ok {
		return ""
	}

	shapes := captureShapes(anchored)
	g := captureGroups{count: original.NumSubexp(), shapes: shapes, byName: map[string][]int{}}
	for i, name := range original.SubexpNames() {
		if name != "" {
			g.byName[name] = append(g.byName[name], i)
		}
	}
	carriers := g.byName["dup"]
	if len(carriers) < 2 {
		return ""
	}

	spans := map[int]sourceSpan{}
	for _, carrier := range g.unclearedCarriers(carriers) {
		span, ok := spanFor(anchored, groups, groupIndexOf(groups, carrier))
		if !ok {
			return ""
		}
		spans[carrier] = span
	}
	if len(spans) == 0 {
		return ""
	}
	probed, probeNames, ok := insertProbes(anchored, spans)
	if !ok {
		return ""
	}
	plan, ok := readBackPlan(original, probed, probeNames, nil)
	if !ok {
		return ""
	}
	g.probe = plan
	searched, probes, _, _, ok := g.expressibleCarriers(carriers)
	if !ok {
		return ""
	}

	compiled, err := regexp.Compile(plan.regex)
	if err != nil {
		return ""
	}
	segment := chplan.LabelReplaceSegment{Group: searched[0], Fallbacks: searched[1:], Probes: probes}
	for _, src := range inputs {
		match := original.FindStringSubmatchIndex(src)
		if match == nil {
			continue
		}
		want := string(original.ExpandString(nil, "$dup", src, match))
		got := evaluateSegments([]chplan.LabelReplaceSegment{segment}, src, compiled.FindStringSubmatch(src))
		if got != want {
			return "regex " + regex + " rewritten to " + plan.regex +
				" answers " + got + " on input " + src + " where Go answers " + want
		}
	}
	return ""
}

// TestProbeRewriteDeclinesInlineFlagSettings pins the one construct that
// would let the rewrite change what the user's pattern MEANS rather than
// merely how its groups are numbered.
//
// A bare `(?i)` applies to the end of the group enclosing it, crossing
// alternation branches. Wrapping a branch in a probe group confines it to
// that branch, so a pattern that matched "B" through the leaked flag stops
// matching it. The first half of this test demonstrates that hazard
// directly — it is the reason for the decline, stated as something that
// runs — and the second half pins that cerberus keeps its rejection rather
// than emitting the rewrite.
func TestProbeRewriteDeclinesInlineFlagSettings(t *testing.T) {
	t.Parallel()

	t.Run("wrapping_a_branch_would_confine_the_setting", func(t *testing.T) {
		t.Parallel()

		leaked := regexp.MustCompile(anchorRegex(`(?:(?i)a(?P<dup>x?)|b)`))
		confined := regexp.MustCompile(anchorRegex(`(?:(?P<probe>(?i)a(?P<dup>x?))|b)`))

		if !leaked.MatchString("B") {
			t.Fatal(`"B" does not match the un-wrapped pattern — the premise of this test is that ` +
				`the inline (?i) reaches the second branch`)
		}
		if confined.MatchString("B") {
			t.Fatal(`"B" still matches once the first branch is wrapped — the hazard this ` +
				`decline exists for has gone away, and the decline should be revisited`)
		}
	})

	t.Run("cerberus_keeps_the_rejection", func(t *testing.T) {
		t.Parallel()

		for _, regex := range []string{
			`(?:(?i)x(?P<dup>a?)|y)(?P<dup>b)`,
			`(?i)(?:x(?P<dup>a?))?(?P<dup>b)`,
			`(?:x(?P<dup>a?)(?i))?(?P<dup>b)`,
		} {
			got, err := ReplacementToCH("$dup", regex)
			if err == nil {
				t.Errorf("ReplacementToCH(%q) produced %+v; want the rejection kept, because "+
					"no rewrite of a pattern carrying a bare inline flag setting is known to "+
					"preserve its language", regex, got)
			}
		}
	})
}

// TestReplacementToCHRewritesOnlyWhenTheRewriteIsTheReason pins that a
// rewrite never happens where it buys nothing — which is a correctness
// requirement, not tidiness.
//
// A rewrite renumbers every capture group. The `replaceRegexpOne`
// substitution string is applied against the regex `match(...)` ran, which
// is the ORIGINAL one, so a template built from rewritten indices points
// at the wrong groups: for `(?:x(?P<dup>a?))?(?P<dup>b)` the reference
// `$1` would render as `\2` and substitute the text of `(?P<dup>b)`. That
// is a silently wrong label value on the wire — the failure mode issue
// #1490 reported — rather than a loud one.
//
// Two guarantees keep it out of reach, and both are asserted here: a
// template that resolves without a rewrite never gets one, and a
// decomposition that DID get one never takes the substitution-string form.
func TestReplacementToCHRewritesOnlyWhenTheRewriteIsTheReason(t *testing.T) {
	t.Parallel()

	// The regex carries a shared name whose carriers would need probes,
	// so a resolution that planned them eagerly would rewrite for every
	// template below — none of which reference that name.
	const regex = `(?:x(?P<dup>a?))?(?P<dup>b)`

	t.Run("a_reference_that_needs_no_probe_keeps_the_original_numbering", func(t *testing.T) {
		t.Parallel()

		inputs := []string{"xab", "b", "xb"}
		oracle := regexp.MustCompile(anchorRegex(regex))

		for _, repl := range []string{"$1", "$2", "v=$1", "$1-$2", "plain"} {
			got, err := ReplacementToCH(repl, regex)
			if err != nil {
				t.Fatalf("ReplacementToCH(%q, %q): unexpected error: %v", repl, regex, err)
			}
			if got.ProbedRegex != "" {
				t.Errorf("ReplacementToCH(%q, %q) rewrote the regex to %q; the template "+
					"references no shared name, so the rewrite renumbers groups for nothing",
					repl, regex, got.ProbedRegex)
			}
			for _, src := range inputs {
				match := oracle.FindStringSubmatchIndex(src)
				if match == nil {
					continue
				}
				want := string(oracle.ExpandString(nil, repl, src, match))
				if evaluated := evaluateReplacement(got, regex, src); evaluated != want {
					t.Errorf("ReplacementToCH(%q, %q) on %q evaluates to %q; Go's "+
						"ExpandString gives %q", repl, regex, src, evaluated, want)
				}
			}
		}
	})

	t.Run("a_rewritten_decomposition_never_takes_the_template_form", func(t *testing.T) {
		t.Parallel()

		got, err := ReplacementToCH("$dup", regex)
		if err != nil {
			t.Fatalf("ReplacementToCH(%q): unexpected error: %v", regex, err)
		}
		if got.ProbedRegex == "" {
			t.Fatal("the shared-name reference did not produce a rewrite, so this test no " +
				"longer covers what it claims to")
		}
		if got.Template != "" {
			t.Errorf("ReplacementToCH(%q) returned the substitution-string form %q alongside a "+
				"rewrite; that string is applied against the ORIGINAL regex, so its backrefs "+
				"would point at the wrong groups", regex, got.Template)
		}
	})
}
