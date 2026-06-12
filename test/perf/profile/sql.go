//go:build chdb

package profile

import (
	"fmt"
	"strings"
)

// planHasCrossJoin reports whether an EXPLAIN PLAN actions=1 body
// contains a CROSS join. ClickHouse renders cross joins as a `Join`
// operator node followed by a `Type: CROSS` line (see the probe under
// internal/perf/profile — `Join (JOIN FillRightFirst)` / `Type: CROSS`).
// Matching `Type: CROSS` is precise: inner/left/right joins render
// `Type: INNER` / `LEFT` / `RIGHT` instead.
func planHasCrossJoin(plan string) bool {
	return strings.Contains(plan, "Type: CROSS")
}

// planHasArrayJoin reports whether the plan contains an ARRAY JOIN
// operator. ClickHouse renders it as an `ArrayJoin (ARRAY JOIN)` node.
func planHasArrayJoin(plan string) bool {
	return strings.Contains(plan, "ArrayJoin (ARRAY JOIN)") ||
		strings.Contains(plan, "ARRAY JOIN")
}

// planHasRecursiveCTE reports whether the plan reads from a recursive
// CTE. ClickHouse renders the recursion as a `ReadFromRecursiveCTEStep`
// node.
func planHasRecursiveCTE(plan string) bool {
	return strings.Contains(plan, "ReadFromRecursiveCTEStep") ||
		strings.Contains(plan, "RecursiveCTE")
}

// fromSourceLevels returns the SQL text of each FROM-source subquery
// level, OUTERMOST FIRST, so callers can run `count() FROM (<level>)` per
// level to build the intermediate-cardinality decomposition. Depth 0 is
// the full query; each subsequent entry strips one layer of `SELECT ...
// FROM (<inner>) ...` nesting, descending the leftmost FROM-source chain
// down to the leaf scan.
//
// Only the LEFTMOST FROM source is descended at each level — the common
// fan-out shape in cerberus's emitted SQL is a straight nest of
// `SELECT ... FROM (SELECT ... FROM (... merge(...)))`, where each layer
// is the Project / Aggregate / Filter / RangeWindow stage wrapping the
// one below. A CROSS JOIN / ARRAY JOIN widens the row set WITHIN a level
// (so its inflated count shows up as that level's count()), which is
// exactly what we want the per-level count to capture. Branch subqueries
// (UNION arms, join RHS, scalar/IN subqueries) are not separately
// descended — the level's own count() already reflects their contribution
// to that level's row set.
//
// A WITH-prefixed query (CTE chain, recursive or set-op CSE) is kept
// intact at depth 0 and not descended, because its inner SELECTs
// reference CTE names that are only in scope at the outer level — running
// `count()` on a stripped inner level would fail (caught + excluded by the
// caller). The outer count() still measures the post-CTE result, and the
// EXPLAIN plan flags still detect the recursive/cross operators.
func fromSourceLevels(query string) []string {
	query = strings.TrimSpace(query)
	levels := []string{query}

	// WITH-prefixed queries: keep depth 0 only. Descending into CTE
	// bodies would reference out-of-scope CTE names.
	if hasWithPrefix(query) {
		return levels
	}

	cur := query
	// Bound the descent so a pathological input can't loop unboundedly.
	for i := 0; i < 64; i++ {
		inner, ok := leftmostFromSubquery(cur)
		if !ok {
			break
		}
		inner = strings.TrimSpace(inner)
		if inner == "" || strings.EqualFold(inner, cur) {
			break
		}
		// A CTE-prefixed inner level can't be counted standalone; stop
		// descending (its references are out of scope).
		if hasWithPrefix(inner) {
			break
		}
		levels = append(levels, inner)
		cur = inner
	}
	return levels
}

// hasWithPrefix reports whether query begins with a `WITH ` keyword
// (case-insensitive), i.e. carries a leading CTE chain.
func hasWithPrefix(query string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "WITH ")
}

// leftmostFromSubquery extracts the parenthesised subquery that is the
// leftmost FROM source of query, returning (inner, true) when the FROM
// source is a `(SELECT ...)` subquery. Returns ("", false) when the FROM
// source is a bare table / merge(...) / table function (the leaf scan) or
// when the query has no depth-0 FROM.
//
// The scan walks at paren depth 0 for the ` FROM ` keyword, then checks
// whether the next non-space token opens a `(`. If so it returns the
// balanced contents of that paren group. Single-quoted strings shield
// any ` FROM ` or parens inside literals.
func leftmostFromSubquery(query string) (string, bool) {
	fromIdx := indexDepth0(query, " FROM ")
	if fromIdx < 0 {
		return "", false
	}
	rest := strings.TrimLeft(query[fromIdx+len(" FROM "):], " \t\n\r")
	if rest == "" || rest[0] != '(' {
		return "", false
	}
	inner, ok := balancedParen(rest)
	if !ok {
		return "", false
	}
	// The subquery must itself be a SELECT (or a WITH/SELECT) to be a
	// countable FROM source. A `(merge(...))` or `('a','b')` IN-list
	// isn't a level.
	trimmed := strings.TrimSpace(inner)
	up := strings.ToUpper(trimmed)
	if !strings.HasPrefix(up, "SELECT ") && !strings.HasPrefix(up, "WITH ") && !strings.HasPrefix(up, "(") {
		return "", false
	}
	return inner, true
}

// indexDepth0 returns the byte index of the first occurrence of needle in
// s that sits at parenthesis depth 0 and outside single-quoted string
// literals. Returns -1 when not found. needle must not contain quotes or
// parens (callers pass ` FROM `).
func indexDepth0(s, needle string) int {
	depth := 0
	inStr := false
	for i := 0; i+len(needle) <= len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && strings.EqualFold(s[i:i+len(needle)], needle) {
				return i
			}
		}
	}
	return -1
}

// balancedParen, given a string whose first byte is '(', returns the
// contents between that '(' and its matching ')' (exclusive), and true.
// Returns ("", false) when the parens are unbalanced. Single-quoted
// strings shield parens.
func balancedParen(s string) (string, bool) {
	if len(s) == 0 || s[0] != '(' {
		return "", false
	}
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// inlineArgs substitutes the positional `?` placeholders in query with
// the literal forms of args, left to right. chDB's session API has no
// placeholder binding, so the profiler inlines bound args textually. The
// substitution is plan- and count-faithful: a bound string becomes a
// single-quoted CH string literal, numbers become bare numeric literals.
//
// `?` characters inside single-quoted string literals are NOT
// placeholders and are left untouched.
func inlineArgs(query string, args []any) string {
	if len(args) == 0 || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16*len(args))
	argIdx := 0
	inStr := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inStr {
			b.WriteByte(c)
			if c == '\'' {
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
			b.WriteByte(c)
		case '?':
			if argIdx < len(args) {
				b.WriteString(literalArg(args[argIdx]))
				argIdx++
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// literalArg renders a bound arg as a CH SQL literal.
func literalArg(v any) string {
	switch x := v.(type) {
	case string:
		// Single-quote, escaping embedded quotes + backslashes.
		esc := strings.ReplaceAll(x, `\`, `\\`)
		esc = strings.ReplaceAll(esc, `'`, `\'`)
		return "'" + esc + "'"
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", x)
	}
}
