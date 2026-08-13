package qlcommon

import "regexp/syntax"

// This file answers one question about a `label_replace` regex: when a
// capture-group NAME is carried by several groups, which of them can Go's
// `Regexp.ExpandString` pick, and is that choice observable from SQL?
//
// ExpandString expands `$name` to the first carrier that TOOK PART in the
// row's match — the first index i with `match[2*i] >= 0`. ClickHouse's
// `extractGroups` renders "took no part" and "took part, matched empty"
// identically, both as `''`, so participation is not directly readable
// back. The emitted form is therefore a search for the first NON-EMPTY
// capture, and the job here is to decide statically when those two
// searches cannot disagree.
//
// They disagree on exactly one event: the first carrier that takes part
// captures the empty string, AND a later carrier in the same match
// captures text. "First non-empty" skips the empty carrier Go stopped at
// and answers with the later one. Every other combination agrees —
// including the case where the first participant captures empty and every
// later carrier is empty too, since both searches then yield `''`.
//
// Three facts about a carrier's POSITION in the parse tree rule that event
// out, and together they admit far more than nullability alone:
//
//  1. A carrier whose subpattern cannot match the empty string never
//     captures empty while taking part, so it can never be the culprit.
//     This is the only fact the guard used to consider.
//  2. A carrier that takes part in EVERY match is where Go's scan always
//     stops, so no carrier after it can ever be reached. Truncating the
//     search there makes every later carrier irrelevant — nullable or not.
//  3. Two carriers in different branches of one alternation never take
//     part in the same match, so an empty capture by the earlier one
//     implies the later one took no part and reads as `''` as well.
//
// The residue — a nullable carrier that can be skipped while a later
// carrier that can co-occur with it captures text — stays rejected,
// because no ClickHouse expression over `extractGroups` can tell the two
// apart. Issue #1956 tracks it.

// captureShape is what one capture group's position in a regex's parse
// tree says about when that group takes part in a match.
type captureShape struct {
	// nullable reports whether the group's subpattern can match the empty
	// string. A nullable group that takes part may still capture nothing,
	// which is what makes it indistinguishable from one that took no part.
	nullable bool
	// unconditional reports whether every match of the whole regex must
	// pass through this group. Go's scan for the first participating
	// carrier always stops at such a group, so carriers after it are
	// unreachable.
	unconditional bool
	// spine is the path from the parse tree's root down to (but not
	// including) this group's OpCapture node. Two spines are compared to
	// decide whether their groups can ever take part in the same match.
	spine []captureSpineStep
}

// captureSpineStep is one edge of a [captureShape.spine]: the node the
// path passed through and which of its children it descended into. The
// child index matters because two groups under the same alternation are
// mutually exclusive only when they sit in DIFFERENT branches of it.
type captureSpineStep struct {
	node   *syntax.Regexp
	branch int
}

// captureShapes parses regex and reports, per capture-group index, the
// shape facts above. The indices are the ones `Regexp.SubexpNames` reports
// for the same pattern, because this parses with the same flags
// `regexp.Compile` does and `OpCapture.Cap` is that same numbering.
//
// An unparseable regex yields a nil map. Every consumer reads a missing
// entry as "nullable, conditional, and exclusive of nothing", which keeps
// the rejection rather than emitting SQL whose answer might be wrong.
func captureShapes(regex string) map[int]captureShape {
	parsed, err := syntax.Parse(regex, syntax.Perl)
	if err != nil {
		return nil
	}
	out := map[int]captureShape{}
	collectCaptureShapes(parsed, nil, out)
	return out
}

// collectCaptureShapes walks re, recording each capture group's shape
// against the spine that reached it.
func collectCaptureShapes(re *syntax.Regexp, spine []captureSpineStep, out map[int]captureShape) {
	if re.Op == syntax.OpCapture {
		owned := make([]captureSpineStep, len(spine))
		copy(owned, spine)
		out[re.Cap] = captureShape{
			nullable:      matchesEmpty(re.Sub[0]),
			unconditional: unconditionalSpine(owned),
			spine:         owned,
		}
	}
	for i, sub := range re.Sub {
		collectCaptureShapes(sub, append(spine, captureSpineStep{node: re, branch: i}), out)
	}
}

// unconditionalSpine reports whether every match of the whole regex must
// descend the given spine — i.e. no node along it can be skipped.
func unconditionalSpine(spine []captureSpineStep) bool {
	for _, step := range spine {
		if skippable(step.node) {
			return false
		}
	}
	return true
}

// skippable reports whether a match of the whole regex can decline to
// enter re at all: a quest or star admits zero repetitions, a `{0,n}`
// repeat likewise, and an alternation enters only one of its branches so
// every branch is skippable.
//
// Deliberately conservative in the same direction as [matchesEmpty]: an
// operator this switch does not name reads as skippable, which costs the
// caller the truncation it might have earned but never claims a group
// takes part when it does not.
func skippable(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpStar, syntax.OpQuest, syntax.OpAlternate:
		return true
	case syntax.OpRepeat:
		return re.Min == 0
	case syntax.OpConcat, syntax.OpCapture, syntax.OpPlus:
		// Concatenation and a capture are pass-throughs, and a plus runs
		// its body at least once.
		return false
	}
	return true
}

// reenterable reports whether one match can pass through re more than
// once. It is what stops [mutuallyExclusive] from trusting an alternation
// that sits under a repetition: `(?:x(a?)|(b))*` enters the alternation
// once per iteration, so matching `bbbx` takes the SECOND branch and then
// the FIRST in the same match and both groups take part.
//
// The switch names the operators that CANNOT repeat and defaults to true,
// which is the opposite arrangement to [skippable] and [matchesEmpty] for
// the same reason all three share: the default has to be the answer that
// keeps the rejection. Here that is "assume it can repeat", so an operator
// a future regexp/syntax introduces costs an exclusivity claim rather than
// silently granting one that does not hold.
func reenterable(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpConcat, syntax.OpCapture, syntax.OpAlternate, syntax.OpQuest:
		// Pass-throughs, plus a quest: entered at most once per match.
		return false
	case syntax.OpRepeat:
		// `x{1}` runs its body exactly once; anything else can repeat.
		return re.Max != 1
	}
	return true
}

// mutuallyExclusive reports whether two capture groups can never both take
// part in the same match.
//
// The test is structural: their spines must diverge at an ALTERNATION —
// each descending a different branch of it — and no node above that
// alternation may be re-enterable, since a repetition around it would let
// one match take a different branch on each pass.
//
// Divergence anywhere else (a concatenation, say) proves nothing: both
// parts of a concatenation take part in the same match. A spine that is a
// prefix of the other means one group encloses the other, which makes them
// co-participants rather than alternatives.
func mutuallyExclusive(a, b captureShape) bool {
	shared := 0
	for shared < len(a.spine) && shared < len(b.spine) &&
		a.spine[shared].node == b.spine[shared].node &&
		a.spine[shared].branch == b.spine[shared].branch {
		shared++
	}
	if shared >= len(a.spine) || shared >= len(b.spine) {
		return false
	}
	// Both spines are root-down paths through one tree, so having agreed
	// on every step up to here they are standing on the same node; only
	// the branch they take out of it can differ.
	fork := a.spine[shared]
	if fork.node.Op != syntax.OpAlternate || fork.branch == b.spine[shared].branch {
		return false
	}
	for _, step := range a.spine[:shared] {
		if reenterable(step.node) {
			return false
		}
	}
	return true
}
