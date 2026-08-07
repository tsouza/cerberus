package telemetry

import (
	"fmt"
	"strings"
	"unicode"
)

// SanitizeForLog neutralizes control characters in a string before it is
// attached to a log record, closing the log-injection class CodeQL's
// go/log-injection rule flags: a request-supplied value containing a raw
// newline, carriage return, or other C0/C1 control character can forge a
// fake log line when the record is rendered as text, or corrupt a
// downstream line-oriented log consumer — regardless of which slog.Handler
// ends up rendering it (config.go can wire either NewTextHandler or
// NewJSONHandler, and telemetry's own otelslog wrapper re-exports every
// record to an OTel log backend on top of that). Call it on request-derived
// values immediately before they reach a log call — the query string a
// client sent, never a value cerberus itself constructed.
//
// Every control rune (unicode.IsControl — the C0, DEL, and C1 ranges) is
// replaced with its Go-syntax escape (\n, \r, \t) or a \xNN escape for
// anything else, so the logged value stays legible for debugging instead of
// being silently dropped or truncated.
func SanitizeForLog(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s // fast path: the overwhelming common case has no control chars
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String()
}

// SanitizeArgsForLog applies [SanitizeForLog] to every string in a
// query-argument slice, so a bound parameter carrying a raw newline cannot
// forge a log line when the slice is attached to a log record. Arguments
// cerberus binds are request-derived by construction — a label value, a
// metric name, a line-filter string — which makes the whole slice a
// log-injection source the moment it reaches a log call.
//
// Non-string arguments (timestamps, limits, numeric bounds) render through
// their own formatters and carry no control characters, so they pass
// through untouched. The input slice is never mutated: the caller still
// owns it and passes the same values to the database driver.
func SanitizeArgsForLog(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			out[i] = SanitizeForLog(s)
			continue
		}
		out[i] = a
	}
	return out
}
