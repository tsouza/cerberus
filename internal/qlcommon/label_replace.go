// Package qlcommon hosts small, lowering-layer helpers shared across
// the PromQL / LogQL / TraceQL heads. Functions here translate
// upstream-QL semantics into shapes the shared chplan IR + the chsql
// emitter expect, so each language's lowering can stay focused on its
// own AST.
package qlcommon

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tsouza/cerberus/internal/chplan"
)

// maxCHBackref is the highest capture-group index ClickHouse's
// `replaceRegexpOne` substitution syntax can address: `\0` (the whole
// match) through `\9`. Go's replacement templates have no such ceiling,
// so a reference above this is the one shape the `replaceRegexpOne` form
// cannot express — [ReplacementSegments] expresses it instead, by
// decomposing the template so the emitter can index `extractGroups`,
// which has no ceiling.
const maxCHBackref = 9

// unresolvedGroup is the capture-group index reported for a reference
// that binds to nothing — an index past the regex's group count, or a
// `$name` naming no group. Go's `ExpandString` substitutes the empty
// string for both, so the translation drops the backref.
const unresolvedGroup = -1

// ReplacementToCH translates a Go-`regexp` replacement template
// (`$1` / `${1}` / `$$` syntax — used by both PromQL's
// `label_replace` and LogQL's `label_replace` per their reference
// implementations) into the equivalent ClickHouse `replaceRegexpOne`
// replacement (`\1` / `\\` syntax).
//
// PromQL runs the replacement through Go's `regexp.Regexp.ExpandString`;
// LogQL's `label_replace` does the same. Both treat:
//
//   - `$$`            → literal `$`
//   - `$N` / `${N}`   → numbered capture group N
//   - `$name` / `${name}` → named capture group
//
// ClickHouse's `replaceRegexpOne` uses backslash escapes instead:
//
//   - `\\`            → literal backslash
//   - `\0` … `\9`     → numbered capture group (`\0` = whole match)
//
// Without translation, a replacement like `"svc-$1"` is passed to CH
// verbatim and emitted as the literal string `svc-$1` — the capture
// group is never substituted.
//
// Translation rules implemented here — the reference for every one of
// them is Go's `Regexp.ExpandString`, which is the engine both QLs run
// their replacement through, so this function reproduces its reference
// splitter ([extractRef]) rather than approximating it:
//
//   - Every literal `\` in the input becomes `\\`, so a literal
//     backslash in the QL template survives as a literal backslash in
//     CH and is not re-read as the start of one of the `\N` backrefs
//     spliced in below.
//   - `$$` → `$` (literal dollar).
//   - `$N` / `${N}` for ANY index N — including multi-digit `$10` —
//     resolves to capture group N, provided the regex has at least N
//     capture groups. Groups up to 9 render as `\N`; a higher one takes
//     the Segments form described below.
//   - `$name` / `${name}` → the index of the capture group that carries
//     that name.
//   - A reference that binds to nothing — an index past the regex's
//     group count, or a name no group carries — expands to the empty
//     string, exactly as ExpandString does.
//   - A `$` that starts no reference at all (end of string, `$-`,
//     `${unclosed`) is emitted verbatim, again as ExpandString does.
//
// Two shapes are expressible, just not as a `replaceRegexpOne` template:
// a reference to a capture group above CH's `\9` substitution ceiling,
// and a reference to a name several capture groups share. Both carry
// [CHReplacement.Segments] instead, which the emitter renders as a
// `concat` of literal runs and `extractGroups` subscripts — the second
// selecting among the like-named groups' subscripts with `arrayFirst`.
// The single shape with no faithful translation at all is that second one
// when any of the like-named groups can match the EMPTY STRING — see
// [captureGroups.resolve].
//
// regex is the regex string the replacement is applied against; it is
// compiled to resolve capture-group names and to count groups so
// out-of-range backrefs can be rewritten to the empty string. CH
// validates `replaceRegexpOne`'s substitution string against the
// regex's capture-group count at SQL-parse time and rejects backrefs
// that exceed it (Code 36, BAD_ARGUMENTS) — even on rows where match()
// short-circuits the if-branch that owns the replaceRegexpOne call.
// Dropping the backref preserves the upstream empty-string semantics on
// the (unreachable) hot path and unblocks the SQL parser on the
// (very-much-reachable) cold path where the regex doesn't match
// anything.
func ReplacementToCH(repl, regex string) (CHReplacement, error) {
	segments, probed, err := ReplacementSegments(repl, regex)
	if err != nil {
		return CHReplacement{}, err
	}
	return chReplacement(segments, probed), nil
}

// chReplacement picks the output form a decomposition takes.
//
// A group above ClickHouse's `\9` ceiling has no `\N` spelling, and a
// reference to a name several groups share is a SELECTION among indices
// rather than one index — neither fits a `replaceRegexpOne` substitution
// string, and both are expressible over the `extractGroups`
// decomposition.
//
// A probe rewrite forces the same choice whatever the indices look like.
// The substitution string is applied against the regex `match(...)` ran,
// which is the ORIGINAL one, while every index in a rewritten
// decomposition is numbered against the REWRITTEN pattern — so a template
// built from those indices would point at the wrong groups. The
// `extractGroups` form is the one that reads the rewritten pattern, so it
// is the only form a rewrite may take.
func chReplacement(segments []chplan.LabelReplaceSegment, probed string) CHReplacement {
	if probed != "" {
		return CHReplacement{Segments: segments, ProbedRegex: probed}
	}
	for _, seg := range segments {
		if seg.Group > maxCHBackref || len(seg.Fallbacks) > 0 {
			return CHReplacement{Segments: segments}
		}
	}
	return CHReplacement{Template: renderCHTemplate(segments)}
}

// CHReplacement is the ClickHouse-side form of a Go replacement template.
// Exactly one of its fields describes the substituted value: Template
// when every reference resolves to a single capture group that fits CH's
// `\0`–`\9` substitution syntax, Segments when one does not — because it
// names a group above the ceiling, or because it names a group NAME
// several groups share and so denotes a selection rather than an index.
type CHReplacement struct {
	// Template is the `replaceRegexpOne` substitution string.
	Template string
	// ProbedRegex, when non-empty, is the regex the emitter must feed
	// `extractGroups` in place of the one the query named: the same
	// pattern with synthetic capture groups added so that a carrier whose
	// own capture cannot report whether it took part in the match has one
	// that can. It matches the same strings as the original — the added
	// groups only observe — but it NUMBERS groups differently, so every
	// index in Segments is already in its numbering and the two must be
	// read together. See [planCaptureProbes].
	ProbedRegex string
	// Segments is the literal-run / capture-group decomposition the
	// emitter renders as a `concat` over `extractGroups`. A segment's
	// Literal is the DECODED text — `$$` has already collapsed to `$`,
	// and a backslash is a real backslash — so each output form applies
	// whatever escaping its own syntax needs.
	Segments []chplan.LabelReplaceSegment
}

// ReplacementSegments splits a Go replacement template into the
// alternating literal runs and capture-group references that make it up,
// resolving each reference against regex's capture-group metadata.
//
// This is the single walk of the template that both output forms are
// derived from: [renderCHTemplate] folds it back into a
// `replaceRegexpOne` substitution string, and the emitter renders it as a
// `concat` when a referenced group sits above CH's ceiling. Deriving both
// from one decomposition is what keeps them from drifting apart.
//
// References that bind to nothing — an index past the regex's group
// count, a name no group carries — contribute nothing at all, exactly as
// Go's `ExpandString` substitutes the empty string for them.
func ReplacementSegments(repl, regex string) ([]chplan.LabelReplaceSegment, string, error) {
	// Unrewritten first: everything already expressible stays exactly what
	// it was, and no reference is renumbered by a rewrite it did not need.
	// Only the shared-carrier ambiguity can be cleared by one, and any
	// other failure reproduces identically on the second pass, so the
	// error a caller sees is always the probed pass's.
	if segments, err := resolveSegments(repl, newCaptureGroups(regex, withoutCaptureProbes)); err == nil {
		return segments, "", nil
	}
	groups := newCaptureGroups(regex, withCaptureProbes)
	segments, err := resolveSegments(repl, groups)
	if err != nil {
		return nil, "", err
	}
	return segments, groups.probe.regex, nil
}

// captureProbeUse says whether a resolution may rewrite the regex to
// expose capture participation. The two passes are separate because a
// rewrite renumbers every group, so it must not happen where it buys
// nothing.
type captureProbeUse bool

const (
	withoutCaptureProbes captureProbeUse = false
	withCaptureProbes    captureProbeUse = true
)

// errTemplateStalled is raised by the replacement scanners' progress
// invariant. It names a defect in the step functions rather than anything
// about the template: no input reaches it, because every branch of
// replacementStep and emptyCapturesStep consumes at least one byte.
const errTemplateStalled = "replacement template scan made no progress"

func resolveSegments(repl string, groups captureGroups) ([]chplan.LabelReplaceSegment, error) {
	var segments []chplan.LabelReplaceSegment
	var literal strings.Builder
	flush := func() {
		if literal.Len() == 0 {
			return
		}
		segments = append(segments, chplan.LabelReplaceSegment{Literal: literal.String(), Group: chplan.NoCaptureGroup})
		literal.Reset()
	}

	// Step-based loop: each branch returns the number of input bytes it
	// consumed via `step`, and the for-iterator advances `i` by that
	// amount. Phrasing the loop this way (rather than using `continue` /
	// `break` inside an inner `switch`) makes every per-iteration choice
	// observable in the iterator clause — without it the gremlins
	// INVERT_LOOPCTRL operator could swap `continue` ↔ `break` inside
	// dead-end switch cases and the swap would be unkillable because no
	// statements ran between the keyword and the iterator step. See PR
	// #499 (the mutant-kill tests) and the follow-up PR that landed this
	// refactor for the full diagnosis.
	for i := 0; i < len(repl); {
		prev := i
		ref, step, err := replacementStep(&literal, repl, i, groups)
		if err != nil {
			return nil, err
		}
		if ref.group != unresolvedGroup {
			flush()
			segments = append(segments, chplan.LabelReplaceSegment{
				Group:          ref.group,
				Fallbacks:      ref.fallbacks,
				Probes:         ref.probes,
				NegativeProbes: ref.negativeProbes,
			})
		}
		i += step
		// Progress invariant. replacementStep's contract is at least one
		// byte per call, and the comment above says the iterator clause is
		// what makes that observable — but nothing enforced it. A template
		// is user-supplied, so a step that consumes nothing is an unbounded
		// `segments` append driven by untrusted text rather than a wrong
		// answer, and the loop never returns to report it.
		if i <= prev {
			return nil, fmt.Errorf("replacement %q: %s", repl, errTemplateStalled)
		}
	}
	flush()
	return segments, nil
}

// renderCHTemplate folds a decomposition back into a `replaceRegexpOne`
// substitution string. Every literal backslash is doubled so CH reads it
// as a literal rather than as the start of one of the `\N` backrefs
// spliced in alongside it; every capture reference becomes `\N`.
//
// Callers must have checked that no segment's group exceeds
// [maxCHBackref] — `\10` would be read by CH as group 1 followed by a
// literal `0`.
func renderCHTemplate(segments []chplan.LabelReplaceSegment) string {
	var b strings.Builder
	for _, seg := range segments {
		if seg.Group != chplan.NoCaptureGroup {
			b.WriteByte('\\')
			b.WriteString(strconv.Itoa(seg.Group))
			continue
		}
		for i := 0; i < len(seg.Literal); i++ {
			if seg.Literal[i] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(seg.Literal[i])
		}
	}
	return b.String()
}

// captureGroups resolves a replacement template's `$N` / `$name`
// references to capture-group indices for one regex.
type captureGroups struct {
	// count is the regex's capture-group count. A numbered reference
	// above it binds to nothing.
	count int
	// byName maps each capture-group name to every index carrying it.
	// Go's regexp permits the same name on several groups, so this is a
	// slice rather than a single index. Empty when the regex did not
	// compile, so every named reference then binds to nothing.
	byName map[string][]int
	// shapes reports, per capture-group index, what that group's position
	// in the regex's parse tree says about when it takes part in a match:
	// whether its subpattern can match the empty string, whether every
	// match must pass through it, and which alternation branches enclose
	// it. Together those decide whether a shared-name reference is
	// expressible — see [captureGroups.expressibleCarriers]. A group
	// missing from the map reads as nullable and conditional, so an
	// unparseable or unclassifiable regex keeps the rejection rather than
	// risking a wrong answer.
	shapes map[int]captureShape
	// probe is the regex rewritten to expose the participation of
	// carriers whose own capture cannot report it, together with the
	// index translation that rewrite forces. Its `regex` is empty when no
	// carrier needed a probe, which leaves every index below untouched.
	probe captureProbePlan
}

// newCaptureGroups compiles regex for its capture-group metadata.
//
// Compilation is best-effort: when Go's parser rejects the regex there
// is no group metadata to read, so every single-digit numbered backref
// is allowed through and CH's own parse stage surfaces the regex error
// to the client — the same fallback the pre-name-resolution version of
// this file used.
func newCaptureGroups(regex string, probes captureProbeUse) captureGroups {
	// Anchored to mirror the SQL emitter and reference Prometheus
	// (promql/functions.go: `"^(?s:" + regexStr + ")$"`), including the
	// non-capturing `(?s:...)` wrapper: without it, `^...$` binds only to
	// the first/last arm of a top-level alternation (alternation has
	// lower precedence than anchoring), and without the `s` flag `.`
	// would not match a newline. `(?s:...)` is non-capturing, so it
	// shifts no group index — the metadata read back is still the
	// unanchored regex's.
	anchored := anchorRegex(regex)
	compiled, err := regexp.Compile(anchored)
	if err != nil {
		return captureGroups{count: maxCHBackref}
	}
	g := captureGroups{
		count:  compiled.NumSubexp(),
		byName: map[string][]int{},
		shapes: captureShapes(anchored),
	}
	for i, name := range compiled.SubexpNames() {
		if name == "" {
			continue
		}
		g.byName[name] = append(g.byName[name], i)
	}
	if probes == withCaptureProbes {
		// A failed plan — including the empty request a pattern with no
		// shared name makes — leaves the zero value, which reads as "no
		// probes" everywhere below and keeps the rejection those carriers
		// had.
		g.probe, _ = planCaptureProbes(regex, g.carriersNeedingProbes())
	}
	return g
}

// carriersNeedingProbes lists every capture group that shares its name,
// survives the truncation, and can neither be cleared by non-nullability
// nor by exclusivity — the carriers for which the emitted search would
// otherwise have to guess. The list is the input to the regex rewrite;
// planning is separate from deciding, because one rewrite serves every
// shared name in the pattern at once.
//
// The walk over names is ordered as well as the result. The accumulated
// set already made the output order-independent, so this is defence
// rather than a fix: it keeps the traversal itself reproducible, which is
// what a reader stepping through a rewrite needs.
func (g captureGroups) carriersNeedingProbes() []int {
	names := make([]string, 0, len(g.byName))
	for name := range g.byName {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := map[int]bool{}
	for _, name := range names {
		carriers := g.byName[name]
		if len(carriers) < 2 {
			continue
		}
		for _, idx := range g.unclearedCarriers(carriers) {
			seen[idx] = true
		}
	}
	out := make([]int, 0, len(seen))
	for idx := range seen {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

// unclearedCarriers reports which of one name's carriers the static facts
// alone cannot clear, over the same truncated prefix
// [captureGroups.expressibleCarriers] searches.
func (g captureGroups) unclearedCarriers(carriers []int) []int {
	searched := carriers[:g.searchableEnd(carriers):g.searchableEnd(carriers)]
	var out []int
	for i, idx := range searched {
		shape, known := g.shapes[idx]
		if !known {
			continue
		}
		if !shape.nullable || g.exclusiveOfAll(shape, searched[i+1:]) {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// searchableEnd is how many of carriers the search may look through:
// everything up to and including the first one every match must pass
// through, since Go's own scan can never walk past that one either.
func (g captureGroups) searchableEnd(carriers []int) int {
	for i, idx := range carriers {
		if g.shapes[idx].unconditional {
			return i + 1
		}
	}
	return len(carriers)
}

// anchorRegex anchors a `label_replace` regex to a full-string match, the
// same way reference Prometheus does (`promql/functions.go`:
// `"^(?s:" + regexStr + ")$"`). The chsql emitter's `exprLabelReplace`
// must build the identical string for the `match(...)` / `extractGroups`
// arguments it emits — the two sites are read together whenever this
// changes.
//
// A bare `"^" + regex + "$"` gets two things wrong:
//
//   - Alternation binds looser than anchoring, so `^a|b$` parses as
//     `(^a)|(b$)`, not `^(a|b)$` — the anchors bind only to the
//     first/last arm of a top-level alternation, not the whole pattern.
//     The non-capturing `(?s:...)` wrapper gives the anchors a single
//     group to bind around, fixing that.
//   - Without the `s` flag, `.` does not match a newline; Prometheus's
//     anchoring always sets it, so a label_replace regex's `.` matches
//     `\n` too.
//
// `(?s:...)` is non-capturing, so it introduces no new capture group and
// shifts no existing group's index — the metadata callers read back
// (count, names, nullability) is still the unanchored regex's.
func anchorRegex(regex string) string {
	return "^(?s:" + regex + ")$"
}

// matchesEmpty reports whether re's language contains the empty string.
//
// This is the hinge the shared-capture-group-name translation rests on
// ([captureGroups.resolve]): a capture group whose subpattern CANNOT
// match the empty string captures at least one character whenever it
// takes part in a match, so ClickHouse's `extractGroups` — which reports
// "did not take part" and "took part, matched empty" identically, as `”`
// — tells the two apart after all, by non-emptiness.
//
// Deliberately conservative: any operator this switch does not classify
// falls through to `true` (nullable), which keeps today's rejection
// rather than emitting SQL whose answer might be wrong.
func matchesEmpty(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		// A parsed literal is normally non-empty; the zero-rune form is
		// the empty string and matches it.
		return len(re.Rune) == 0
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		// Every member of a character class is exactly one rune wide.
		return false
	case syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		// Zero-width: matches the empty string (subject, for the
		// assertions, to a condition on the surrounding text).
		return true
	case syntax.OpCapture:
		return matchesEmpty(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		// Zero repetitions is always in the language.
		return true
	case syntax.OpPlus:
		// At least one repetition, so nullable exactly when the body is.
		return matchesEmpty(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min == 0 {
			return true
		}
		return matchesEmpty(re.Sub[0])
	case syntax.OpConcat:
		// Every part must be able to contribute nothing.
		for _, sub := range re.Sub {
			if !matchesEmpty(sub) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		// One nullable branch is enough.
		for _, sub := range re.Sub {
			if matchesEmpty(sub) {
				return true
			}
		}
		return false
	}
	return true
}

// resolvedRef is where one template reference points in the regex.
type resolvedRef struct {
	// group is the capture-group index the reference resolves to, or
	// [unresolvedGroup] when it binds to nothing.
	group int
	// fallbacks holds the FURTHER capture-group indices the search may
	// look through when several groups carry the referenced name. It is
	// empty for every other reference shape. Together with `group` it is a
	// PREFIX of the like-named groups in regex order, not necessarily all
	// of them. The value contributed is the first of `group` followed by
	// `fallbacks` whose capture is non-empty — see
	// [captureGroups.expressibleCarriers] for why that reproduces Go's
	// choice exactly.
	fallbacks []int
	// probes holds, for each searched carrier — `group` followed by
	// `fallbacks`, so one element apiece and in that order — the capture
	// group whose NON-EMPTINESS reports that carrier took part in the
	// match. It is empty when no carrier needed one, in which case every
	// carrier's own capture reports its participation and the search is
	// the plain non-empty one. See [planCaptureProbes].
	probes         []int
	negativeProbes [][]int
}

// resolve maps one extracted reference to the capture group(s) it points
// at, or to [unresolvedGroup] when the reference binds to nothing. It
// errors on the one reference shape no ClickHouse form can express.
//
// A group index above CH's `\9` ceiling is NOT such a shape: it is
// returned like any other, and [ReplacementToCH] switches the whole
// template to the ceiling-free `extractGroups` decomposition.
//
// # Names several groups share
//
// Go's regexp permits several capture groups to share one name, and
// ExpandString then picks the first of them that TOOK PART in the row's
// match (its start offset is not -1), regardless of which alternation
// branch matched. Participation is not directly observable from SQL:
// `extractGroups` reports a group that did not take part and a group that
// took part matching the empty string identically, as `”` (verified
// against ClickHouse: `extractGroups('b', '^(a)|(b)$')` → `[”, 'b']`,
// and `extractGroups(”, '^(x?)$')` → `[”]`).
//
// Note what separates those two ClickHouse results: only the second has a
// group whose subpattern can match the empty string. That is the whole
// hinge. For a group whose subpattern CANNOT match the empty string,
// "took part" and "captured something non-empty" are the same predicate:
//
//   - a group that took part matched a string in its subpattern's
//     language, and the empty string is not in that language, so the
//     capture is non-empty;
//   - a group that did not take part reports `”`.
//
// So a non-nullable carrier can never be the one the two searches
// disagree about, and when EVERY carrier is non-nullable "the first
// carrier with a non-empty capture" is exactly Go's "the first carrier
// that took part". The reference is then expressible as
// `arrayFirst(x -> x != ”, [g[i], g[j], …])` over the same
// `extractGroups` decomposition the ceiling-free form already uses.
// (`arrayFirst` yields `”` when no element qualifies, which is what
// ExpandString substitutes when no carrier took part.)
//
// Nullability alone is not the boundary, though, because the two searches
// disagree on one event only: the first carrier that takes part captures
// the EMPTY string while a LATER carrier in the same match captures text.
// [captureGroups.expressibleCarriers] rules that event out from the
// carriers' positions in the parse tree as well as from their
// nullability, and narrows the searched list accordingly; it is the
// authority on which shapes survive, and its file documents the three
// facts it reasons from. What stays rejected is a nullable carrier that a
// match can skip while a later carrier that can co-occur with it captures
// text — issue #1956 tracks that residue.
func (g captureGroups) resolve(ref templateRef) (resolvedRef, error) {
	out := resolvedRef{group: unresolvedGroup}
	switch carriers := g.byName[ref.name]; {
	case ref.num != unresolvedGroup:
		// A numbered reference past the regex's group count binds to
		// nothing, which is the empty string — not an error.
		if ref.num <= g.count {
			out.group = g.probe.probedIndex(ref.num)
		}
	case len(carriers) > 1:
		searched, probes, negativeProbes, ambiguous, ok := g.expressibleCarriers(carriers)
		if !ok {
			return resolvedRef{}, fmt.Errorf(
				"references capture-group name %q, which %d capture groups share, "+
					"and cerberus cannot rule out a match in which capture group %d "+
					"takes part capturing the empty string while a later carrier "+
					"captures text — ClickHouse's extractGroups reports a group that "+
					"matched empty and a group that took no part in the match "+
					"identically, so which one Go's expansion would pick would not be "+
					"observable from SQL",
				ref.name, len(carriers), ambiguous,
			)
		}
		out.group, out.fallbacks, out.probes, out.negativeProbes = searched[0], searched[1:], probes, negativeProbes
	case len(carriers) == 1:
		out.group = g.probe.probedIndex(carriers[0])
	}
	return out, nil
}

// expressibleCarriers narrows carriers — every capture group sharing one
// name, in regex order — to the prefix an `arrayFirst` non-empty search
// may look through, or names the carrier whose participation no
// ClickHouse expression can observe.
//
// Two steps, in this order:
//
//  1. TRUNCATE at the first carrier every match must pass through. Go's
//     scan for the first participating carrier can never walk past such a
//     group, so every carrier after it is unreachable and its nullability
//     cannot matter. Truncating to a single element is the common case
//     for a shape like `(?P<d>a?)x(?P<d>b)`, where the answer is simply
//     group 1's capture.
//  2. CLEAR each surviving carrier. A carrier is clear when it cannot
//     match the empty string, or when it is mutually exclusive with every
//     later surviving carrier — in which case an empty capture by it means
//     none of them took part either, so both searches yield `”`. The last
//     surviving carrier is cleared by the second test vacuously, there
//     being nothing after it for the search to wrongly skip ahead to.
//
// The exclusivity test is over EVERY later surviving carrier, not just the
// next one: with three carriers, an earlier one can be exclusive of the
// second and still co-occur with the third, and it is the third that the
// search would wrongly skip ahead to.
//
// A carrier the parse-tree walk could not classify has no shape recorded,
// which reads as nullable and exclusive of nothing, so an unrecognised
// regex keeps the rejection.
func (g captureGroups) expressibleCarriers(carriers []int) ([]int, []int, [][]int, int, bool) {
	end := g.searchableEnd(carriers)
	searched := carriers[:end:end]

	probes := make([]int, len(searched))
	negativeProbes := make([][]int, len(searched))
	anyProbe := false
	anyNegativeProbe := false
	for i, idx := range searched {
		shape, known := g.shapes[idx]
		if !known {
			return nil, nil, nil, idx, false
		}
		// By default a carrier reports its own participation, which is
		// what makes the emitted search the plain non-empty one.
		probes[i] = g.probe.probedIndex(idx)
		if !shape.nullable || g.exclusiveOfAll(shape, searched[i+1:]) {
			continue
		}
		at, probed := g.probe.probeOf[idx]
		if probed {
			probes[i] = at
			anyProbe = true
			continue
		}
		if negative := g.probe.negativeOf[idx]; len(negative) > 0 {
			negativeProbes[i] = negative
			anyNegativeProbe = true
			continue
		}
		return nil, nil, nil, idx, false
	}
	if !anyProbe {
		probes = nil
	}
	if !anyNegativeProbe {
		negativeProbes = nil
	}

	// Every index leaves in the rewritten regex's numbering, which is the
	// one the emitter's `extractGroups` reads. Without a rewrite the
	// translation is the identity.
	out := make([]int, len(searched))
	for i, idx := range searched {
		out[i] = g.probe.probedIndex(idx)
	}
	return out, probes, negativeProbes, 0, true
}

// exclusiveOfAll reports whether shape's group can never take part in the
// same match as any of the given later carriers. An empty `later` makes
// this vacuously true, which is what clears the last surviving carrier.
func (g captureGroups) exclusiveOfAll(shape captureShape, later []int) bool {
	for _, idx := range later {
		other, known := g.shapes[idx]
		if !known || !mutuallyExclusive(shape, other) {
			return false
		}
	}
	return true
}

// replacementStep handles a single dispatch step of
// [ReplacementSegments] at offset `i` of `repl`. Literal text is appended
// to `b`; a resolved capture reference is returned as a [resolvedRef]
// (whose group is [unresolvedGroup] otherwise, which covers both "this
// step was literal text" and "this reference binds to nothing"). The
// second return is the number of input bytes consumed — always >= 1 on
// the success path, so the outer loop always makes progress.
//
// Splitting this out of the loop body keeps the per-iteration consumed
// count observable in the caller's iterator clause, so the gremlins
// INVERT_LOOPCTRL operator can't swap `continue` ↔ `break` and produce
// an equivalent mutant — the dispatch keywords don't live in a `for`
// scope at all here.
func replacementStep(b *strings.Builder, repl string, i int, groups captureGroups) (resolvedRef, int, error) {
	unbound := resolvedRef{group: unresolvedGroup}
	c := repl[i]
	if c != '$' {
		// Backslashes are kept verbatim here; each output form escapes
		// them the way its own syntax requires.
		b.WriteByte(c)
		return unbound, 1, nil
	}
	// Lone `$` at end of string — ExpandString emits it verbatim.
	if i+1 >= len(repl) {
		b.WriteByte('$')
		return unbound, 1, nil
	}
	if repl[i+1] == '$' {
		// `$$` → literal `$`.
		b.WriteByte('$')
		return unbound, 2, nil
	}
	ref, ok := extractRef(repl[i+1:])
	if !ok {
		// Not a reference at all (`$-`, `${unclosed`) — ExpandString
		// emits the `$` verbatim and resumes at the next byte.
		b.WriteByte('$')
		return unbound, 1, nil
	}
	resolved, err := groups.resolve(ref)
	if err != nil {
		return resolvedRef{}, 0, fmt.Errorf("replacement %q %w", repl, err)
	}
	// An unresolved reference binds to nothing; ExpandString substitutes
	// the empty string for it, so it contributes no segment either.
	return resolved, 1 + ref.width, nil
}

// templateRef is one `$…` reference split off the front of a Go
// replacement template.
type templateRef struct {
	// name is the reference text with any braces stripped: `1`, `10`,
	// `svc`.
	name string
	// num is the capture-group index when name is a plain decimal index,
	// and [unresolvedGroup] when the reference is a NAME instead.
	num int
	// width is how many bytes of the template the reference occupies,
	// not counting the leading `$`.
	width int
}

// extractRef mirrors the unexported `extract` helper in Go's regexp
// package, which is what `Regexp.ExpandString` — the engine both
// PromQL's and LogQL's `label_replace` run their replacement through —
// uses to split a `$…` reference off the front of a template.
//
// Reproducing it rather than pattern-matching a digit is what makes the
// translation agree with the reference implementations on the shapes
// that a hand-rolled scan gets wrong: `$10` is capture group TEN (Go
// takes the LONGEST run of name characters, not one digit), `$1x` is a
// reference to a group NAMED `1x` rather than group 1 followed by a
// literal `x`, and `$01` is likewise a name because Go rejects leading
// zeros as indices.
//
// str is the template starting immediately after the `$`. The reported
// width excludes that `$`.
func extractRef(str string) (templateRef, bool) {
	rest := str
	braced := strings.HasPrefix(rest, "{")
	if braced {
		rest = rest[1:]
	}
	// A name runs to the first byte that is not a letter, digit or `_`.
	end := 0
	for end < len(rest) {
		r, size := utf8.DecodeRuneInString(rest[end:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
		end += size
	}
	if end == 0 {
		// An empty name is not a reference.
		return templateRef{}, false
	}
	name := rest[:end]
	width := end
	if braced {
		if end >= len(rest) || rest[end] != '}' {
			// Missing closing brace — not a reference.
			return templateRef{}, false
		}
		// Both braces are part of the reference's width.
		width += len("{}")
	}
	return templateRef{name: name, num: refNum(name), width: width}, true
}

// refNum returns the capture-group index a reference name denotes, or
// [unresolvedGroup] when the name is not a plain decimal index and must
// be looked up as a capture-group NAME instead. Mirrors the numeric half
// of Go's `extract`: a leading zero on a multi-character name, any
// non-digit byte, or a value too large to hold demotes the reference to
// a name lookup.
func refNum(name string) int {
	if name[0] == '0' && len(name) > 1 {
		return unresolvedGroup
	}
	// The name charset is letters / digits / `_`, so Atoi can only fail
	// on a non-digit byte or on overflow — both of which Go's own parse
	// loop also treats as "this is a name".
	num, err := strconv.Atoi(name)
	if err != nil {
		return unresolvedGroup
	}
	return num
}

// EmptyCapturesReplacement returns the result of substituting Go's
// regex `ExpandString` template `repl` against an EMPTY source string
// that matched the regex via a match where every capture group binds
// to "". This matches the semantics of `label_replace(m, dst, repl,
// src, regex)` when `src` is absent from the input series labels (Prom
// reads missing labels as the empty string) AND the regex matches that
// empty string — e.g. `(.*)`, `.*`, `^()$` all match `""` with
// every group capturing `""`.
//
// Why we need a separate path:
//
//	CH ≤ 24.8's `replaceRegexpOne('', '^(.*)$', 'value-\1')` returns
//	`""` (the empty input is passed through verbatim, regardless of
//	the replacement template), instead of the spec-correct `"value-"`.
//	The outer `mapFilter((k, v) -> v != '', …)` then drops the dst
//	label entirely, diverging from reference Prom which emits
//	`dst="value-"`. CH ≥ 25.8 honours the replacement on empty inputs,
//	and the cerberus deployment lane now targets CH 25.8 — but the
//	short-circuit stays load-bearing: it is forward-safe (collapses to
//	the same spec-correct value on 25.8) and keeps the emit identical
//	while the compatibility reference backend moves in lock-step. We
//	patch the divergence at SQL build time by pre-computing the
//	empty-captures result and using it as a short-circuit when the
//	source value is empty at row time.
//
// Substitution rules (the same reference splitter `ReplacementToCH`
// uses, but every reference resolves to the empty string instead of to
// CH's `\N` form):
//
//   - `$$` → literal `$`
//   - `$N` / `${N}` / `$name` / `${name}` → empty string. Every
//     reference ExpandString recognises contributes "" here: a group
//     that took part in the empty match expanded to "", and a group
//     that bound to nothing (index past the group count, unknown name)
//     expands to "" as well. No regex is needed to tell the two apart
//     because the answer is the same either way.
//   - A `$` that starts no reference (end of string, `$-`,
//     `${unclosed`) → preserved verbatim, as ExpandString does.
func EmptyCapturesReplacement(repl string) string {
	var b strings.Builder
	b.Grow(len(repl))
	// Step-based loop — see ReplacementToCH for the same rationale: the
	// dispatch keywords moved into a helper so the gremlins
	// INVERT_LOOPCTRL operator has no `continue`/`break` to swap inside
	// a dead-end switch case.
	for i := 0; i < len(repl); {
		prev := i
		i += emptyCapturesStep(&b, repl, i)
		// Progress invariant — see resolveSegments. This function has no
		// error channel, so a stalled step falls back to the rule it already
		// applies to every `$` it cannot read as a reference: pass the rest
		// of the template through verbatim.
		if i <= prev {
			b.WriteString(repl[prev:])
			return b.String()
		}
	}
	return b.String()
}

// emptyCapturesStep handles a single dispatch step of
// EmptyCapturesReplacement at offset `i` of `repl`. It writes the
// translated bytes to `b` and returns the number of input bytes it
// consumed (always >= 1).
//
// Mirrors replacementStep but resolves numbered captures to the empty
// string instead of CH's `\N` form. See replacementStep for the
// mutation-testing rationale behind the extraction.
func emptyCapturesStep(b *strings.Builder, repl string, i int) int {
	c := repl[i]
	if c != '$' {
		b.WriteByte(c)
		return 1
	}
	// Lone `$` at end of string — preserve.
	if i+1 >= len(repl) {
		b.WriteByte('$')
		return 1
	}
	if repl[i+1] == '$' {
		// `$$` → literal `$`.
		b.WriteByte('$')
		return 2
	}
	ref, ok := extractRef(repl[i+1:])
	if !ok {
		// Not a reference — ExpandString emits the `$` verbatim and
		// resumes at the next byte.
		b.WriteByte('$')
		return 1
	}
	// Every recognised reference expands to "" against an empty match,
	// so there is nothing to write — just step over the reference.
	return 1 + ref.width
}
