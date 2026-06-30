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

// tableRegexps bundles the two spans-table-name-dependent matchers.
type tableRegexps struct {
	// spansFrom matches every physical scan of the spans table —
	// `FROM `otel_traces`` and `FROM `otel_traces` AS t`.
	spansFrom *regexp.Regexp
	// passthroughFrom matches the plain pass-through wrapper
	// `SELECT * FROM `otel_traces`` (submatch 1 = the FROM token). This is the
	// matrix-family / seed-leaf shape: a Timestamp predicate on the wrapper's
	// own (or the enclosing) scope DOES push into this scan, so it is excluded.
	passthroughFrom *regexp.Regexp
}

// regexpCache memoises the per-table compiled regexps so the emit chokepoint
// (which runs on every query, almost always with the same default table) does
// not recompile on each call. Keyed by spans-table name; value is tableRegexps.
var regexpCache sync.Map

func regexpsFor(spansTable string) tableRegexps {
	if v, ok := regexpCache.Load(spansTable); ok {
		return v.(tableRegexps)
	}
	q := regexp.QuoteMeta(spansTable)
	tr := tableRegexps{
		spansFrom:       regexp.MustCompile("FROM\\s+`" + q + "`"),
		passthroughFrom: regexp.MustCompile("SELECT\\s+\\*\\s+(FROM\\s+`" + q + "`)"),
	}
	regexpCache.Store(spansTable, tr)
	return tr
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
//   - the scan is a pass-through wrapper (`SELECT * FROM <spans table>`), whose
//     enclosing-scope Timestamp predicate pushes in and prunes (the validated
//     matrix-family wrapper shape);
//   - the scan carries a co-scope `Timestamp` comparison (already prunes);
//   - the SQL contains no scan of spansTable at all.
//
// Findings are returned in ascending FROM-offset order (regexp scan order).
func UnwindowedSpansScans(sql, spansTable string) []Finding {
	if spansTable == "" || strings.TrimSpace(sql) == "" {
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

	tr := regexpsFor(spansTable)

	// Pass-through wrappers (`SELECT * FROM <spans table>`) are pruning-safe;
	// record their FROM offsets so they are excluded.
	passthrough := make(map[int]struct{})
	for _, m := range tr.passthroughFrom.FindAllStringSubmatchIndex(sql, -1) {
		// m[2] = start of submatch 1 (the FROM token).
		passthrough[m[2]] = struct{}{}
	}

	recBodies := recursiveBodySpans(sql)

	var out []Finding
	for _, m := range tr.spansFrom.FindAllStringIndex(sql, -1) {
		from := m[0]
		if _, ok := passthrough[from]; ok {
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

// topLevelScopeForward returns the text from byte offset i forward that lives
// at the SAME parenthesis depth as i AND in the SAME set-operation arm as i —
// i.e. the FROM/WHERE/GROUP BY clauses of the scan's own SELECT, with every
// nested subquery (the `TraceId IN (…)` seed, a `fromUnixTimestamp64Nano(<arg>)`
// call's arg) elided. Collection stops at the `)` that closes the enclosing
// scope, OR at a depth-0 set-op boundary (`UNION [ALL|DISTINCT]` or a sibling
// `SELECT` opening a new arm), whichever comes first.
//
// The set-op stop closes a false-accept hole: a windowless `otel_traces` scan
// in one arm of a UNION must NOT borrow a sibling arm's `Timestamp` predicate to
// satisfy the co-scope prune check. A scan's own pruning predicate always sits
// in its arm's WHERE/GROUP BY, which precede any UNION / sibling SELECT, so
// truncating at the boundary never drops a legitimate co-scope `Timestamp`.
func topLevelScopeForward(sql string, i int) string {
	var b strings.Builder
	depth := 0
	for j := i; j < len(sql); j++ {
		switch c := sql[j]; c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return b.String()
			}
		default:
			if depth == 0 {
				if setOpBoundaryAt(sql, j) {
					return b.String()
				}
				b.WriteByte(c)
			}
		}
	}
	return b.String()
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
