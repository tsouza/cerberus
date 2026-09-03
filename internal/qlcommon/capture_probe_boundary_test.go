package qlcommon

import (
	"regexp"
	"strings"
	"testing"
)

// scannerSyntaxCorpus is a regex source per branch of the source scanner:
// each escape form, each character-class edge, each group spelling, each
// quantifier, and nestings of them. It is the input to
// [TestEveryScannedSpanSurvivesProbeInsertion], which is what turns these
// from strings into assertions about byte offsets.
var scannerSyntaxCorpus = []string{
	`(a)`,
	`(?:a)`,
	`(?P<n>a)`,
	`(?<n>a)`,
	`x(a)y`,
	`(a)(b)(c)`,
	`((a)(b))`,
	`(?:(a)|(b))`,
	`(a|b)`,
	`(a|b|c)`,
	`((?:a|b)c)`,
	`(?:x(a)|y(b)|z(c))`,

	// Escapes: the next byte is covered whatever it is, so none of these
	// parentheses or brackets open anything.
	`(\(a\))`,
	`(\[a\])`,
	`(\\)`,
	`(\\\\)`,
	`(a\|b)`,
	`(\p{L})`,
	`(\pL)`,
	`(\d\w\s)`,

	// Character classes: a leading `]` is a member, `^` may precede it, a
	// POSIX name carries its own `]`, and an escape inside covers a byte.
	`([()])`,
	`([]()])`,
	`([^]()])`,
	`([\]()])`,
	`([[:alpha:]])`,
	`([[:^alpha:]])`,
	`([[:alpha:]()])`,
	`x[[:digit:]](a)`,
	`[[:alpha:]][[:digit:]](a)`,
	`([a[b])`,
	`([-a])`,
	`([a-])`,
	`([a-z])`,
	`([^a-z])`,
	`x([|])y(a)`,

	// Quantifiers after a group must not be swallowed into its span.
	`(?:(a))?`,
	`(?:(a))*`,
	`(?:(a))+`,
	`(?:(a)){2,3}`,
	`(a+?)`,
	`(a*?b)`,
	`(a{2,3})`,

	// `\Q…\E` quotes its contents, so parentheses inside it are literal
	// text and must not be counted as groups — including an unterminated
	// run, which quotes the rest of the pattern.
	`\Q(x)\E(a)`,
	`\Q(?P<n>q)\E(a)`,
	`\Q(\E(a)`,
	`\Q[\E(a)`,
	`(a)\Q(y`,
	`\Q\E(a)`,

	// Scoped flag groups are self-contained and stay scannable.
	`((?i:a))`,
	`(?i:(a))`,
	`(?is:(a)|b)`,
	`(?i-s:(a))`,
}

// TestEveryScannedSpanSurvivesProbeInsertion is the scanner's real
// assertion. Every other test of it checks a span against a substring the
// test itself wrote down; this one checks the span against the Go regexp
// COMPILER, by doing to it exactly what the rewrite does — inserting a
// capture group at its two ends — and requiring the result to be a
// pattern that still compiles, still matches the same strings, and still
// captures the same text into every group the source wrote.
//
// That is what makes an off-by-one visible. A boundary that is one byte
// out lands a parenthesis inside a character class, between a group and
// its quantifier, or in the middle of an escape — and the pattern either
// stops compiling or quietly changes meaning, both of which fail here.
// Asserting the spans as substrings cannot catch the second.
func TestEveryScannedSpanSurvivesProbeInsertion(t *testing.T) {
	t.Parallel()

	inputs := stringsUpTo("abxyz", 3)
	spans := 0

	for _, source := range scannerSyntaxCorpus {
		original, err := regexp.Compile(source)
		if err != nil {
			t.Fatalf("corpus regex %q does not compile: %v", source, err)
		}
		groups, ok := scanSourceGroups(source)
		if !ok {
			t.Fatalf("scanSourceGroups(%q) declined; every corpus entry is meant to scan", source)
		}
		if got, want := countCapturing(groups), original.NumSubexp(); got != want {
			t.Errorf("scanSourceGroups(%q) found %d capturing group(s), the compiler reports %d",
				source, got, want)
		}

		for at, g := range groups {
			if at == wholePatternGroup {
				continue
			}
			spans++
			probed := source[:g.bodyStart] + "(?P<" + probeNamePrefix + "0>" +
				source[g.bodyStart:g.bodyEnd] + ")" + source[g.bodyEnd:]

			rewritten, err := regexp.Compile(probed)
			if err != nil {
				t.Errorf("scanSourceGroups(%q) group %d spans [%d,%d); wrapping it gives %q, "+
					"which does not compile: %v", source, at, g.bodyStart, g.bodyEnd, probed, err)
				continue
			}
			assertSameLanguage(t, source, probed, original, rewritten, inputs)
		}
	}

	if spans == 0 {
		t.Fatal("no span was checked — this test would pass vacuously")
	}
	t.Logf("probe insertion validated %d scanned spans over %d sources",
		spans, len(scannerSyntaxCorpus))
}

// assertSameLanguage requires two patterns to accept the same strings and
// to capture the same text into every group of the first.
func assertSameLanguage(t *testing.T, source, probed string, original, rewritten *regexp.Regexp, inputs []string) {
	t.Helper()

	var positions []int
	for at, name := range rewritten.SubexpNames() {
		if !strings.HasPrefix(name, probeNamePrefix) {
			positions = append(positions, at)
		}
	}
	if len(positions) != len(original.SubexpNames()) {
		t.Errorf("wrapping %q into %q changed the capture-group count", source, probed)
		return
	}

	for _, src := range inputs {
		before := original.FindStringSubmatchIndex(src)
		after := rewritten.FindStringSubmatchIndex(src)
		if (before == nil) != (after == nil) {
			t.Errorf("wrapping %q into %q changed whether %q matches", source, probed, src)
			return
		}
		if before == nil {
			continue
		}
		for group, mapped := range positions {
			if before[2*group] != after[2*mapped] || before[2*group+1] != after[2*mapped+1] {
				t.Errorf("wrapping %q into %q moved capture group %d on input %q",
					source, probed, group, src)
				return
			}
		}
	}
}

func countCapturing(groups []sourceGroup) int {
	n := 0
	for _, g := range groups {
		if g.capturing {
			n++
		}
	}
	return n
}

// TestScanSourceGroupsWholePatternGroup pins the virtual group standing
// for the entire pattern. It is what gives a group at the top level a
// parent to walk up to, so a walk that skipped it would silently stop
// looking one level early.
func TestScanSourceGroupsWholePatternGroup(t *testing.T) {
	t.Parallel()

	const source = `x(?P<dup>a?)y`
	groups, ok := scanSourceGroups(source)
	if !ok {
		t.Fatalf("scanSourceGroups(%q) declined", source)
	}
	root := groups[wholePatternGroup]
	if root.open != -1 {
		t.Errorf("whole-pattern group opens at %d, want -1 — it stands for a group that is "+
			"not written in the source", root.open)
	}
	if root.bodyStart != 0 || root.bodyEnd != len(source) {
		t.Errorf("whole-pattern group spans [%d,%d), want [0,%d)",
			root.bodyStart, root.bodyEnd, len(source))
	}
	if root.parent != -1 {
		t.Errorf("whole-pattern group has parent %d, want -1", root.parent)
	}
	if root.capturing {
		t.Error("whole-pattern group reports as capturing; it corresponds to no capture index")
	}
}

// TestBranchContainingSplitsEveryBranch pins the branch boundaries a probe
// is placed at. The first branch starts at the body, the last ends at it,
// and a middle branch is bounded by the two bars around it — each of which
// is an offset the rewrite inserts a parenthesis at.
func TestBranchContainingSplitsEveryBranch(t *testing.T) {
	t.Parallel()

	const source = `(?:aa|bb|cc)`
	groups, ok := scanSourceGroups(source)
	if !ok {
		t.Fatalf("scanSourceGroups(%q) declined", source)
	}
	g := groups[1]

	cases := []struct {
		name   string
		offset int
		want   string
	}{
		{"first_branch", strings.Index(source, "aa"), "aa"},
		{"middle_branch", strings.Index(source, "bb"), "bb"},
		{"last_branch", strings.Index(source, "cc"), "cc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			span, split := branchContaining(g, tc.offset)
			if !split {
				t.Fatalf("branchContaining reported no alternation for %q", source)
			}
			if got := source[span.start:span.end]; got != tc.want {
				t.Errorf("branchContaining(offset %d) = %q, want %q", tc.offset, got, tc.want)
			}
		})
	}

	t.Run("no_alternation", func(t *testing.T) {
		t.Parallel()
		plain, ok := scanSourceGroups(`(?:ab)`)
		if !ok {
			t.Fatal("scanSourceGroups declined")
		}
		if _, split := branchContaining(plain[1], 3); split {
			t.Error("branchContaining reported an alternation in a body that has none, which " +
				"would make the rewrite probe a branch boundary that does not exist")
		}
	})
}

// TestCandidateSpansReachTheWholePattern pins that the ladder walks all
// the way to the virtual whole-pattern group. A walk that stopped at the
// last WRITTEN group would never offer the outermost span, so a carrier
// whose only non-nullable ancestor is the pattern itself would be refused.
func TestCandidateSpansReachTheWholePattern(t *testing.T) {
	t.Parallel()

	const source = `x(?P<dup>a?)`
	groups, ok := scanSourceGroups(source)
	if !ok {
		t.Fatalf("scanSourceGroups(%q) declined", source)
	}
	at := 0
	for i, g := range groups {
		if g.capturing {
			at = i
		}
	}
	spans := candidateSpans(groups, at)
	if len(spans) == 0 {
		t.Fatal("candidateSpans returned nothing for a top-level group; its parent is the " +
			"whole-pattern group, which the walk must still visit")
	}
	last := spans[len(spans)-1]
	if last.start != 0 || last.end != len(source) {
		t.Errorf("outermost candidate spans [%d,%d), want the whole pattern [0,%d)",
			last.start, last.end, len(source))
	}
}

// TestProbedIndexTranslatesOnlyRealGroups pins the index translation's
// edges. Index 0 is the whole match and must translate like any other;
// anything past the table is not a group the rewrite knows about and must
// come back untouched, which is what keeps an out-of-range reference
// resolving to nothing rather than to a group it never named.
func TestProbedIndexTranslatesOnlyRealGroups(t *testing.T) {
	t.Parallel()

	// The whole-match entry is deliberately not 0: a table that mapped it
	// to itself would make the range check's lower bound unobservable,
	// since returning the argument and returning the entry would agree.
	plan := captureProbePlan{toProbed: []int{5, 2, 3}}

	cases := []struct {
		name     string
		original int
		want     int
	}{
		{"whole_match", 0, 5},
		{"first_group", 1, 2},
		{"last_group", 2, 3},
		{"one_past_the_table", 3, 3},
		{"far_past_the_table", 99, 99},
		{"negative", -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := plan.probedIndex(tc.original); got != tc.want {
				t.Errorf("probedIndex(%d) = %d, want %d", tc.original, got, tc.want)
			}
		})
	}

	t.Run("the_zero_plan_is_the_identity", func(t *testing.T) {
		t.Parallel()
		var none captureProbePlan
		for _, original := range []int{0, 1, 9} {
			if got := none.probedIndex(original); got != original {
				t.Errorf("probedIndex(%d) on a plan with no rewrite = %d, want %d",
					original, got, original)
			}
		}
	})
}

// TestReadBackPlanAcceptsCarrierZero pins readBackPlan's carrier-index
// boundary: index 0 (the whole match) is a real carrier like any other,
// distinct from the NEGATIVE indices [planCaptureProbes] mints for a
// synthetic negative-sibling-probe request. A carrier filter that treated
// 0 as one of those synthetic requests (rather than the boundary sitting
// where the doc comment on [captureProbePlan.toProbed] says it does — "0
// maps to itself") would silently drop the whole-match probe from
// [captureProbePlan.probeOf] instead of erroring, so this exercises
// readBackPlan directly rather than through a query that could never name
// group 0 as a carrier (no attribute syntax spells `$0`'s OWN name).
func TestReadBackPlanAcceptsCarrierZero(t *testing.T) {
	t.Parallel()

	original, err := regexp.Compile(`a`)
	if err != nil {
		t.Fatalf("regexp.Compile: %v", err)
	}
	const probed = `(?P<` + probeNamePrefix + `0>a)`
	probeNames := map[int]string{0: probeNamePrefix + "0"}

	plan, ok := readBackPlan(original, probed, probeNames, nil)
	if !ok {
		t.Fatal("readBackPlan declined a whole-match carrier, which is index 0 like any other")
	}
	at, planned := plan.probeOf[0]
	if !planned {
		t.Fatal("readBackPlan planned no probe for carrier 0 — a carrier filter that also caught " +
			"index 0 would silently drop it here instead")
	}
	if got, want := compileNames(t, probed)[at], probeNamePrefix+"0"; got != want {
		t.Errorf("readBackPlan(carrier 0) probe index %d names %q, want %q", at, got, want)
	}
}

// negativeCarrierWalks is how many times the walk below is repeated. The
// defect it pins is a control-flow one over a Go map, whose iteration order
// is randomised per range statement, so a single call observes the wrong
// order only about half the time. Each repeat is an independent draw, which
// puts a false pass below 2^-64 — small enough that a green run is evidence
// rather than luck, and cheap enough (one Compile per repeat) to sit in the
// unit lane.
const negativeCarrierWalks = 64

// TestReadBackPlanSkipsNegativeCarriersWithoutStopping pins that the
// synthetic negative-sibling requests planCaptureProbes mints — the
// descending -1, -2, … keys counted down from
// capture_probe.go:`nextRequest := -1` — are SKIPPED by the positive-probe
// walk rather than ending it.
//
// probeNames is keyed by carrier and mixes both kinds, so a walk that
// stopped at the first negative key would drop every positive carrier the
// map happened to order after it. Nothing about that is deterministic: the
// resulting plan would be correct or silently missing a probe depending on
// Go's per-range map seed, which is why the assertion is a repeated one.
func TestReadBackPlanSkipsNegativeCarriersWithoutStopping(t *testing.T) {
	t.Parallel()

	original, err := regexp.Compile(`(?P<orig>x)`)
	if err != nil {
		t.Fatalf("regexp.Compile: %v", err)
	}
	positive := probeNamePrefix + "0"
	negative := probeNamePrefix + "1"
	probed := `(?P<` + positive + `>(?P<orig>x))(?P<` + negative + `>y)?`
	// Carrier 1 is a real capture group; -1 is a synthetic negative-sibling
	// request, whose probe index reaches the plan through negativeNames.
	probeNames := map[int]string{1: positive, -1: negative}

	for i := range negativeCarrierWalks {
		plan, ok := readBackPlan(original, probed, probeNames, nil)
		if !ok {
			t.Fatalf("walk %d: readBackPlan declined a plan mixing positive and negative carriers", i)
		}
		if at, planned := plan.probeOf[1]; !planned || compileNames(t, probed)[at] != positive {
			t.Fatalf("walk %d: probeOf = %v, want carrier 1 mapped to the %q group — a walk that "+
				"stops at a negative carrier drops whatever the map ordered after it",
				i, plan.probeOf, positive)
		}
	}
}

// TestReadBackPlanRejectsInvalidRewriteMetadata pins each part of the
// compiler-backed proof that turns inserted source spans into capture indexes.
// Every case is a rewrite that looks plausible to one weaker check but cannot
// safely answer which original carrier participated.
func TestReadBackPlanRejectsInvalidRewriteMetadata(t *testing.T) {
	t.Parallel()

	original, err := regexp.Compile(`(?P<orig>x)`)
	if err != nil {
		t.Fatalf("regexp.Compile: %v", err)
	}
	missingProbe := probeNamePrefix + "missing"
	cases := []struct {
		name          string
		probed        string
		probeNames    map[int]string
		negativeNames map[int][]string
	}{
		{name: "rewrite_does_not_compile", probed: `(`},
		{name: "original_group_was_lost", probed: `x`},
		{name: "original_group_was_renamed", probed: `(?P<other>x)`},
		{
			name:       "positive_probe_name_is_absent",
			probed:     `(?P<orig>x)`,
			probeNames: map[int]string{1: missingProbe},
		},
		{
			name:          "negative_probe_name_is_absent",
			probed:        `(?P<orig>x)`,
			negativeNames: map[int][]string{1: {missingProbe}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := readBackPlan(original, tc.probed, tc.probeNames, tc.negativeNames); ok {
				t.Fatalf("readBackPlan accepted unsafe rewrite %q", tc.probed)
			}
		})
	}
}

// TestNegativeSiblingSpansDeclinesUnprovableShapes pins the three structural
// guards that keep an empty sibling probe from being treated as proof when the
// carrier itself cannot be classified, its branch can be skipped, or the
// source-group ancestry no longer reaches the alternation.
func TestNegativeSiblingSpansDeclinesUnprovableShapes(t *testing.T) {
	t.Parallel()

	assertDeclined := func(t *testing.T, src string, mutate func([]sourceGroup, int)) {
		t.Helper()
		groups, ok := scanSourceGroups(src)
		if !ok {
			t.Fatalf("scanSourceGroups(%q) declined", src)
		}
		at := groupIndexOf(groups, 1)
		if mutate != nil {
			mutate(groups, at)
		}
		if spans, usable := negativeSiblingSpans(src, groups, at); usable || len(spans) != 0 {
			t.Fatalf("negativeSiblingSpans(%q) = %v, %v; want no usable proof", src, spans, usable)
		}
	}

	t.Run("capture_shape_is_unknown", func(t *testing.T) {
		t.Parallel()
		assertDeclined(t, `(?P<dup>a?)|b`, func(groups []sourceGroup, at int) {
			groups[at].capIndex = 99
		})
	})
	t.Run("carrier_branch_is_optional", func(t *testing.T) {
		t.Parallel()
		assertDeclined(t, `(?P<dup>a)?|b`, nil)
	})
	t.Run("alternation_parent_is_missing", func(t *testing.T) {
		t.Parallel()
		assertDeclined(t, `(?P<dup>a?)|b`, func(groups []sourceGroup, at int) {
			groups[at].parent = -1
		})
	})
}

// TestCaptureGroupsRejectMissingShapes pins both callers' conservative answer
// for a carrier the parse-tree walk could not classify. It must be omitted
// from the statically cleared set and rejected by the executable search.
func TestCaptureGroupsRejectMissingShapes(t *testing.T) {
	t.Parallel()

	groups := captureGroups{shapes: map[int]captureShape{}}
	if cleared := groups.unclearedCarriers([]int{1}); len(cleared) != 0 {
		t.Fatalf("unclearedCarriers with no shape = %v, want no static clearance", cleared)
	}
	_, _, _, ambiguous, ok := groups.expressibleCarriers([]int{1})
	if ok || ambiguous != 1 {
		t.Fatalf("expressibleCarriers with no shape = ambiguous %d, ok %v; want 1, false", ambiguous, ok)
	}

	// An unclassified carrier must be SKIPPED, not treated as the end of
	// the search: the carriers after it are still classifiable and still
	// have to be reported. With a single unknown carrier the two readings
	// are indistinguishable — both end the loop — so the mixed list below
	// is what separates them.
	mixed := captureGroups{shapes: map[int]captureShape{
		2: {nullable: true},
		3: {nullable: true},
	}}
	cleared := mixed.unclearedCarriers([]int{1, 2, 3})
	if len(cleared) != 1 || cleared[0] != 2 {
		t.Fatalf("unclearedCarriers past an unknown carrier = %v, want [2]", cleared)
	}
}

// compileNames compiles pattern and returns its SubexpNames, failing the
// test on a compile error rather than every caller repeating the check.
func compileNames(t *testing.T, pattern string) []string {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("regexp.Compile(%q): %v", pattern, err)
	}
	return re.SubexpNames()
}

// TestStripAnchorsRequiresBothEnds pins that the wrapper is only removed
// when it is really there. A rewrite that lost its anchoring would be
// handed back to the caller as an unanchored pattern and then anchored
// again by the emitter, so the two halves are checked separately.
func TestStripAnchorsRequiresBothEnds(t *testing.T) {
	t.Parallel()

	if body, ok := stripAnchors(anchorRegex(`a(b)`)); !ok || body != `a(b)` {
		t.Errorf("stripAnchors(%q) = (%q, %v), want (%q, true)",
			anchorRegex(`a(b)`), body, ok, `a(b)`)
	}
	for _, name := range []string{`^(?s:a(b)`, `a(b))$`, `a(b)`, ``} {
		if _, ok := stripAnchors(name); ok {
			t.Errorf("stripAnchors(%q) succeeded; want a decline, because the wrapper it is "+
				"meant to remove is not present", name)
		}
	}
}

// TestInsertProbesRejectsSpansItCannotWrap pins the fail-closed arm of the
// insertion. The edits are applied in one forward pass, so a span reaching
// past the source, or one overlapping another without nesting inside it,
// would splice parentheses into text already written — producing a pattern
// that may still compile while bracketing something nobody chose.
func TestInsertProbesRejectsSpansItCannotWrap(t *testing.T) {
	t.Parallel()

	const source = `(?:xa)(?:yb)`

	t.Run("a_span_reaching_past_the_source", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 3, end: len(source) + 1},
		}); ok {
			t.Error("insertProbes accepted a span reaching past the source")
		}
	})

	t.Run("a_span_ending_exactly_at_the_source_end", func(t *testing.T) {
		t.Parallel()
		got, _, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 0, end: len(source)},
		})
		if !ok {
			t.Fatal("insertProbes declined a span covering the whole source, which is a " +
				"perfectly wrappable one")
		}
		if _, err := regexp.Compile(got); err != nil {
			t.Errorf("insertProbes produced %q, which does not compile: %v", got, err)
		}
	})

	t.Run("a_span_entirely_past_the_source", func(t *testing.T) {
		t.Parallel()
		// This one never opens at all, where the case above opens and
		// never closes — the two halves of the walk's postcondition.
		if _, _, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: len(source) + 4, end: len(source) + 8},
		}); ok {
			t.Error("insertProbes accepted a span lying entirely past the source")
		}
	})

	t.Run("spans_that_touch_without_overlapping", func(t *testing.T) {
		t.Parallel()
		// One span ending exactly where the next begins is disjoint, not
		// overlapping, and both can be wrapped. This is the input that
		// separates "ends before the other starts" from "ends at or
		// before" — the bound the nesting test turns on.
		got, names, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 0, end: 6},
			2: {start: 6, end: 12},
		})
		if !ok {
			t.Fatal("insertProbes declined two spans that merely touch; neither contains the " +
				"other and neither overlaps it, so both are wrappable")
		}
		if len(names) != 2 {
			t.Errorf("insertProbes named %d probes, want 2", len(names))
		}
		assertProbed(t, got, `(?P<`+probeNamePrefix+`0>(?:xa))(?P<`+probeNamePrefix+`1>(?:yb))`)
	})

	t.Run("a_span_nested_inside_another_sharing_an_end", func(t *testing.T) {
		t.Parallel()
		got, _, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 0, end: 12},
			2: {start: 6, end: 12},
		})
		if !ok {
			t.Fatal("insertProbes declined a span nested inside another that shares its end")
		}
		assertProbed(t, got, `(?P<`+probeNamePrefix+`0>(?:xa)(?P<`+probeNamePrefix+`1>(?:yb)))`)
	})

	t.Run("overlapping_spans", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 0, end: 8},
			2: {start: 4, end: 12},
		}); ok {
			t.Error("insertProbes accepted two spans that overlap without nesting; the " +
				"parentheses it would insert cannot bracket both")
		}
	})

	t.Run("nested_spans_sharing_a_start", func(t *testing.T) {
		t.Parallel()
		got, names, ok := insertProbes(source, map[int]sourceSpan{
			1: {start: 0, end: len(source)},
			2: {start: 0, end: 6},
		})
		if !ok {
			t.Fatal("insertProbes declined two properly nested spans that share a start; the " +
				"wider one simply opens first")
		}
		if len(names) != 2 {
			t.Errorf("insertProbes named %d probes, want 2", len(names))
		}
		// The WIDER span must open first. Both open at the same offset, so
		// only the delimiter order decides which probe wraps which span —
		// and a swap still compiles, still has two groups, and answers for
		// the wrong one.
		assertProbed(t, got, `(?P<`+probeNamePrefix+`0>(?P<`+probeNamePrefix+`1>(?:xa))(?:yb))`)
	})
}

// assertProbed compares a rewritten pattern against the exact expected
// text and checks it still compiles. The exact text matters: several
// orderings of the same delimiters produce a pattern that compiles and has
// the right group count while bracketing the wrong spans.
func assertProbed(t *testing.T, got, want string) {
	t.Helper()

	if got != want {
		t.Errorf("insertProbes produced\n  %s\nwant\n  %s", got, want)
	}
	if _, err := regexp.Compile(got); err != nil {
		t.Errorf("insertProbes produced %q, which does not compile: %v", got, err)
	}
}

// TestPlanCaptureProbesSkipsUnknownCarriers pins that a carrier index the
// scanner has no group for is stepped over rather than ending the plan.
// The needy list is a union across every shared name, so one unusable
// entry must not cost the others their rewrite.
func TestPlanCaptureProbesSkipsUnknownCarriers(t *testing.T) {
	t.Parallel()

	const regex = `(?:x(?P<dup>a?))?(?P<dup>b)`
	plan, ok := planCaptureProbes(regex, []int{99, 1})
	if !ok {
		t.Fatal("planCaptureProbes gave up after an index it could not place; the carriers " +
			"it CAN place must still be planned")
	}
	if _, planned := plan.probeOf[1]; !planned {
		t.Errorf("planCaptureProbes(%q) planned no probe for carrier 1: %+v", regex, plan)
	}
	if _, planned := plan.probeOf[99]; planned {
		t.Error("planCaptureProbes planned a probe for a carrier index that names no group")
	}
}

// TestCarriersNeedingProbesIgnoresUnsharedNames pins that a name only one
// group carries contributes nothing to the plan, and — because the walk is
// over every name in the pattern — that meeting such a name does not stop
// the walk before it reaches a shared one that does need probes.
func TestCarriersNeedingProbesIgnoresUnsharedNames(t *testing.T) {
	t.Parallel()

	// `alone` sorts before `dup`, so a walk that stopped at the first
	// unshared name would never reach the carriers that need probes.
	const regex = `(?P<alone>q?)(?:x(?P<dup>a?))?(?P<dup>b)`
	groups := newCaptureGroups(regex, withCaptureProbes)

	needy := groups.carriersNeedingProbes()
	if len(needy) == 0 {
		t.Fatal("carriersNeedingProbes found nothing; the shared name in this pattern has a " +
			"carrier no static fact clears")
	}
	for _, idx := range needy {
		if idx == 1 {
			t.Error("carriersNeedingProbes listed the group carrying the unshared name; a " +
				"name only one group carries is never ambiguous")
		}
	}
	if groups.probe.regex == "" {
		t.Error("no rewrite was planned, so the unshared name ended the walk early")
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// Four surviving mutants in capture_probe.go. Each was re-applied and the
// whole package suite re-run to confirm it survives rather than merely
// lacking a test.
//
// capture_probe.go:`for parent >= 0 && len(groups[parent].alternations) == 0`
// (CONDITIONALS_BOUNDARY, `parent >= 0` -> `parent > 0`). Index 0 is the virtual whole-pattern group, minted with
// parent -1 at
// regex_source_scan.go:`open: -1, bodyStart: 0, bodyEnd: len(src), parent: -1`,
// so the two forms differ only at
// parent == 0 with no alternations on that group: the original steps to -1
// and returns at the `parent < 0` check, while the mutant leaves the loop
// holding 0. It then reaches branchContaining(groups[0], …), which returns
// (sourceSpan{}, false) for a group with no alternations; the ignored bool
// leaves `branch` the zero span, captureShapes(src[0:0]) parses the empty
// pattern into an empty map, indexing it yields the zero captureShape whose
// unconditional field is false, and the next guard returns (nil, false) —
// the answer the original returned one branch earlier.
//
// capture_probe.go:`return ordered[i].start < ordered[j].start`
// (CONDITIONALS_BOUNDARY, the sort's `<` -> `<=`). The line is guarded by
// `if ordered[i].start != ordered[j].start`, and on unequal operands `<` and
// `<=` are the same function.
//
// capture_probe.go:`return ordered[i].end > ordered[j].end`
// (CONDITIONALS_BOUNDARY, `>` -> `>=`). Reached only when the two starts are equal.
// `ordered` is filled through the `named map[sourceSpan]string` seen-check
// above, so its elements are pairwise distinct spans; equal starts therefore
// force unequal ends, where `>` and `>=` agree. sort.SliceStable never calls
// less(i, i).
//
// capture_probe.go:`if offset < bar` (CONDITIONALS_BOUNDARY,
// branchContaining's `<` -> `<=`). `offset` is always a group's `open`, the byte
// index of a '(', and `bar` is the byte index of a '|'. One byte cannot be
// both, so offset == bar is unreachable and the two comparisons agree on
// every offset a caller can supply.

// anchorBreakoutWitnesses are the regexes that reach the two arms
// [planCaptureProbes] applies to its OWN rewrite after the scanner has
// picked a span — the read-back against the compiler, and the removal of
// the anchoring wrapper. Both arms are about the same event, which no
// other test in this package produces: a probe boundary landing at an END
// of the anchored pattern rather than strictly inside it.
//
// The event needs a regex whose own `)` closes the `^(?s:` wrapper early.
// Reference Prometheus anchors the same way, so `a)|b` is a pattern a
// user may legitimately write and cerberus must accept: after anchoring
// it is `^(?s:a)|b$`, a top-level alternation whose second arm sits
// BESIDE the wrapper instead of inside it. A carrier in that second arm
// has the whole arm — the wrapper's closing `)$` included — as its
// nearest probeable span, so the probe cerberus would insert brackets
// text the wrapper was supposed to bracket.
//
// The two witnesses differ by a trailing `\Q`, which quotes the rest of
// the pattern. Without it the probed pattern still compiles and it is the
// wrapper check that refuses the rewrite; with it the probe's own closing
// parenthesis falls inside the quoted run, so the pattern the rewrite
// produces does not compile at all and the read-back refuses it first.
var anchorBreakoutWitnesses = []struct {
	name  string
	regex string
	// probed is the pattern the rewrite would hand back if the arm under
	// test were not there. It is written out rather than derived so that
	// a change in which span gets probed is visible here as a diff.
	probed string
}{
	{
		name:   "probe_swallows_the_closing_wrapper",
		regex:  `a)|(?P<d>b?)(?P<d>c`,
		probed: `^(?s:a)|(?P<` + probeNamePrefix + `0>(?P<d>b?)(?P<d>c)$)`,
	},
	{
		name:   "quoted_run_swallows_the_probe",
		regex:  `a)|(?P<d>b?)(?P<d>c)\Q`,
		probed: `^(?s:a)|(?P<` + probeNamePrefix + `0>(?P<d>b?)(?P<d>c)\Q)$)`,
	},
}

// TestPlanCaptureProbesRefusesARewriteItCannotHandBack pins that a rewrite
// which does not survive its own verification is refused rather than
// trimmed, patched, or handed back anyway.
//
// The assertion is the CONTRACT, not the decline: whenever the planner
// reports success, the pattern it returns must be the caller's own
// spelling with capture groups added and nothing else — it re-anchors to
// a pattern that compiles, carries exactly the original's capture groups
// in the original's order once the probes are struck out, and accepts
// exactly the strings the user's own pattern accepts. Asserting only
// "it declines" would be satisfied by a planner that declined for the
// wrong reason.
//
// The two arms are not independent, and this test does not pretend they
// are. The wrapper check is the one the contract catches: with its early
// return removed the planner hands back the anchored text it was given,
// and this test fails. The read-back's early return is DOMINATED by it —
// removing that one leaves the plan at its zero value, whose empty regex
// the wrapper check refuses instead, so no input tells the two apart and
// the whole package passes with the mutation applied. What the read-back
// verification itself does is pinned by
// [TestReadBackPlanRejectsInvalidRewriteMetadata]; the second witness
// here is what drives a real rewrite into it.
func TestPlanCaptureProbesRefusesARewriteItCannotHandBack(t *testing.T) {
	t.Parallel()

	inputs := stringsUpTo("abc", 3)

	for _, w := range anchorBreakoutWitnesses {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()

			anchored := anchorRegex(w.regex)
			original, err := regexp.Compile(anchored)
			if err != nil {
				t.Fatalf("anchoring %q gives %q, which must compile for this witness to be "+
					"about the rewrite at all: %v", w.regex, anchored, err)
			}

			// The span the scanner picks is what makes this witness the
			// shape it claims to be: it reaches the END of the anchored
			// pattern, so the probe's own parenthesis lands past the
			// wrapper's. Pinning it here means a future change that made
			// the planner choose a different span would surface as this
			// test losing its subject rather than as a silent pass.
			groups, ok := scanSourceGroups(anchored)
			if !ok {
				t.Fatalf("the scanner declined %q outright, so this witness never reaches the "+
					"rewrite it is about", anchored)
			}
			span, ok := probeSpanFor(anchored, groups, groupIndexOf(groups, 1))
			if !ok || span.end != len(anchored) {
				t.Fatalf("probeSpanFor(%q) chose %v (ok=%v); this witness is only about the "+
					"rewrite arms when the span reaches the end of the anchored pattern, at %d",
					anchored, span, ok, len(anchored))
			}
			probed, _, ok := insertProbes(anchored, map[int]sourceSpan{1: span})
			if !ok || probed != w.probed {
				t.Fatalf("insertProbes produced (%q, ok=%v), want %q", probed, ok, w.probed)
			}

			// The planner's own entry, driven with the carriers the
			// public path asks for rather than a hand-written list.
			need := newCaptureGroups(w.regex, withoutCaptureProbes).carriersNeedingProbes()
			if len(need) == 0 {
				t.Fatalf("no carrier of %q needs a probe, so planCaptureProbes is never asked "+
					"and this witness proves nothing about it", w.regex)
			}
			plan, ok := planCaptureProbes(w.regex, need)
			if ok {
				assertPlanIsTheCallersOwnPattern(t, w.regex, original, plan, inputs)
				t.Fatalf("planCaptureProbes accepted %q, whose rewrite is %q — a pattern that "+
					"is not the caller's own spelling and cannot be re-anchored by the emitter",
					w.regex, plan.regex)
			}

			// And the decline reaches the caller as a rejection rather
			// than as a wrong answer: no probed regex is emitted.
			got, err := ReplacementToCH("$d", w.regex)
			if err == nil {
				t.Fatalf("ReplacementToCH accepted %q; with no probe plan the shared name %q "+
					"stays unanswerable and must be rejected", w.regex, "d")
			}
			if got.ProbedRegex != "" {
				t.Errorf("ReplacementToCH rejected %q but still returned the rewrite %q",
					w.regex, got.ProbedRegex)
			}
		})
	}
}

// assertPlanIsTheCallersOwnPattern checks the postcondition every accepted
// plan owes its caller: re-anchoring the rewrite must give a pattern that
// compiles, that carries the original's capture groups — same count, same
// names, same order — once the probes are struck out, and that accepts and
// captures exactly what the original does.
func assertPlanIsTheCallersOwnPattern(
	t *testing.T, regex string, original *regexp.Regexp, plan captureProbePlan, inputs []string,
) {
	t.Helper()

	probed, err := regexp.Compile(anchorRegex(plan.regex))
	if err != nil {
		t.Errorf("regex %q: the plan's rewrite %q does not survive the emitter's own "+
			"anchoring: %v", regex, plan.regex, err)
		return
	}
	positions := originalGroupPositions(t, original, probed, regex)
	for _, src := range inputs {
		before := original.FindStringSubmatchIndex(src)
		after := probed.FindStringSubmatchIndex(src)
		if (before == nil) != (after == nil) {
			t.Errorf("regex %q rewritten to %q: %q matches %v before and %v after",
				regex, plan.regex, src, before != nil, after != nil)
			return
		}
		if before == nil {
			continue
		}
		for group, at := range positions {
			if before[2*group] != after[2*at] || before[2*group+1] != after[2*at+1] {
				t.Errorf("regex %q rewritten to %q: on %q capture group %d moved",
					regex, plan.regex, src, group)
				return
			}
		}
	}
}
