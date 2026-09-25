// Package spansscan is the shared, dependency-light matcher behind the
// spans-scan partition-pruning invariant. It statically inspects an emitted
// ClickHouse statement for a physical spans-table scan sitting in a scope
// where ClickHouse CANNOT push the request window down into the partition
// pruner — a recursive (`WITH RECURSIVE`) arm or a pre-`TraceId IN` `GROUP BY`
// — yet carrying no co-scope `Timestamp` predicate.
//
// The spans table is `PARTITION BY toDate(Timestamp)`, so ONLY a Timestamp
// range sitting directly on the physical scan prunes partitions; a windowed
// `TraceId IN (<seed>)` membership is inert for pruning. A recursive STEP /
// ANCHOR arm or a pre-IN `GROUP BY` therefore reads the WHOLE table when its
// scan is unwindowed — the traces-OOM class confirmed against prod ClickHouse.
//
// The matcher lives in this leaf (stdlib-only) package so BOTH the perf/fanout
// corpus linter AND the chsql emit chokepoint can import it without a layering
// violation: chsql may not import perf/fanout and vice-versa, but both may
// import spansscan (declared in .go-arch-lint.yml). The corpus lint gives a
// per-fixture tripwire; the emit chokepoint gives the universal, construction-
// proof backstop — every statement that reaches the wire is scanned.
package spansscan

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Finding is one windowless physical spans-table scan: a `FROM <spans table>`
// sitting in a recursive arm or under a `GROUP BY` with no co-scope `Timestamp`
// predicate, inside a statement that otherwise carries a request window.
type Finding struct {
	// Offset is the byte offset of the matched `FROM` token in the scanned SQL.
	Offset int
	// Reason describes which non-pushdown scope the scan sits in and why it
	// reads the whole table — suitable as an error / test-failure message.
	Reason string
}

const (
	// RequestWindowBound is the search / leaf rendering of a request-window
	// bound (`Timestamp >= fromUnixTimestamp64Nano(<nanos>)`) — the shape the
	// structural / nested-set recursive arms and the trace-id seed leaves emit.
	// It is NOT the only rendering: the metrics range-window grid bounds its
	// matrix wrapper with `Timestamp > toDateTime64('<ts>', 9)` instead, never
	// emitting fromUnixTimestamp64Nano. So the matcher must NOT key its
	// window-present precondition on this literal alone (that left the metrics
	// path uncovered — a windowless recursive scan under a `| rate()` slipped
	// through because the grid emits toDateTime64). The precondition instead
	// arms on any `Timestamp` range comparison (reTimestampCmp), which subsumes
	// both renderings. This const is retained as the canonical search-window
	// marker for callers/tests that assert on that specific shape.
	RequestWindowBound = "fromUnixTimestamp64Nano("
	// groupByClause marks a scope whose aggregation runs BEFORE any
	// `TraceId IN (...)` seed can filter — CH cannot push a window below it, so
	// the window must sit on the scan itself.
	groupByClause = "GROUP BY"
)

var (
	// reRecursive locates each `WITH RECURSIVE` so recursiveBodySpans can carve
	// out the body whose arms CH cannot push a window below.
	reRecursive = regexp.MustCompile(`(?i)WITH\s+RECURSIVE`)
	// reTimestampCmp matches a `Timestamp` column in a comparison — the only
	// shape that prunes. The path-encoding `toUnixTimestamp64Nano(`Timestamp`)`
	// is a function arg, not a comparison, so it does not count.
	reTimestampCmp = regexp.MustCompile("`Timestamp`\\s*(>=|<=|>|<|=)|(>=|<=|>|<|=)\\s*`Timestamp`")
)

// regexpCache memoises the per-table compiled `FROM <spans table>` matcher so
// the emit chokepoint (which runs on every query, almost always with the same
// default table) does not recompile on each call. Keyed by spans-table name;
// value is *regexp.Regexp.
var regexpCache sync.Map

// spansFromRegexp returns the matcher for every physical scan of the spans
// table — `FROM `otel_traces“ and `FROM `otel_traces` AS t`.
func spansFromRegexp(spansTable string) *regexp.Regexp {
	if v, ok := regexpCache.Load(spansTable); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile("FROM\\s+`" + regexp.QuoteMeta(spansTable) + "`")
	regexpCache.Store(spansTable, re)
	return re
}

// UnwindowedSpansScans returns every physical scan of spansTable in sql that
// sits in a non-pushdown scope (a `WITH RECURSIVE` arm or under a `GROUP BY`)
// while the statement carries a request window, yet has no co-scope `Timestamp`
// predicate to prune partitions.
//
// It returns nil — defers — when:
//   - spansTable is empty or sql is blank;
//   - the statement carries no request window at all (no `Timestamp` range
//     comparison in any rendering — neither fromUnixTimestamp64Nano nor the
//     metrics grid's toDateTime64) — there is nothing to push down, so the
//     unbounded concern belongs to the resource-bound gate, not this
//     partition-pruning matcher;
//   - the scan is the table of a DERIVED TABLE — a subquery in `FROM (…)` /
//     `JOIN (…)` position — whose enclosing-scope Timestamp predicate pushes in
//     and prunes (the validated matrix-family wrapper and seed-leaf shape);
//   - the scan carries a co-scope `Timestamp` comparison (already prunes);
//   - the SQL contains no scan of spansTable at all.
//
// Findings are returned in ascending FROM-offset order (regexp scan order).
func UnwindowedSpansScans(sql, spansTable string) []Finding {
	if spansTable == "" || strings.TrimSpace(sql) == "" {
		return nil
	}
	// Every finding this matcher can return is anchored on a `FROM <spans
	// table>` match, so a statement that never names the spans table has no
	// finding to give — provably, not heuristically. Answering that with one
	// substring scan BEFORE any regexp runs is what keeps the emit chokepoint
	// (which sees EVERY statement, metrics and logs included) off the regexp
	// engine on the overwhelming majority of queries: only traces queries name
	// the spans table. Without it, `reTimestampCmp` below backtracked across
	// the full text of every metrics statement — on a broad metadata probe
	// that renders ~110KB statements it cost more CPU than generating the SQL
	// did (37% of request time against chsql.Emit's own 13%).
	if !strings.Contains(sql, spansTable) {
		return nil
	}
	// No request window anywhere in the statement → nothing to push down; the
	// unbounded-query concern belongs to the resource-bound gate, not this
	// partition-pruning matcher. A request window is ANY `Timestamp` range
	// comparison, however it is rendered — the search / leaf path emits
	// `fromUnixTimestamp64Nano(<nanos>)` (RequestWindowBound), the metrics
	// range-window grid emits `toDateTime64('<ts>', 9)`. Keying on the
	// `Timestamp` comparison itself (reTimestampCmp) rather than one function
	// rendering is what closes the metrics-path gap: a `{ } >> { } | rate()`
	// whose recursive arm lost its window would otherwise slip through here
	// because the grid never emits fromUnixTimestamp64Nano.
	if !reTimestampCmp.MatchString(sql) {
		return nil
	}

	recBodies := recursiveBodySpans(sql)

	var out []Finding
	for _, m := range spansFromRegexp(spansTable).FindAllStringIndex(sql, -1) {
		from := m[0]
		if derivedTableScan(sql, from) {
			// A derived table is a bare relational operand: an enclosing-scope
			// Timestamp predicate pushes through it and prunes. Fine.
			continue
		}
		scope := topLevelScopeForward(sql, from)
		if reTimestampCmp.MatchString(scope) {
			// A co-scope Timestamp predicate sits directly on the scan — it
			// prunes. Fine.
			continue
		}
		switch {
		case withinAnySpan(from, recBodies):
			out = append(out, Finding{Offset: from, Reason: recursiveArmReason(spansTable)})
		case strings.Contains(scope, groupByClause):
			out = append(out, Finding{Offset: from, Reason: groupByReason(spansTable)})
		}
	}
	return out
}

// recursiveArmReason is the message for a windowless scan inside a recursive arm.
func recursiveArmReason(spansTable string) string {
	return fmt.Sprintf(
		"physical `%s` scan in a WITH RECURSIVE arm carries no co-scope `Timestamp` "+
			"predicate while the query is windowed — the recursive closure reads the whole "+
			"table (a windowed `TraceId IN (...)` seed is inert for partition pruning). "+
			"Replicate the request window onto the recursive scan.",
		spansTable,
	)
}

// groupByReason is the message for a windowless scan under a GROUP BY.
func groupByReason(spansTable string) string {
	return fmt.Sprintf(
		"physical `%s` scan under a GROUP BY carries no co-scope `Timestamp` predicate while the query is windowed — the aggregation runs over the whole table before any `TraceId IN (...)` seed can filter. Push the request window onto the scan itself.",
		spansTable,
	)
}

// recursiveBodySpans returns the [open, close] byte offsets of every
// `WITH RECURSIVE <name> AS ( … )` body's outermost parentheses. A scan whose
// FROM offset falls inside one of these spans is a recursive arm (anchor or
// STEP), where CH cannot push a window below the recursion.
func recursiveBodySpans(sql string) [][2]int {
	var spans [][2]int
	for _, loc := range reRecursive.FindAllStringIndex(sql, -1) {
		open := strings.IndexByte(sql[loc[1]:], '(')
		if open < 0 {
			continue
		}
		open += loc[1]
		depth := 0
		for j := open; j < len(sql); j++ {
			switch sql[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					spans = append(spans, [2]int{open, j})
					j = len(sql) // break the inner loop
				}
			}
		}
	}
	return spans
}

// topLevelScopeForward returns the text from byte offset i forward that belongs
// to the SAME SELECT scope as i AND to the SAME set-operation arm as i — i.e.
// the FROM/WHERE/GROUP BY clauses of the scan's own SELECT, with every nested
// SUBQUERY (the `TraceId IN (…)` seed, a `FROM (SELECT …)` inline view) elided.
// Collection stops at the `)` that closes the enclosing scope, OR at a depth-0
// set-op boundary (`UNION [ALL|DISTINCT]` or a sibling `SELECT` opening a new
// arm), whichever comes first.
//
// Scope membership is decided by what a parenthesis OPENS, not by how deeply it
// nests. A predicate is co-scope wherever the emitter chose to bracket it:
// `WHERE `Timestamp` >= x AND `Timestamp` <= y` and
// `WHERE (`Timestamp` >= x) AND (`Timestamp` <= y)` bound the same physical
// scan and prune the same partitions, and chsql brackets a `Binary` operand
// whenever the surrounding expression needs it. Reading nesting alone as
// "another scope" hides the bounds of any fully windowed scan the optimizer's
// late-materialisation rewrite leaves bracketed, and rejects a query that is in
// fact bounded — a false positive that takes the query off the wire (#1889). So
// a group whose first keyword is `SELECT`/`WITH` is a subquery and is dropped
// whole (its bounds belong to a different scan and must never be borrowed);
// every other group is an expression group, descended into and reproduced
// verbatim, brackets included, so an operator can never bind across a dropped
// bracket.
//
// The set-op stop closes a false-accept hole: a windowless `otel_traces` scan
// in one arm of a UNION must NOT borrow a sibling arm's `Timestamp` predicate to
// satisfy the co-scope prune check. A scan's own pruning predicate always sits
// in its arm's WHERE/GROUP BY, which precede any UNION / sibling SELECT, so
// truncating at the boundary never drops a legitimate co-scope `Timestamp`.
func topLevelScopeForward(sql string, i int) string {
	var b strings.Builder
	// depth counts open parentheses relative to i. subqueryDepth records the
	// depth of the outermost subquery group currently being elided, or
	// notEliding when collection is live.
	const notEliding = -1
	depth := 0
	subqueryDepth := notEliding
	for j := i; j < len(sql); j++ {
		switch c := sql[j]; c {
		case '(':
			depth++
			if subqueryDepth == notEliding {
				if opensSubquery(sql, j) {
					subqueryDepth = depth
					continue
				}
				b.WriteByte(c)
			}
		case ')':
			switch subqueryDepth {
			case depth:
				subqueryDepth = notEliding
			case notEliding:
				b.WriteByte(c)
			}
			depth--
			if depth < 0 {
				return b.String()
			}
		default:
			if subqueryDepth != notEliding {
				continue
			}
			if depth == 0 && setOpBoundaryAt(sql, j) {
				return b.String()
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// opensSubquery reports whether the parenthesis at sql[j] opens a SUBQUERY —
// a group whose first keyword is `SELECT` or `WITH`, reached by skipping
// whitespace and any run of immediately nested opening parentheses so the
// `((SELECT …) UNION ALL (SELECT …))` set-op group a `TraceId IN` seed renders
// is recognised at its outermost bracket. Any other group — a bracketed
// conjunct, a function-call argument list, an array or tuple literal — is an
// expression group belonging to the enclosing SELECT's own scope.
func opensSubquery(sql string, j int) bool {
	for k := j + 1; k < len(sql); k++ {
		switch sql[k] {
		case '(', ' ', '\t', '\n', '\r':
			continue
		default:
			return wordAt(sql, k, "SELECT") || wordAt(sql, k, "WITH")
		}
	}
	return false
}

// derivedTableScan reports whether the physical scan whose `FROM` token begins
// at byte offset i belongs to a DERIVED TABLE — a subquery sitting in a table
// position, `FROM (SELECT …)` or `JOIN (SELECT …)`.
//
// That position IS the pruning-safe pass-through shape: a derived table is a
// bare relational operand, so a `Timestamp` predicate in the ENCLOSING scope
// pushes through it and prunes this scan's partitions. Deciding it by position
// replaces an exclusion keyed on ONE literal rendering of that wrapper
// (`SELECT\s+\*\s+FROM\s+`<table>“), which made the guard's verdict a function
// of the emitter's projection spelling and was wrong in both directions:
//
//   - FALSE POSITIVE. The optimizer's late-materialisation rule rewrote a
//     wrapper from `SELECT *` into an explicit column list, the exclusion
//     stopped matching, and a pruning-safe scan was flagged as unbounded — a
//     500 on a live surface (#1889).
//   - FALSE NEGATIVE. A genuinely unwindowed scan that KEPT the `SELECT *`
//     spelling — a `WITH RECURSIVE … AS (SELECT * FROM <table> …)` arm, or a
//     top-level `SELECT * FROM <table> … GROUP BY …` — was skipped by the guard
//     entirely, which is the whole-table read it exists to stop (#1109).
//
// A projection list is a rendering choice; a table position is structure. Only
// structure may decide this, so an added alias, an explicit column list, a
// `SELECT *` carrying a `REPLACE` / `EXCEPT` modifier and any whitespace change
// all leave the verdict untouched.
//
// The owning `SELECT` is located by owningSelect and the token in front of it by
// precedingToken, which skips whitespace and any run of opening parentheses so a
// set-op group (`FROM ((SELECT …) UNION ALL (SELECT …))`) is recognised at its
// outermost bracket — the backward mirror of what opensSubquery does forward. A
// `SELECT` reached from `AS` (a CTE body, which is every `WITH RECURSIVE` arm)
// or from the statement root is NOT a derived table: nothing encloses it that
// ClickHouse could push a window in from. Both "no owning SELECT" and "nothing
// in front of it" arrive as a negative offset, which wordEndingAt rejects, so
// neither needs a guard of its own.
func derivedTableScan(sql string, i int) bool {
	k := precedingToken(sql, owningSelect(sql, i))
	return wordEndingAt(sql, k, "FROM") || wordEndingAt(sql, k, "JOIN")
}

// precedingToken returns the offset of the last byte before sel that is neither
// whitespace nor an opening parenthesis — the end of the token that introduces
// the `SELECT` at sel. It returns -1 when sel is not positive, or when only
// skippable bytes precede it: the same "no keyword here" answer wordEndingAt
// gives for a negative offset, so callers need no separate empty case.
func precedingToken(sql string, sel int) int {
	for k := sel - 1; k >= 0; k-- {
		switch sql[k] {
		case ' ', '\t', '\n', '\r', '(':
			continue
		}
		return k
	}
	return -1
}

// owningSelect returns the byte offset of the `SELECT` keyword that owns the
// clause at offset i — the nearest preceding `SELECT` sitting at i's OWN
// parenthesis depth. Scanning backwards, a `)` opens a nested group that must be
// skipped whole and a `(` closes one; a `(` met at depth zero means the
// enclosing scope opened without an intervening `SELECT`, so there is no owning
// SELECT to report. Returns -1 when none is found.
func owningSelect(sql string, i int) int {
	depth := 0
	for j := i - 1; j >= 0; j-- {
		switch sql[j] {
		case ')':
			depth++
		case '(':
			if depth == 0 {
				return -1
			}
			depth--
		default:
			if depth == 0 && wordAt(sql, j, "SELECT") {
				return j
			}
		}
	}
	return -1
}

// wordEndingAt reports whether the standalone keyword kw ENDS at sql[j]
// (inclusive) — the backward-scan counterpart of wordAt, which anchors at a
// keyword's first byte instead.
func wordEndingAt(sql string, j int, kw string) bool {
	start := j - len(kw) + 1
	if start < 0 {
		return false
	}
	return wordAt(sql, start, kw)
}

// setOpBoundaryAt reports whether a set-operation arm boundary begins at sql[j]:
// a `UNION` (covering `UNION ALL` / `UNION DISTINCT`) or a sibling `SELECT` that
// opens a new arm (the `SELECT` form also covers `INTERSECT` / `EXCEPT` chains,
// whose next arm still begins with `SELECT`). Only meaningful at depth 0, where
// the caller invokes it — a subquery's own `SELECT`/`UNION` sits at depth > 0
// and is elided before this is ever consulted.
func setOpBoundaryAt(sql string, j int) bool {
	return wordAt(sql, j, "UNION") || wordAt(sql, j, "SELECT")
}

// wordAt reports whether the case-insensitive keyword kw appears at sql[j] as a
// standalone token: the byte before j (if any) and the byte after the keyword
// must both be non-identifier characters, so `_union`, `unionx`, and the `UNION`
// inside an identifier never trip the boundary.
func wordAt(sql string, j int, kw string) bool {
	if j+len(kw) > len(sql) {
		return false
	}
	if !strings.EqualFold(sql[j:j+len(kw)], kw) {
		return false
	}
	if j > 0 && isIdentByte(sql[j-1]) {
		return false
	}
	if end := j + len(kw); end < len(sql) && isIdentByte(sql[end]) {
		return false
	}
	return true
}

// isIdentByte reports whether c can appear inside a SQL identifier word.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// withinAnySpan reports whether offset i lies inside any [open, close] span.
func withinAnySpan(i int, spans [][2]int) bool {
	for _, s := range spans {
		if i >= s[0] && i <= s[1] {
			return true
		}
	}
	return false
}
