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
// FromSourceLevels is the exported wrapper over [fromSourceLevels] for
// callers outside the corpus profiler that need the same per-level
// decomposition — notably the scale-wall pin (test/perf), which counts
// each level over its own seeded-at-scale table to derive the peak
// intermediate cardinality. Keeping the decomposition in ONE place means
// the corpus ratchet and the scale-wall pin agree on what "a pipeline
// level" is by construction.
//
// This wrapper deliberately drops the uncountable-level reasons
// [levelsWithReasons] also returns: FromSourceLevels' only caller
// (test/perf's scale-wall pin) already tolerates an individual level
// failing to count in isolation (see its own comment at the call site),
// so it has no honesty gap to plug the way [Record.FanFactor] does — see
// issue #1519 part 2.
func FromSourceLevels(query string) []string {
	levels, _ := levelsWithReasons(query)
	return levels
}

// fromSourceLevels is the levels-only view of [levelsWithReasons], kept
// for callers (and tests) that only need the descent, not why it stopped.
func fromSourceLevels(query string) []string {
	levels, _ := levelsWithReasons(query)
	return levels
}

// levelsWithReasons walks the same leftmost FROM-source chain
// [fromSourceLevels] documents, and ADDITIONALLY reports, as
// uncountableReasons, every point where the descent had to stop because
// the next level is a WITH-prefixed (CTE) subquery whose body references
// names only in scope at its own level — the profiler cannot run
// `count() FROM (<that level>)` standalone, so its contribution to
// PeakIntermediate is invisible.
//
// A RECURSIVE CTE (`WITH RECURSIVE ...`) is deliberately EXCLUDED from
// the reasons returned here, even though it also stops the descent: its
// closure can sit anywhere in the plan — the leftmost chain this function
// walks, or a JOIN's right-hand branch this function (by design, see the
// package doc) never visits at all, as `nested_set_left_position` and
// `structural_not_ancestor` demonstrate: the recursive CTE lives inside a
// JOIN RHS, so this descent never even reaches it. [ProfileFixture] gets
// full, structural coverage of recursion (leftmost-chain OR join-branch)
// from EXPLAIN PLAN's HasRecursiveCTE flag instead, and adds exactly one
// "recursive_cte_step" reason from that flag — reporting it again here,
// only for the fraction of cases the leftmost chain happens to pass
// through, would both double-count some fixtures and miss others.
//
// The non-recursive case (a plain `WITH c AS (...) SELECT ...` CTE, e.g.
// emitSetOperation's `&&` arms in internal/chsql/set_op.go) has no such
// structural EXPLAIN signal, so it is reported here — this is the
// "CTE reference" / "pre-rendered subquery splice" category from issue
// #1519: the CTE body is exactly the pre-rendered SQL text
// [subqueryFrag]-family emitters splice into a WITH clause, and once
// spliced there this function cannot see through it.
func levelsWithReasons(query string) ([]string, []string) {
	query = strings.TrimSpace(query)
	levels := []string{query}

	// A RECURSIVE top-level query: 1 level, no reason (see doc above —
	// ProfileFixture's HasRecursiveCTE-driven reason covers it).
	if hasWithRecursivePrefix(query) {
		return levels, nil
	}
	// A non-recursive WITH-prefixed top-level query: 1 level, and a
	// reason — descending into the CTE bodies would reference
	// out-of-scope CTE names.
	if hasWithPrefix(query) {
		return levels, []string{uncountableCTEReason(0)}
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
		if hasWithRecursivePrefix(inner) {
			// Same rationale as the top-level case: no reason here, the
			// HasRecursiveCTE flag covers it exactly once.
			break
		}
		if hasWithPrefix(inner) {
			return levels, []string{uncountableCTEReason(len(levels))}
		}
		levels = append(levels, inner)
		cur = inner
	}
	return levels, nil
}

// uncountableCTEReason renders the human-readable line recorded in
// [Record.UncountableReasons] when the descent stops on a non-recursive
// WITH-prefixed subquery at the given depth.
func uncountableCTEReason(depth int) string {
	return fmt.Sprintf(
		"depth %d: WITH-prefixed subquery (CTE reference / pre-rendered subquery splice) not descended — "+
			"its body's inner row counts are out of scope to measure standalone", depth,
	)
}

// recursiveCTEUncountableReason is the single reason [ProfileFixture]
// records, once, whenever EXPLAIN PLAN reports a recursive CTE step
// anywhere in the plan (leftmost chain or a JOIN branch) — see
// [levelsWithReasons]' doc for why this is a structural (EXPLAIN-driven)
// signal rather than one derived from the leftmost descent.
const recursiveCTEUncountableReason = "recursive_cte_step: EXPLAIN plan reports a recursive CTE " +
	"(ReadFromRecursiveCTEStep); the per-level count() decomposition cannot see the closure's own " +
	"internal peak size, only whatever row count reaches the level that embeds it"

// hasWithPrefix reports whether query begins with a `WITH ` keyword
// (case-insensitive), i.e. carries a leading CTE chain (recursive or
// not).
func hasWithPrefix(query string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "WITH ")
}

// hasWithRecursivePrefix reports whether query begins with `WITH
// RECURSIVE ` — the RECURSIVE-step subset of [hasWithPrefix].
func hasWithRecursivePrefix(query string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "WITH RECURSIVE ")
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
