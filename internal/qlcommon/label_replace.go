// Package qlcommon hosts small, lowering-layer helpers shared across
// the PromQL / LogQL / TraceQL heads. Functions here translate
// upstream-QL semantics into shapes the shared chplan IR + the chsql
// emitter expect, so each language's lowering can stay focused on its
// own AST.
package qlcommon

import (
	"regexp"
	"strings"
)

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
// Translation rules implemented here (single-digit captures only — the
// upstream label_replace functions don't constrain the index but CH's
// backref syntax tops out at `\9`; multi-digit / named captures are
// not used by any test or compatibility fixture and would need a
// separate emit path):
//
//   - Pre-escape every existing `\` in the input to `\\`, so any
//     literal backslash in the QL template survives as a literal
//     backslash in CH (and is not re-interpreted as a CH backref by
//     the digits we're about to introduce).
//   - `$$` → `$` (literal dollar).
//   - `$N` for a single ASCII digit (0-9) → `\N`.
//   - `${N}` for a single ASCII digit (0-9) → `\N`.
//   - Any other `$<x>` (including bare `$` at end-of-string, `$<letter>`,
//     `${name}`, `$10` etc.) is preserved verbatim so we don't silently
//     mistranslate a shape we don't fully support.
//
// regex is the regex string the replacement will be applied against;
// it's used to count capture groups so out-of-range backrefs can be
// rewritten to the empty string. CH validates `replaceRegexpOne`'s
// substitution string against the regex's capture-group count at SQL-
// parse time and rejects backrefs that exceed it (Code 36, BAD_ARGUMENTS)
// — even on rows where match() short-circuits the if-branch that owns
// the replaceRegexpOne call. The upstream QL semantics silently
// substitute the empty string for missing groups (Go's ExpandString
// semantics); replacing the backref with "" preserves that observable
// behaviour on the (unreachable) hot path and unblocks the SQL parser
// on the (very-much-reachable) cold path where the regex doesn't match
// anything.
func ReplacementToCH(repl, regex string) string {
	// First pass: double every literal backslash so CH sees them as
	// "literal backslash" (`\\`) rather than the start of its own
	// backref escape sequence after we splice `\N` in below.
	escaped := strings.ReplaceAll(repl, `\`, `\\`)

	// Count capture groups in the anchored regex (the same anchoring
	// the SQL emitter applies). Best-effort: if Go's parser can't
	// compile the regex, fall back to allowing every single-digit
	// backref — the emit-path will surface the compile error to the
	// client via CH's own parse stage.
	const maxBackref = 9
	allowed := maxBackref
	if compiled, err := regexp.Compile("^" + regex + "$"); err == nil {
		allowed = compiled.NumSubexp()
	}

	var b strings.Builder
	b.Grow(len(escaped))
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
	for i := 0; i < len(escaped); {
		step := replacementStep(&b, escaped, i, allowed)
		i += step
	}
	return b.String()
}

// replacementStep handles a single dispatch step of ReplacementToCH at
// offset `i` of `escaped`. It writes the translated bytes to `b` and
// returns the number of input bytes it consumed (always >= 1, so the
// outer loop always makes progress).
//
// Splitting this out of the loop body keeps the per-iteration consumed
// count observable in the caller's iterator clause, so the gremlins
// INVERT_LOOPCTRL operator can't swap `continue` ↔ `break` and produce
// an equivalent mutant — the dispatch keywords don't live in a `for`
// scope at all here.
func replacementStep(b *strings.Builder, escaped string, i, allowed int) int {
	c := escaped[i]
	if c != '$' {
		b.WriteByte(c)
		return 1
	}
	// Lone `$` at end of string — preserve.
	if i+1 >= len(escaped) {
		b.WriteByte('$')
		return 1
	}
	next := escaped[i+1]
	switch {
	case next == '$':
		// `$$` → literal `$`.
		b.WriteByte('$')
		return 2
	case next >= '0' && next <= '9':
		// `$N` → `\N` (single digit only — `$10` is preserved
		// verbatim per upstream Go regexp semantics, but CH
		// has no `\10`, so we'd mistranslate either way; preserving
		// keeps the failure visible rather than silently wrong).
		if i+2 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' {
			b.WriteByte('$')
			return 1
		}
		n := int(next - '0')
		// `\0` references the whole match and is always valid; for
		// numbered captures, only emit `\N` if the regex actually
		// has N capture groups. Out-of-range refs are dropped so
		// CH's substitution validator stays happy.
		if n == 0 || n <= allowed {
			b.WriteByte('\\')
			b.WriteByte(next)
		}
		return 2
	case next == '{':
		// `${N}` (single digit) → `\N`. Anything else (named
		// captures, multi-digit indices) is preserved verbatim.
		if i+3 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' && escaped[i+3] == '}' {
			n := int(escaped[i+2] - '0')
			if n == 0 || n <= allowed {
				b.WriteByte('\\')
				b.WriteByte(escaped[i+2])
			}
			return 4
		}
		b.WriteByte('$')
		return 1
	default:
		// `$<letter>` etc. — preserve verbatim.
		b.WriteByte('$')
		return 1
	}
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
//	but the cerberus deployment lane targets CH 24.8 (the OTel
//	collector's pinned LTS), so we patch the divergence at SQL build
//	time by pre-computing the empty-captures result and using it as a
//	short-circuit when the source value is empty at row time.
//
// Substitution rules (mirror `ReplacementToCH` but resolve each
// backref to the empty string instead of CH's `\N` form):
//
//   - `$$`                → literal `$`
//   - `$N` / `${N}`       → empty string (the N-th capture binds to ""
//     when the full match was "")
//   - Any other `$<x>`    → preserved verbatim (named groups,
//     multi-digit indices — same opt-out as `ReplacementToCH`)
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
	next := repl[i+1]
	switch {
	case next == '$':
		// `$$` → literal `$`.
		b.WriteByte('$')
		return 2
	case next >= '0' && next <= '9':
		// `$N` → "" (capture N is empty for an empty-string match).
		// Two-digit `$10`+ is preserved verbatim — `ReplacementToCH`
		// makes the same opt-out, so the regex compile / CH parse
		// error surfaces consistently.
		if i+2 < len(repl) && repl[i+2] >= '0' && repl[i+2] <= '9' {
			b.WriteByte('$')
			return 1
		}
		// Single-digit numbered capture → empty. Skip past the digit.
		return 2
	case next == '{':
		// `${N}` (single digit) → "". Anything else (named captures,
		// multi-digit indices) is preserved verbatim — same opt-out
		// as `ReplacementToCH`.
		if i+3 < len(repl) && repl[i+2] >= '0' && repl[i+2] <= '9' && repl[i+3] == '}' {
			return 4
		}
		b.WriteByte('$')
		return 1
	default:
		// `$<letter>` etc. — preserve verbatim.
		b.WriteByte('$')
		return 1
	}
}
