package prom

import "strings"

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

	type stringState int
	const (
		outside stringState = iota
		inDouble
		inSingle
		inBacktick
	)

	state := outside
	identStart := true // start-of-input is identifier-position
	i := 0
	for i < len(q) {
		ch := q[i]
		switch state {
		case inDouble:
			out.WriteByte(ch)
			if ch == '\\' && i+1 < len(q) {
				out.WriteByte(q[i+1])
				i += 2
				continue
			}
			if ch == '"' {
				state = outside
			}
			i++
			identStart = false
			continue
		case inSingle:
			out.WriteByte(ch)
			if ch == '\\' && i+1 < len(q) {
				out.WriteByte(q[i+1])
				i += 2
				continue
			}
			if ch == '\'' {
				state = outside
			}
			i++
			identStart = false
			continue
		case inBacktick:
			out.WriteByte(ch)
			if ch == '`' {
				state = outside
			}
			i++
			identStart = false
			continue
		}

		// outside any string
		switch ch {
		case '"':
			state = inDouble
			out.WriteByte(ch)
			i++
			identStart = false
			continue
		case '\'':
			state = inSingle
			out.WriteByte(ch)
			i++
			identStart = false
			continue
		case '`':
			state = inBacktick
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
				out.WriteString(`{__name__="`)
				out.WriteString(tok)
				out.WriteString(`"}`)
			} else {
				out.WriteString(tok)
			}
			if trailingDot {
				// Emit the dot that we stripped so the parser still
				// sees the surrounding tokens (rare path — defensive).
				out.WriteByte('.')
			}
			i = j
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
