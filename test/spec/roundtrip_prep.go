// Package spec — the fixture `now64(...)` substitution (build-tag-free).
//
// This file holds the one emitted-SQL rewrite that is a fact about the
// FIXTURE FORMAT rather than about an execution substrate: replacing
// every `now64(...)` reference with a deterministic literal so a golden
// pins a stable byte shape instead of wall-clock drift.
//
// It is untagged because two runners need it — the `chdb` round-trip
// assertion AND the `integration`-tagged strict-scan differential
// (strictscan_integration_test.go), which runs the same emitted SQL
// against a REAL ClickHouse via clickhouse-go/v2 to catch the
// prod-vs-chDB scan-type divergence the chDB lane is structurally blind
// to (chDB leniently coerces e.g. UInt8 -> *float64; prod clickhouse-go
// strict-scans and 502s).
//
// The substrate-specific rewrites live in internal/testsql, which both
// runners and the handler-level chDB fake share. Note that the
// strict-scan differential applies only that package's seed/DDL
// backfills and NOT its chDB workarounds — the `toJSONString(...)`
// Map-column wrap, star-projection expansion and ORDER-BY-over-Map
// nesting all MASK the prod divergence, so the differential must execute
// the RAW emitted SQL (native Map / DateTime64 / Float64 columns) to
// observe exactly what production does.
package spec

import (
	"strings"
)

// nowAnchorLiteral is the deterministic CH literal we splice in place
// of every `now64(...)` reference in the emitted SQL when no explicit
// per-evaluation anchor is supplied (the established fixed-anchor
// round-trip path). It is held as a package const — not computed from
// [defaultNowAnchor] at init — so its byte shape is pinned to exactly the
// value the goldens were generated against; the equality to
// `CHNow64Literal(defaultNowAnchor)` is guarded by a test instead.
//
// The third arg (`'UTC'`) is mandatory: chDB's parser treats the
// timezone slot positionally and rejects `toDateTime64(str, 9)` when
// the literal has fractional seconds.
const nowAnchorLiteral = "toDateTime64('2026-01-01 00:00:01.000000000', 9, 'UTC')"

// SubstituteNow64 splices the deterministic fixed anchor literal in place of
// every `now64(...)` / `now()` reference in query, consuming the `now64(?)`
// arg slot. It is the exported, build-tag-free seam onto [substituteNow64] so
// the `integration`-tagged strict-scan differential rewrites a fixture's
// emitted SQL identically to the chDB round-trip runner before executing it
// against a real ClickHouse.
func SubstituteNow64(query string, args []any) (string, []any) {
	return substituteNow64(query, args)
}

// substituteNow64 splices the package-fixed [nowAnchorLiteral] in place of
// every `now64(...)` / `now()` reference, consuming the `now64(?)` arg slot.
func substituteNow64(query string, args []any) (string, []any) {
	return SubstituteNow64At(query, args, nowAnchorLiteral)
}

// SubstituteNow64At is the anchor-parameterised core of
// [substituteNow64]: it splices `anchorLiteral` (a pre-rendered CH
// DateTime64 literal, e.g. from [CHNow64Literal]) in place of every
// `now64(...)` / `now()` reference, instead of the package-fixed
// [nowAnchorLiteral]. [substituteNow64] passes [nowAnchorLiteral] so the
// established fixed-anchor round-trip path is byte-identical; the
// eval-instant sweep passes a per-T literal so the residual outer
// `now64(?)` result-timestamp projection pins to the swept eval instant
// rather than the fixed default. The window bound itself is NOT a
// now64 in the post-fix SQL — it renders as an inline eval-literal — so
// this anchor only governs the wall-clock projection, never the row
// count or the window semantics under test.
func SubstituteNow64At(query string, args []any, anchorLiteral string) (string, []any) {
	// Fast-path: nothing to do when neither shape is present.
	if !strings.Contains(query, "now64(") && !strings.Contains(query, "now()") {
		return query, args
	}

	var (
		out     strings.Builder
		newArgs = make([]any, 0, len(args))
		argIdx  int
		inStr   byte
	)
	out.Grow(len(query))

	for i := 0; i < len(query); i++ {
		c := query[i]
		// Track string literals so a stray `?` or `now64(` inside
		// quotes is left alone. CH SQL uses single-quotes; backticks
		// quote identifiers, not strings, so they don't interfere
		// with placeholder counting.
		if inStr != 0 {
			out.WriteByte(c)
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '\'' {
			inStr = c
			out.WriteByte(c)
			continue
		}

		// Match `now64(?)` — substitute literal and consume one arg.
		if c == 'n' && strings.HasPrefix(query[i:], "now64(?)") {
			out.WriteString(anchorLiteral)
			// Skip the consumed arg slot. argIdx is the next-to-bind
			// index; it points at the `?` inside `now64(?)` which we
			// are about to drop. Advance past it without copying.
			argIdx++
			i += len("now64(?)") - 1
			continue
		}

		// Match `now64(<int>)` — substitute literal, no args change.
		if c == 'n' && strings.HasPrefix(query[i:], "now64(") {
			// Find the matching `)` at depth 0 starting after the `(`.
			rest := query[i+len("now64("):]
			depth := 1
			j := 0
			for ; j < len(rest); j++ {
				if rest[j] == '(' {
					depth++
				} else if rest[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j < len(rest) {
				inner := strings.TrimSpace(rest[:j])
				// Only substitute when the body is a bare numeric
				// literal (the precision arg). Anything else (no
				// known cases today) passes through to surface a
				// real failure rather than silently mis-rewrite.
				if isIntLiteral(inner) {
					out.WriteString(anchorLiteral)
					i += len("now64(") + j // jump past the closing `)`
					continue
				}
			}
		}

		// Match `now()` — the zero-arg DateTime form emitted by PromQL
		// zero-arg date functions. Substitute with the deterministic
		// DateTime64 anchor literal; CH's `toYear`/`toMonth`/
		// `toDayOfMonth`/`toDayOfWeek`/`toLastDayOfMonth`/`toHour`/
		// `toMinute` accept DateTime64 the same as DateTime, so the
		// type widening is invisible at the call site. No args slot
		// to consume.
		if c == 'n' && strings.HasPrefix(query[i:], "now()") {
			out.WriteString(anchorLiteral)
			i += len("now()") - 1
			continue
		}

		// Generic `?` placeholder: copy the arg through.
		if c == '?' {
			out.WriteByte(c)
			if argIdx < len(args) {
				newArgs = append(newArgs, args[argIdx])
			}
			argIdx++
			continue
		}

		out.WriteByte(c)
	}

	return out.String(), newArgs
}

// isIntLiteral reports whether s is a non-empty run of ASCII digits
// (optionally prefixed by `-`). Used by substituteNow64 to recognise
// the precision literal in `now64(9)` without pulling in strconv.
func isIntLiteral(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
		if len(s) == 1 {
			return false
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
