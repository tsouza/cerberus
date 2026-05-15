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
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		// Lone `$` at end of string — preserve.
		if i+1 >= len(escaped) {
			b.WriteByte('$')
			continue
		}
		next := escaped[i+1]
		switch {
		case next == '$':
			// `$$` → literal `$`.
			b.WriteByte('$')
			i++
		case next >= '0' && next <= '9':
			// `$N` → `\N` (single digit only — `$10` is preserved
			// verbatim per upstream Go regexp semantics, but CH
			// has no `\10`, so we'd mistranslate either way; preserving
			// keeps the failure visible rather than silently wrong).
			if i+2 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' {
				b.WriteByte('$')
				continue
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
			i++
		case next == '{':
			// `${N}` (single digit) → `\N`. Anything else (named
			// captures, multi-digit indices) is preserved verbatim.
			if i+3 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' && escaped[i+3] == '}' {
				n := int(escaped[i+2] - '0')
				if n == 0 || n <= allowed {
					b.WriteByte('\\')
					b.WriteByte(escaped[i+2])
				}
				i += 3
				continue
			}
			b.WriteByte('$')
		default:
			// `$<letter>` etc. — preserve verbatim.
			b.WriteByte('$')
		}
	}
	return b.String()
}
