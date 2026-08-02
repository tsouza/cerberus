// Package qlcommon hosts small, lowering-layer helpers shared across
// the PromQL / LogQL / TraceQL heads. Functions here translate
// upstream-QL semantics into shapes the shared chplan IR + the chsql
// emitter expect, so each language's lowering can stay focused on its
// own AST.
package qlcommon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxCHBackref is the highest capture-group index ClickHouse's
// `replaceRegexpOne` substitution syntax can address: `\0` (the whole
// match) through `\9`. Go's replacement templates have no such ceiling,
// so a reference above this is the one shape the translation below
// cannot express and rejects instead.
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
//     → `\N`, provided the regex has at least N capture groups.
//   - `$name` / `${name}` → `\N` for the index of the capture group
//     that carries that name.
//   - A reference that binds to nothing — an index past the regex's
//     group count, or a name no group carries — expands to the empty
//     string, exactly as ExpandString does.
//   - A `$` that starts no reference at all (end of string, `$-`,
//     `${unclosed`) is emitted verbatim, again as ExpandString does.
//
// The single shape with no faithful translation is a reference to a
// capture group that EXISTS but sits above CH's `\9` substitution
// ceiling; that returns an error so the query is rejected loudly rather
// than answered with a wrong label.
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
func ReplacementToCH(repl, regex string) (string, error) {
	groups := newCaptureGroups(regex)

	var b strings.Builder
	b.Grow(len(repl))
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
		step, err := replacementStep(&b, repl, i, groups)
		if err != nil {
			return "", err
		}
		i += step
	}
	return b.String(), nil
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
}

// newCaptureGroups compiles regex for its capture-group metadata.
//
// Compilation is best-effort: when Go's parser rejects the regex there
// is no group metadata to read, so every single-digit numbered backref
// is allowed through and CH's own parse stage surfaces the regex error
// to the client — the same fallback the pre-name-resolution version of
// this file used.
func newCaptureGroups(regex string) captureGroups {
	// Anchored to mirror the SQL emitter. Anchoring shifts no group
	// index, so the metadata read back is the unanchored regex's.
	compiled, err := regexp.Compile("^" + regex + "$")
	if err != nil {
		return captureGroups{count: maxCHBackref}
	}
	g := captureGroups{count: compiled.NumSubexp(), byName: map[string][]int{}}
	for i, name := range compiled.SubexpNames() {
		if name == "" {
			continue
		}
		g.byName[name] = append(g.byName[name], i)
	}
	return g
}

// resolve maps one extracted reference to its capture-group index, or
// [unresolvedGroup] when the reference binds to nothing. It errors on
// the references CH's fixed `\N` substitution cannot express.
func (g captureGroups) resolve(ref templateRef) (int, error) {
	idx := unresolvedGroup
	switch carriers := g.byName[ref.name]; {
	case ref.num != unresolvedGroup:
		// A numbered reference past the regex's group count binds to
		// nothing, which is the empty string — not an error.
		if ref.num <= g.count {
			idx = ref.num
		}
	case len(carriers) > 1:
		// Go's regexp permits several groups to share one name, and
		// ExpandString then picks the first of them that TOOK PART in
		// the row's match. That choice varies row by row; a CH `\N`
		// backref is fixed when the SQL is built and cannot follow it.
		return 0, fmt.Errorf(
			"references capture-group name %q, which %d capture groups share — "+
				"ClickHouse's `\\N` substitution cannot pick between them per row",
			ref.name, len(carriers),
		)
	case len(carriers) == 1:
		idx = carriers[0]
	}
	if idx > maxCHBackref {
		return 0, fmt.Errorf(
			"references capture group %d, above ClickHouse's `\\%d` substitution ceiling",
			idx, maxCHBackref,
		)
	}
	return idx, nil
}

// replacementStep handles a single dispatch step of ReplacementToCH at
// offset `i` of `repl`. It writes the translated bytes to `b` and
// returns the number of input bytes it consumed (always >= 1 on the
// success path, so the outer loop always makes progress).
//
// Splitting this out of the loop body keeps the per-iteration consumed
// count observable in the caller's iterator clause, so the gremlins
// INVERT_LOOPCTRL operator can't swap `continue` ↔ `break` and produce
// an equivalent mutant — the dispatch keywords don't live in a `for`
// scope at all here.
func replacementStep(b *strings.Builder, repl string, i int, groups captureGroups) (int, error) {
	c := repl[i]
	if c != '$' {
		if c == '\\' {
			// Double the literal backslash so CH reads it as a literal
			// rather than as the start of a `\N` backref.
			b.WriteString(`\\`)
			return 1, nil
		}
		b.WriteByte(c)
		return 1, nil
	}
	// Lone `$` at end of string — ExpandString emits it verbatim.
	if i+1 >= len(repl) {
		b.WriteByte('$')
		return 1, nil
	}
	if repl[i+1] == '$' {
		// `$$` → literal `$`.
		b.WriteByte('$')
		return 2, nil
	}
	ref, ok := extractRef(repl[i+1:])
	if !ok {
		// Not a reference at all (`$-`, `${unclosed`) — ExpandString
		// emits the `$` verbatim and resumes at the next byte.
		b.WriteByte('$')
		return 1, nil
	}
	idx, err := groups.resolve(ref)
	if err != nil {
		return 0, fmt.Errorf("replacement %q %w", repl, err)
	}
	if idx != unresolvedGroup {
		b.WriteByte('\\')
		b.WriteString(strconv.Itoa(idx))
	}
	// An unresolved reference binds to nothing; ExpandString substitutes
	// the empty string for it, so there is nothing to write.
	return 1 + ref.width, nil
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
		step := emptyCapturesStep(&b, repl, i)
		i += step
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
