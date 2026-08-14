package qlcommon

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"testing"
)

// TestCaptureShapesUnconditional pins the second of the three facts
// [captureGroups.expressibleCarriers] reasons from: whether EVERY match of
// the regex must pass through a given capture group.
//
// The verdict decides where the carrier search is truncated. Call a
// conditional group unconditional and cerberus drops carriers Go can still
// reach, answering with an earlier carrier's empty capture where
// Prometheus answers with a later one's text; call an unconditional group
// conditional and a query Prometheus answers gets rejected.
//
// Every case drives a regex STRING through the production anchoring, so
// the operator each row claims to exercise is the operator Go's parser
// actually produces for it — and the indices are the ones
// `Regexp.SubexpNames` reports, which is what the resolution looks up.
func TestCaptureShapesUnconditional(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		// want is keyed by capture-group index.
		want map[int]bool
	}{
		// A concatenation and a capture are pass-throughs: a group under
		// nothing but those is on the mandatory spine.
		{"bare_group", "(a)", map[int]bool{1: true}},
		{"concatenated_groups", "(a)(b)", map[int]bool{1: true, 2: true}},
		{"nested_groups", "((a))", map[int]bool{1: true, 2: true}},

		// A plus runs its body at least once, and a `{n,}` repeat with a
		// positive minimum does too, so neither can be skipped.
		{"plus", "((?:a)+)", map[int]bool{1: true}},
		{"group_under_plus", "(?:(a))+", map[int]bool{1: true}},
		{"repeat_min_positive", "(?:(a)){2,3}", map[int]bool{1: true}},

		// Quest, star and a `{0,n}` repeat all admit zero repetitions, so
		// a match can decline to enter them.
		{"group_under_quest", "(?:(a))?", map[int]bool{1: false}},
		{"group_under_star", "(?:(a))*", map[int]bool{1: false}},
		{"group_under_repeat_min_zero", "(?:(a)){0,2}", map[int]bool{1: false}},

		// An alternation enters exactly one branch, so every branch is
		// skippable — including the first.
		{"alternation_branches", "(ab)|(cd)", map[int]bool{1: false, 2: false}},

		// Mixed: the group outside the alternation is still mandatory even
		// though its neighbour is not.
		{"one_branch_one_spine", "(?:(ab)|y)(c)", map[int]bool{1: false, 2: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			shapes := captureShapes(anchorRegex(tc.regex))
			if shapes == nil {
				t.Fatalf("captureShapes(%q) = nil, want a map — the regex must parse", tc.regex)
			}
			if len(shapes) != len(tc.want) {
				t.Fatalf("captureShapes(%q) recorded %d group(s), want %d: %+v",
					tc.regex, len(shapes), len(tc.want), shapes)
			}
			for idx, want := range tc.want {
				shape, ok := shapes[idx]
				if !ok {
					t.Fatalf("captureShapes(%q) recorded no shape for group %d", tc.regex, idx)
				}
				if shape.unconditional != want {
					t.Errorf("captureShapes(%q)[%d].unconditional = %v, want %v",
						tc.regex, idx, shape.unconditional, want)
				}
			}
		})
	}
}

// TestMutuallyExclusiveCarriers pins the third fact: whether two capture
// groups can ever take part in the SAME match. It is what lets a nullable
// carrier through when every later carrier lives in a different branch of
// one alternation — the shape issue #1956 was filed over.
func TestMutuallyExclusiveCarriers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
		a, b  int
		want  bool
	}{
		// Different branches of one alternation: never both.
		{"sibling_branches", "(ab)|(cd)", 1, 2, true},
		{"nested_alternation", "(ab)|(?:(cd)|ef)", 1, 2, true},
		{"branches_with_prefixes", "x(ab)|y(cd)", 1, 2, true},

		// A concatenation puts both groups in every match that reaches
		// them, so divergence there proves nothing.
		{"concatenation", "(ab)(cd)", 1, 2, false},
		{"same_branch", "(?:(ab)(cd))|ef", 1, 2, false},

		// One group enclosing the other makes them co-participants.
		{"nested_capture", "((ab))", 1, 2, false},

		// A repetition around the alternation re-enters it, so one match
		// can take a different branch on each pass and both groups take
		// part. This is the arm that keeps `(?:x(?P<d>a?)|(?P<d>b))*`
		// rejected, where input "bbbx" really does reach both.
		{"star_around_the_alternation", "(?:(ab)|(cd))*", 1, 2, false},
		{"plus_around_the_alternation", "(?:(ab)|(cd))+", 1, 2, false},
		{"repeat_around_the_alternation", "(?:(ab)|(cd)){2}", 1, 2, false},

		// A bounded repeat that runs its body exactly once cannot
		// re-enter, so exclusivity survives it.
		{"repeat_once_around_the_alternation", "(?:(ab)|(cd)){1}", 1, 2, true},

		// A repetition BELOW the fork is irrelevant — it cannot make the
		// other branch reachable.
		{"repetition_inside_a_branch", "(?:(ab))*|(cd)", 1, 2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			shapes := captureShapes(anchorRegex(tc.regex))
			if shapes == nil {
				t.Fatalf("captureShapes(%q) = nil, want a map — the regex must parse", tc.regex)
			}
			a, aok := shapes[tc.a]
			b, bok := shapes[tc.b]
			if !aok || !bok {
				t.Fatalf("captureShapes(%q) missing group %d or %d: %+v", tc.regex, tc.a, tc.b, shapes)
			}
			if got := mutuallyExclusive(a, b); got != tc.want {
				t.Errorf("mutuallyExclusive(%q, groups %d and %d) = %v, want %v",
					tc.regex, tc.a, tc.b, got, tc.want)
			}
			// Exclusivity is symmetric; the resolution only ever asks in
			// one direction, so an asymmetric implementation would decide
			// the same question differently depending on carrier order.
			if got := mutuallyExclusive(b, a); got != tc.want {
				t.Errorf("mutuallyExclusive(%q, groups %d and %d) = %v, want %v (asymmetric)",
					tc.regex, tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestUnclassifiedOperatorsKeepTheRejection covers the default arm of each
// parse-tree classifier — arms no regex string can reach, because every
// operator Go's parser emits from a well-formed pattern is named
// explicitly. [syntax.OpNoMatch] stands in for "an operator this code has
// never seen".
//
// They are not dead weight. Each default is the guarantee that a FUTURE
// operator — one a newer regexp/syntax introduces, or one a simplification
// pass produces — costs the resolution an acceptance rather than buying it
// one. The two classifiers default in OPPOSITE directions because the
// conservative answer differs: an unknown ancestor must read as skippable
// (so no truncation is claimed) and as re-enterable (so no exclusivity is
// claimed).
func TestUnclassifiedOperatorsKeepTheRejection(t *testing.T) {
	t.Parallel()

	unknown := &syntax.Regexp{Op: syntax.OpNoMatch}

	if !skippable(unknown) {
		t.Error("skippable(OpNoMatch) = false, want true — an unclassified ancestor must not " +
			"let a carrier be read as one every match passes through")
	}
	if !reenterable(unknown) {
		t.Error("reenterable(OpNoMatch) = false, want true — an unclassified ancestor must not " +
			"let two carriers be read as mutually exclusive")
	}
}

// participationCarrierBodies are the subpatterns each carrier is built
// from in the differential below: a mix of nullable and non-nullable
// shapes, single- and multi-rune, so the corpus covers both sides of every
// verdict the resolution makes.
var participationCarrierBodies = []string{"a", "b", "a?", "b?", "a*", "", "ab", "a|b", "x?"}

// participationArrangement is one structural template the carriers are
// dropped into.
type participationArrangement struct {
	// template has `C1`, `C2` (and `C3`) standing in for one carrier
	// apiece.
	template string
	// carriers is how many the template takes.
	carriers int
	// alwaysExpressible marks an arrangement whose carriers are pairwise
	// exclusive BY CONSTRUCTION — every pair sits in different branches of
	// one alternation that nothing re-enters — so the arrangement is
	// expressible whatever subpatterns fill it. It is what gives the
	// differential a non-vacuous acceptance floor without hard-coding a
	// tuned count.
	alwaysExpressible bool
}

// participationArrangements are the structural shapes the corpus crosses
// with every combination of carrier subpatterns: concatenation,
// alternation, optional wrappers, repetitions above and below an
// alternation, and carriers straddling other groups.
//
// The three-carrier rows are not decoration. With two carriers the
// "exclusive of EVERY later carrier" test in
// [captureGroups.expressibleCarriers] can never look at more than one
// element, so a mutant checking only the next carrier would pass a
// two-carrier corpus with an identical accept count — the gap this list
// closes. Three carriers is where "exclusive of the second but
// co-occurring with the third" becomes expressible at all.
var participationArrangements = []participationArrangement{
	{template: "C1C2", carriers: 2},
	{template: "C1|C2", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1)?C2", carriers: 2},
	{template: "C1(?:C2)?", carriers: 2},
	{template: "(?:C1|x)C2", carriers: 2},
	{template: "C1(?:x|C2)", carriers: 2},
	{template: "(?:C1C2)|y", carriers: 2},
	{template: "(?:C1|C2)*", carriers: 2},
	{template: "(?:C1|C2)+", carriers: 2},
	{template: "(?:C1|C2)?", carriers: 2, alwaysExpressible: true},
	{template: "xC1yC2z", carriers: 2},
	{template: "(?:C1)*C2", carriers: 2},
	{template: "C1(?:C2)*", carriers: 2},
	{template: "(?:xC1)?C2", carriers: 2},
	{template: "C1(?:xC2)?", carriers: 2},
	{template: "(?:C1|C2)|z", carriers: 2, alwaysExpressible: true},
	{template: "z|(?:C1|C2)", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1x|C2y)", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1|x)(?:C2|y)", carriers: 2},
	{template: "(?:(?:C1|q)|C2)", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1|(?:q|C2))", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1{2}|C2)", carriers: 2, alwaysExpressible: true},
	{template: "(?:C1|C2){2}", carriers: 2},

	// A branch with LITERAL text of its own is what gives a carrier
	// inside it a non-nullable ancestor to probe, so these are the
	// arrangements the probe rewrite actually turns on — and, under a
	// repetition, the ones where one match can enter both branches and a
	// probe has to keep answering for its own carrier only.
	{template: "(?:xC1|C2)*", carriers: 2},
	{template: "(?:xC1|C2)+", carriers: 2},
	{template: "(?:xC1|C2)?", carriers: 2},
	{template: "(?:xC1|yC2)*", carriers: 2},
	{template: "(?:xC1)?(?:yC2)?", carriers: 2},
	{template: "(?:xC1)*(?:yC2)*", carriers: 2},
	{template: "(?:x(?:yC1))?C2", carriers: 2},

	// Three carriers. The first four put a carrier in an alternation
	// branch alongside a THIRD that sits outside it, which is the shape
	// where clearing a carrier against only its immediate successor is not
	// enough.
	{template: "(?:C1|C2)C3", carriers: 3},
	{template: "C1(?:C2|C3)", carriers: 3},
	{template: "(?:C1|C2)(?:C3|q)", carriers: 3},
	{template: "(?:C1|C2)?C3", carriers: 3},
	{template: "C1|C2|C3", carriers: 3, alwaysExpressible: true},
	{template: "(?:C1|C2|C3)?", carriers: 3, alwaysExpressible: true},
	{template: "C1C2C3", carriers: 3},
	{template: "(?:C1)?C2(?:C3)?", carriers: 3},
	{template: "(?:C1|C2|C3)*", carriers: 3},
	{template: "(?:xC1)?(?:C2|C3)", carriers: 3},
	{template: "(?:xC1|C2)*C3", carriers: 3},
	{template: "(?:xC1)?(?:yC2)?C3", carriers: 3},
	{template: "C1(?:xC2|yC3)?", carriers: 3},
}

// participationCorpus expands every arrangement against every combination
// of carrier subpatterns, returning the regex strings both differentials
// walk. Building it once, here, is what keeps the two tests looking at the
// same corpus.
func participationCorpus() []string {
	var out []string
	for _, arr := range participationArrangements {
		for _, bodies := range carrierBodyTuples(arr.carriers) {
			out = append(out, buildParticipationRegex(arr.template, bodies))
		}
	}
	return out
}

// buildParticipationRegex substitutes one carrier per `Cn` placeholder.
func buildParticipationRegex(template string, bodies []string) string {
	pairs := make([]string, 0, 2*len(bodies))
	for i, body := range bodies {
		pairs = append(pairs, "C"+strconv.Itoa(i+1), "(?P<dup>"+body+")")
	}
	return strings.NewReplacer(pairs...).Replace(template)
}

// carrierBodyTuples returns every ordered n-tuple of carrier subpatterns.
func carrierBodyTuples(n int) [][]string {
	tuples := [][]string{{}}
	for range n {
		var next [][]string
		for _, prefix := range tuples {
			for _, body := range participationCarrierBodies {
				grown := make([]string, len(prefix), len(prefix)+1)
				copy(grown, prefix)
				next = append(next, append(grown, body))
			}
		}
		tuples = next
	}
	return tuples
}

// alwaysExpressibleCount is how many corpus regexes come from arrangements
// whose carriers are pairwise exclusive by construction. Every one of them
// must be accepted whatever its subpatterns are, which makes it an
// acceptance floor derived from the corpus rather than a tuned constant.
func alwaysExpressibleCount() int {
	total := 0
	for _, arr := range participationArrangements {
		if !arr.alwaysExpressible {
			continue
		}
		combinations := 1
		for range arr.carriers {
			combinations *= len(participationCarrierBodies)
		}
		total += combinations
	}
	return total
}

// TestExpressibleCarriersAgreeWithExpandString is the differential that
// makes the shared-capture-name narrowing trustworthy. It is the reason
// the guard may be anything looser than "no carrier is nullable".
//
// For every regex in the corpus that [ReplacementToCH] ACCEPTS, the
// ClickHouse form it returns must equal Go's `Regexp.ExpandString` — the
// engine reference Prometheus and reference Loki both run `label_replace`
// through — on every input string over a small alphabet. One disagreement
// anywhere is a silently wrong label value on the wire, so the assertion
// is exact and unconditional.
//
// The corpus is exhaustive over its grammar rather than sampled: every
// template crossed with every pair of carrier bodies. That is what makes
// it evidence about the RULE and not about a handful of examples — the
// nullable/non-nullable, skippable/mandatory and exclusive/co-occurring
// axes are all crossed against each other rather than varied one at a
// time.
func TestExpressibleCarriersAgreeWithExpandString(t *testing.T) {
	t.Parallel()

	inputs := stringsUpTo("abxy", 3)

	corpus := participationCorpus()
	accepted, probed := 0, 0
	for _, regex := range corpus {
		re, err := regexp.Compile(anchorRegex(regex))
		if err != nil {
			t.Fatalf("corpus regex %q does not compile: %v", regex, err)
		}

		got, err := ReplacementToCH("$dup", regex)
		if err != nil {
			continue
		}
		accepted++
		if got.ProbedRegex != "" {
			probed++
		}

		for _, src := range inputs {
			match := re.FindStringSubmatchIndex(src)
			if match == nil {
				continue
			}
			want := string(re.ExpandString(nil, "$dup", src, match))
			evaluated := evaluateReplacement(got, regex, src)
			if evaluated != want {
				t.Fatalf("regex %q src %q: %+v evaluates to %q; "+
					"Go's ExpandString gives %q",
					regex, src, got, evaluated, want)
			}
		}
	}

	// A guard that rejected everything would satisfy the loop above
	// vacuously. The floor is the corpus's own arithmetic rather than a
	// tuned number: every arrangement whose carriers are pairwise
	// exclusive by construction must be accepted whatever subpatterns fill
	// it, because carriers in different branches of one alternation cannot
	// take part in the same match.
	if floor := alwaysExpressibleCount(); accepted < floor {
		t.Errorf("ReplacementToCH accepted %d of %d corpus regexes, want at least %d — "+
			"the differential proves nothing about a guard that rejects everything",
			accepted, len(corpus), floor)
	}
	// The probe rewrite must actually FIRE. Every regex it turns on is one
	// the static facts alone reject, so a change that silently stopped
	// planning probes would leave the loop above passing on a strictly
	// smaller set — green, and having quietly given the capability back.
	// The two counts also report the narrowing itself: acceptance without
	// a rewrite is exactly what the guard admitted before probes existed,
	// because the rewrite only ever adds.
	if probed == 0 {
		t.Error("no corpus regex was accepted through a probe rewrite — the rewrite is inert, " +
			"and the shapes it exists to express are being rejected again")
	}
	t.Logf("shared-capture-name corpus: %d regexes, %d accepted (%d without a probe rewrite, "+
		"%d only because of one), %d inputs each",
		len(corpus), accepted, accepted-probed, probed, len(inputs))
}

// stringsUpTo returns every string over alphabet with length 0..maxLen.
func stringsUpTo(alphabet string, maxLen int) []string {
	out := []string{""}
	frontier := []string{""}
	for range maxLen {
		var next []string
		for _, prefix := range frontier {
			for _, r := range alphabet {
				next = append(next, prefix+string(r))
			}
		}
		out = append(out, next...)
		frontier = next
	}
	return out
}

// TestExpressibleCarriersRejectionsHaveWitnesses is the differential's
// mirror image, and the reason the guard may not be narrowed further by
// guesswork: every corpus regex ReplacementToCH REJECTS is checked for a
// witness — an input on which the emitted search would really disagree
// with Go — and the count of witnessed rejections is held above a floor.
//
// Without this, a guard that drifted towards rejecting more would still
// pass the differential above, since rejecting is always "safe". This test
// makes over-rejection visible by naming how much of the rejected set is
// genuinely inexpressible.
func TestExpressibleCarriersRejectionsHaveWitnesses(t *testing.T) {
	t.Parallel()

	inputs := stringsUpTo("abxyzq", 3)

	var rejected, witnessed int
	var unwitnessed []string
	for _, regex := range participationCorpus() {
		if _, err := ReplacementToCH("$dup", regex); err == nil {
			continue
		}
		rejected++

		re := regexp.MustCompile(anchorRegex(regex))
		if witness := firstNonEmptyDisagreement(re, inputs); witness != "" {
			witnessed++
			continue
		}
		unwitnessed = append(unwitnessed, regex)
	}

	if rejected == 0 {
		t.Fatal("no corpus regex was rejected — this test would pass vacuously")
	}
	// The unwitnessed remainder is the guard's conservatism: shapes whose
	// carriers the static analysis cannot clear even though greedy
	// matching happens to keep them in agreement. It is real, so the floor
	// is a majority rather than everything; a guard that started rejecting
	// broad swathes of expressible shapes would drive this ratio down.
	if witnessed*2 <= rejected {
		t.Errorf("only %d of %d rejected corpus regexes have a divergence witness; "+
			"the guard has drifted into rejecting shapes it could express. "+
			"Unwitnessed sample: %s",
			witnessed, rejected, strings.Join(unwitnessed[:min(len(unwitnessed), 10)], ", "))
	}
	t.Logf("rejected %d corpus regexes, %d with a concrete divergence witness", rejected, witnessed)
}

// firstNonEmptyDisagreement returns a description of the first input on
// which "the first carrier with a non-empty capture" differs from Go's
// ExpandString, or the empty string when the two agree everywhere. It
// models the emitted `arrayFirst` search over the FULL carrier list, which
// is the most permissive form the emitter could have produced.
func firstNonEmptyDisagreement(re *regexp.Regexp, inputs []string) string {
	var carriers []int
	for idx, name := range re.SubexpNames() {
		if name == "dup" {
			carriers = append(carriers, idx)
		}
	}
	for _, src := range inputs {
		match := re.FindStringSubmatchIndex(src)
		if match == nil {
			continue
		}
		want := string(re.ExpandString(nil, "$dup", src, match))
		groups := re.FindStringSubmatch(src)
		got := ""
		for _, idx := range carriers {
			if groups[idx] != "" {
				got = groups[idx]
				break
			}
		}
		if got != want {
			return fmt.Sprintf("input %q: ExpandString %q, first non-empty %q", src, want, got)
		}
	}
	return ""
}
