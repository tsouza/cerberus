package prom

import "strings"

// stringState tracks which (if any) string literal the dotted-selector
// rewriter is currently inside. Package-scoped so the small helpers
// below can share it with normalizeDottedSelectors.
type stringState int

const (
	outside stringState = iota
	inDouble
	inSingle
	inBacktick
)

// normalizeDottedSelectors rewrites OTel-style dotted metric names in
// selector position to the explicit `{__name__="..."}` form so the
// PromQL parser — which only accepts ASCII identifiers matching
// `[a-zA-Z_:][a-zA-Z0-9_:]*` — can handle them.
//
// Example: `rate(http.server.request.duration[5m])` becomes
// `rate({__name__="http.server.request.duration"}[5m])`.
//
// Why this is needed: OpenTelemetry's semantic conventions emit dotted
// metric names (`http.server.request.duration`, `db.client.duration`,
// `system.cpu.utilization`, etc.). Reference Prometheus has no support
// for dotted identifiers in selector position; users either rewrite to
// `{__name__="..."}` manually or hit a 400 parse error. Grafana's
// metric picker happily round-trips dotted names through cerberus's
// `/api/v1/label/__name__/values` endpoint, so without this rewrite a
// user can SEE the dotted metric in the picker, click it, and watch
// the query fail to parse.
//
// The rewrite walks the query as runes, tracking:
//   - whether we're inside `"…"` / `'…'` / “ `…` “ strings (skip rewrites
//     so a string literal `"a.b.c"` isn't molested)
//   - whether the current rune-position is a valid identifier-start
//     position (after start-of-string, whitespace, or any of the
//     selector-context delimiters `({,=~!|+-*/<>` etc.)
//
// At an identifier-start position we greedy-consume `[a-zA-Z_]([a-zA-Z0-9_]|\.)*`.
// If the consumed token contains at least one `.` (the load-bearing
// distinguisher between OTel-dotted names and PromQL-valid names like
// `http_server_request_duration`), we emit `{__name__="<token>"}`;
// otherwise we leave the token unchanged.
//
// We do NOT try to second-guess whether the dotted name corresponds to
// a real metric; the resulting selector either matches rows in CH or
// it doesn't (just like any other selector). False positives — e.g. a
// dotted token that was meant as a function name — surface as a clean
// "metric not found" empty result, not a parse error.
//
// Known limitations:
//   - PromQL function names that contain dots are not currently a thing;
//     no false positive there.
//   - PromQL label-matcher operators (`=`, `!=`, `=~`, `!~`) are
//     skipped during string-state tracking via the surrounding
//     identifier-position predicate.
//   - Numeric literals like `1.5` are not identifier-start positions
//     (digit cannot start an identifier), so they pass through unchanged.
func normalizeDottedSelectors(q string) string {
	if !strings.ContainsRune(q, '.') {
		return q
	}
	var out strings.Builder
	out.Grow(len(q) + 32)

	state := outside
	identStart := true // start-of-input is identifier-position
	i := 0
	for i < len(q) {
		ch := q[i]
		if state != outside {
			i, state = advanceInString(&out, q, i, state)
			identStart = false
			continue
		}

		if next, ok := openString(ch); ok {
			state = next
			out.WriteByte(ch)
			i++
			identStart = false
			continue
		}

		if identStart && isIdentStart(ch) {
			// Greedy-consume an identifier-with-dots token.
			j := i + 1
			for j < len(q) && (isIdentCont(q[j]) || q[j] == '.') {
				j++
			}
			tok := q[i:j]
			// Trailing dot is suspicious (e.g. a stray `.` at end of
			// token) — strip it from the rewrite candidate and write
			// the leftover dot back into the stream so downstream
			// tokens still see it.
			trailingDot := strings.HasSuffix(tok, ".")
			if trailingDot {
				tok = strings.TrimRight(tok, ".")
			}
			if strings.Contains(tok, ".") {
				i = emitDottedSelector(&out, q, tok, j, trailingDot)
			} else {
				out.WriteString(tok)
				if trailingDot {
					// Emit the dot that we stripped so the parser
					// still sees the surrounding tokens (rare path —
					// defensive).
					out.WriteByte('.')
				}
				i = j
			}
			identStart = false
			continue
		}

		// Default: emit the rune and update identStart state.
		out.WriteByte(ch)
		identStart = isIdentBoundary(ch)
		i++
	}
	return out.String()
}

// emitDottedSelector writes the `{__name__="<tok>"…}` selector form
// for a confirmed dotted token, folding into an adjacent
// `{label=…}` group when present so the parser sees a single,
// well-formed selector instead of two adjacent ones (which is a
// parse error upstream — `{__name__="x"}{job="api"}` →
// `unexpected "{"`).
//
// Return value is the new `i` cursor: the byte index in `q` to resume
// scanning from. `j` is the position just past the consumed token (or
// just past the consumed trailing dot, when one was present).
//
// trailingDot indicates the caller stripped a trailing `.` off `tok`;
// when set, we emit the bare-form selector + the dot back into the
// stream and skip the fold-into-`{` shortcut (a stray dot between the
// token and `{` means the input is already malformed; the parser
// surfaces a clean error from there).
func emitDottedSelector(out *strings.Builder, q, tok string, j int, trailingDot bool) int {
	if trailingDot {
		out.WriteString(`{__name__="`)
		out.WriteString(tok)
		out.WriteString(`"}`)
		out.WriteByte('.')
		return j
	}
	// Look ahead for an adjacent `{…}` label-matcher group.
	if j < len(q) && q[j] == '{' {
		if j+1 < len(q) && q[j+1] == '}' {
			// Empty `{}` — collapse to a single matcher group.
			out.WriteString(`{__name__="`)
			out.WriteString(tok)
			out.WriteString(`"}`)
			return j + 2
		}
		// Populated `{...}` — splice `__name__="<tok>",` into it.
		out.WriteString(`{__name__="`)
		out.WriteString(tok)
		out.WriteString(`",`)
		return j + 1 // consume the leading `{`
	}
	// Bare form — no adjacent matcher group.
	out.WriteString(`{__name__="`)
	out.WriteString(tok)
	out.WriteString(`"}`)
	return j
}

// openString reports whether `ch` opens a string literal and, if so,
// the string state we enter. Returned `ok=false` means `ch` is not a
// string-opening delimiter.
func openString(ch byte) (stringState, bool) {
	switch ch {
	case '"':
		return inDouble, true
	case '\'':
		return inSingle, true
	case '`':
		return inBacktick, true
	}
	return outside, false
}

// advanceInString copies one rune (or one escape pair) from `q` at
// position `i` into `out`, updating the string state when the closing
// delimiter is seen. Backticked strings do not honour `\` escapes (raw
// strings); double / single quoted strings do.
func advanceInString(out *strings.Builder, q string, i int, state stringState) (int, stringState) {
	ch := q[i]
	out.WriteByte(ch)
	if state != inBacktick && ch == '\\' && i+1 < len(q) {
		out.WriteByte(q[i+1])
		return i + 2, state
	}
	switch {
	case state == inDouble && ch == '"',
		state == inSingle && ch == '\'',
		state == inBacktick && ch == '`':
		state = outside
	}
	return i + 1, state
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == ':'
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// isIdentBoundary reports whether the previous-rune state means the
// next rune (if a letter / underscore) starts a new identifier — i.e.
// we are AFTER whitespace or a selector-context delimiter. Carefully
// excludes `.` so `foo.bar` isn't split into two identifier-position
// tokens (the rewrite needs to see the whole dotted name in one shot).
func isIdentBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r',
		'(', '{', '[', ',',
		'+', '-', '*', '/', '%', '^',
		'=', '<', '>', '!', '|', '&', '@':
		return true
	}
	return false
}
