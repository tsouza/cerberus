package qlcommon

import (
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
)

// This file recovers, for a capture group whose own capture cannot report
// it, the one bit `extractGroups` throws away: whether the group TOOK
// PART in the match.
//
// The bit is unobservable only because every group cerberus can read is
// nullable — a group that took part capturing nothing renders as `''`,
// exactly like one that took no part. But nullability is a property of
// the group's SUBPATTERN, not of the position it occupies, and cerberus
// gets to choose which subpatterns it reads. So rather than reason harder
// about the groups the user wrote, this rewrites the REGEX to add a group
// whose emptiness answers the question directly.
//
// Walk up from the carrier to the nearest enclosing span that
//
//  1. cannot match the empty string, and
//  2. every match of which must pass through the carrier,
//
// and wrap that span in a fresh capture group — a PROBE. (1) makes the
// probe non-empty whenever it takes part, and (2) makes it take part
// exactly when the carrier does. Its non-emptiness is therefore the
// carrier's participation bit, and the selection over like-named groups
// becomes a search whose test reads the probe while its answer reads the
// carrier:
//
//	`(?:x(?P<dup>a?))?(?P<dup>b)`
//
// becomes
//
//	`(?:(?P<cerberusprobe0>x(?P<dup>a?)))?(?P<dup>b)`
//
// and on `xb` the probe captures `x` while the carrier captures nothing,
// so the search stops at the carrier and answers `''` — which is what
// Go's ExpandString answers, and what a first-non-empty search over the
// carriers alone gets wrong by answering `b`.
//
// (2) is checked against the span's OWN parse rather than the whole
// regex's: within `x(?P<dup>a?)` the carrier sits on the mandatory spine,
// even though within the whole pattern it does not. That is precisely the
// question a probe answers, so the sub-parse is the right frame.
//
// Two facts make the walk terminate correctly. Going outwards makes (1)
// easier to satisfy — a wider span holds more mandatory text — and (2)
// harder, since a wider span can take in an alternation or a quantifier
// the carrier sits under. (2) is also MONOTONE: once a span puts a
// skippable node above the carrier, so does every wider span. So the walk
// stops at the first span that fails (2), and takes the first that
// satisfies both.
//
// What this does not reach is a carrier with no such span at all —
// typically one alone in an alternation branch, where the branch is
// nullable and the alternation above it is what the carrier can be
// skipped by. Issue #1956 tracks the remainder.
//
// The spans are located by scanning the regex SOURCE, because
// `regexp/syntax` records no byte offsets: its parse tree cannot say
// where a node came from, and reconstructing a source string from a
// mutated tree would stake a user's query semantics on `Regexp.String()`
// round-tripping. Scanning instead means the rewritten regex is the
// user's own text with two bytes inserted, and every judgement about a
// span is made by handing the SUBSTRING back to `regexp/syntax` — so the
// parse tree stays the authority on meaning and the scanner only ever
// answers "which bytes".

// probeNamePrefix names the capture groups this file inserts. The
// rewritten regex is handed back to `regexp.Compile`, so the probes are
// readable off `SubexpNames` by name — which is what lets
// [planCaptureProbes] verify its own rewrite rather than trust its
// arithmetic.
const probeNamePrefix = "cerberusprobe"

// captureProbePlan is a regex rewritten to expose participation, together
// with the index translation the rewrite forces.
type captureProbePlan struct {
	// regex is the rewritten pattern, unanchored, in the same spelling
	// the caller passed in.
	regex string
	// toProbed maps a capture-group index in the ORIGINAL regex to the
	// index the same group holds in regex. Index 0 (the whole match) maps
	// to itself.
	toProbed []int
	// probeOf maps an original carrier index to the probe group whose
	// non-emptiness reports that carrier's participation. Keyed by the
	// ORIGINAL index; the value is in the PROBED numbering.
	probeOf map[int]int
}

// probedIndex translates a capture-group index from the original regex's
// numbering into the rewritten one. The zero plan — no rewrite — leaves
// every index alone, so callers need not branch on whether one happened.
func (p captureProbePlan) probedIndex(original int) int {
	if original < 0 || original >= len(p.toProbed) {
		return original
	}
	return p.toProbed[original]
}

// planCaptureProbes rewrites regex so that each capture group named in
// need gains a probe, and reports ok=false when NONE of them has a
// probeable span — a rewrite that answers nothing is only index churn.
//
// A carrier with no probeable span is left out of the plan rather than
// sinking it, because the carriers of one shared NAME are decided
// together but different names are decided apart: one name's
// unanswerable carrier must not cost another name the answer it earned.
// The caller reads [captureProbePlan.probeOf] per carrier and keeps its
// rejection for whichever names remain unanswered.
//
// need holds capture-group indices in regex's own numbering.
func planCaptureProbes(regex string, need []int) (captureProbePlan, bool) {
	if len(need) == 0 {
		return captureProbePlan{}, false
	}
	anchored := anchorRegex(regex)
	original, err := regexp.Compile(anchored)
	if err != nil {
		return captureProbePlan{}, false
	}
	// A pattern that already carries a probe name would make the rewrite
	// unreadable back off SubexpNames, so decline rather than guess.
	for _, name := range original.SubexpNames() {
		if strings.HasPrefix(name, probeNamePrefix) {
			return captureProbePlan{}, false
		}
	}
	groups, ok := scanSourceGroups(anchored)
	if !ok {
		return captureProbePlan{}, false
	}
	byCapture := map[int]int{}
	for i, g := range groups {
		if g.capturing {
			byCapture[g.capIndex] = i
		}
	}

	spans := map[int]sourceSpan{}
	for _, carrier := range need {
		at, known := byCapture[carrier]
		if !known {
			continue
		}
		if span, ok := probeSpanFor(anchored, groups, at); ok {
			spans[carrier] = span
		}
	}
	if len(spans) == 0 {
		return captureProbePlan{}, false
	}

	probed, probeNames, ok := insertProbes(anchored, spans)
	if !ok {
		return captureProbePlan{}, false
	}
	plan, ok := readBackPlan(original, probed, probeNames)
	if !ok {
		return captureProbePlan{}, false
	}
	// The probes all sit inside the anchoring wrapper, so stripping it
	// returns the rewrite to the caller's own spelling — and a wrapper
	// that did not survive means the rewrite landed somewhere this code
	// did not intend, which is a reason to decline rather than to trim
	// harder.
	body, ok := stripAnchors(plan.regex)
	if !ok {
		return captureProbePlan{}, false
	}
	plan.regex = body
	return plan, true
}

// stripAnchors removes the wrapper [anchorRegex] added.
//
// The two halves are derived from [anchorRegex] rather than written out
// again, because the emitter already carries its own copy of that
// spelling and a third one would be a third thing to keep in step.
func stripAnchors(anchored string) (string, bool) {
	prefix, suffix := anchorWrapper()
	if !strings.HasPrefix(anchored, prefix) || !strings.HasSuffix(anchored, suffix) {
		return "", false
	}
	return anchored[len(prefix) : len(anchored)-len(suffix)], true
}

// anchorWrapper returns the text [anchorRegex] puts before and after a
// pattern, by asking it to wrap a marker no regex source can contain and
// reading the halves off either side.
func anchorWrapper() (prefix, suffix string) {
	const marker = "\x00"
	wrapped := anchorRegex(marker)
	at := strings.Index(wrapped, marker)
	return wrapped[:at], wrapped[at+len(marker):]
}

// readBackPlan verifies the rewrite against the compiler and derives the
// index translation from it.
//
// This is the rewrite's own proof of work. The scanner computed byte
// offsets; rather than trust them, the probed pattern is compiled and its
// capture-group NAMES are compared against the original's. Deleting the
// probe names from the rewritten vector must reproduce the original
// vector exactly — same length, same names, same order. A probe inserted
// at a boundary that does not bracket what it was meant to therefore
// fails here instead of silently answering for the wrong span.
func readBackPlan(original *regexp.Regexp, probed string, probeNames map[int]string) (captureProbePlan, bool) {
	compiled, err := regexp.Compile(probed)
	if err != nil {
		return captureProbePlan{}, false
	}
	names := compiled.SubexpNames()
	plan := captureProbePlan{regex: probed, probeOf: map[int]int{}}
	probeIndex := map[string]int{}
	for i, name := range names {
		if strings.HasPrefix(name, probeNamePrefix) {
			probeIndex[name] = i
			continue
		}
		plan.toProbed = append(plan.toProbed, i)
	}
	originalNames := original.SubexpNames()
	if len(plan.toProbed) != len(originalNames) {
		return captureProbePlan{}, false
	}
	for i, at := range plan.toProbed {
		if names[at] != originalNames[i] {
			return captureProbePlan{}, false
		}
	}
	for carrier, name := range probeNames {
		at, known := probeIndex[name]
		if !known {
			return captureProbePlan{}, false
		}
		plan.probeOf[carrier] = at
	}
	return plan, true
}

// sourceSpan is a half-open byte range of a regex source.
type sourceSpan struct{ start, end int }

// insertProbes wraps each distinct span in a named capture group,
// returning the rewritten source and the probe name serving each carrier.
//
// Two carriers under one ancestor resolve to the same span and share a
// single probe, which keeps the rewrite as small as the question needs.
func insertProbes(src string, spans map[int]sourceSpan) (string, map[int]string, bool) {
	carriers := make([]int, 0, len(spans))
	for carrier := range spans {
		carriers = append(carriers, carrier)
	}
	sort.Ints(carriers)

	type edit struct {
		at   int
		open bool
		span sourceSpan
		name string
	}
	named := map[sourceSpan]string{}
	probeNames := map[int]string{}
	var edits []edit
	for _, carrier := range carriers {
		if !nestsCleanly(spans[carrier], spans, carriers) {
			return "", nil, false
		}
		span := spans[carrier]
		name, seen := named[span]
		if !seen {
			name = probeNamePrefix + strconv.Itoa(len(named))
			named[span] = name
			edits = append(edits,
				edit{at: span.start, open: true, span: span, name: name},
				edit{at: span.end, open: false, span: span, name: name})
		}
		probeNames[carrier] = name
	}
	// Spans nest, so at a shared offset the wider one must open first and
	// close last for the inserted parentheses to nest the same way.
	sort.SliceStable(edits, func(i, j int) bool {
		a, b := edits[i], edits[j]
		if a.at != b.at {
			return a.at < b.at
		}
		if a.open != b.open {
			return !a.open
		}
		wa, wb := a.span.end-a.span.start, b.span.end-b.span.start
		if a.open {
			return wa > wb
		}
		return wa < wb
	})

	var out strings.Builder
	prev := 0
	for _, e := range edits {
		if e.at < prev || e.at > len(src) {
			return "", nil, false
		}
		out.WriteString(src[prev:e.at])
		if e.open {
			out.WriteString("(?P<")
			out.WriteString(e.name)
			out.WriteString(">")
		} else {
			out.WriteString(")")
		}
		prev = e.at
	}
	out.WriteString(src[prev:])
	return out.String(), probeNames, true
}

// nestsCleanly reports whether span is either nested inside or disjoint
// from every other span in the set.
//
// Parentheses can only bracket a set of ranges that nests — the edits are
// applied in one forward pass, so a pair that merely OVERLAPS would emit
// its four delimiters in an order that brackets two DIFFERENT ranges than
// the ones asked for, and the result would still compile. Ancestor walks
// only ever produce nesting sets, so this never fires in practice; it is
// here because "wrapped a span nobody chose" is the one failure this file
// cannot detect after the fact.
func nestsCleanly(span sourceSpan, spans map[int]sourceSpan, carriers []int) bool {
	for _, carrier := range carriers {
		other := spans[carrier]
		disjoint := span.end <= other.start || other.end <= span.start
		nested := (span.start <= other.start && other.end <= span.end) ||
			(other.start <= span.start && span.end <= other.end)
		if !disjoint && !nested {
			return false
		}
	}
	return true
}

// probeSpanFor finds the span whose emptiness answers for the capture
// group at groups[at], or reports that no span does.
//
// The ladder is walked innermost-first, and the two tests are applied in
// the order their monotonicity demands: failing "every match of the span
// passes through the carrier" ends the walk, because no wider span can
// restore it, while failing "the span cannot match empty" only moves the
// walk outwards, where more mandatory text may make it hold.
func probeSpanFor(src string, groups []sourceGroup, at int) (sourceSpan, bool) {
	carrierOpen := groups[at].open
	for _, span := range candidateSpans(groups, at) {
		body := src[span.start:span.end]
		parsed, err := syntax.Parse(body, syntax.Perl)
		if err != nil {
			return sourceSpan{}, false
		}
		shape, known := captureShapes(body)[captureOrdinalIn(groups, span, carrierOpen)]
		if !known || !shape.unconditional {
			return sourceSpan{}, false
		}
		if !matchesEmpty(parsed) {
			return span, true
		}
	}
	return sourceSpan{}, false
}

// captureOrdinalIn reports the capture-group number the group opening at
// carrierOpen holds within span, which is what a parse of the span alone
// numbers it. Go numbers capture groups by the position of their opening
// parenthesis, so this counts openings.
func captureOrdinalIn(groups []sourceGroup, span sourceSpan, carrierOpen int) int {
	ordinal := 0
	for _, g := range groups {
		if g.capturing && g.open >= span.start && g.open <= carrierOpen {
			ordinal++
		}
	}
	return ordinal
}

// candidateSpans returns the ancestor spans to try for the group at
// groups[at], innermost first.
//
// Each enclosing group contributes two: the single alternation BRANCH
// holding the carrier, then the group's whole body. The branch comes
// first because it is the tighter frame — inside `(?:xA|y)` the branch
// `xA` puts the carrier on a mandatory spine that the full body, an
// alternation, does not.
func candidateSpans(groups []sourceGroup, at int) []sourceSpan {
	var out []sourceSpan
	child := at
	for parent := groups[at].parent; parent >= 0; parent = groups[parent].parent {
		g := groups[parent]
		if branch, split := branchContaining(g, groups[child].open); split {
			out = append(out, branch)
		}
		out = append(out, sourceSpan{start: g.bodyStart, end: g.bodyEnd})
		child = parent
	}
	return out
}

// branchContaining returns the top-level alternation branch of g's body
// that holds the group opening at offset, and whether g's body is an
// alternation at all.
func branchContaining(g sourceGroup, offset int) (sourceSpan, bool) {
	if len(g.alternations) == 0 {
		return sourceSpan{}, false
	}
	start := g.bodyStart
	for _, bar := range g.alternations {
		if offset < bar {
			return sourceSpan{start: start, end: bar}, true
		}
		start = bar + 1
	}
	return sourceSpan{start: start, end: g.bodyEnd}, true
}
