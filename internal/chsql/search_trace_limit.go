package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitSearchTraceLimit renders a chplan.SearchTraceLimit: the input row
// source restricted to the N newest traces, so /api/search drains only those
// traces' spans instead of every matching row.
//
// Rendered shape:
//
//	SELECT s.* FROM (<input>) AS s
//	WHERE `TraceId` IN (
//	  SELECT `TraceId` FROM (<input>)
//	  GROUP BY `TraceId`
//	  ORDER BY min(`Timestamp`) DESC, `TraceId` ASC
//	  LIMIT <TraceLimit>)
//
// The top-N subquery ranks each trace by its start time (min span Timestamp),
// newest first, with a TraceId-ascending tie-break — the same order
// toTraceSummaries records as StartTimeUnixNano and sortSummariesStartDesc
// applies — so the SQL-selected set is exactly the set TruncateSummaries
// keeps. The input is rendered on both arms: the request's time window and
// matchers ride inside it, so the inner GROUP BY is bounded to the window
// (never the whole table) and the outer drain returns only matching spans,
// keeping the per-spanset Matched total correct.
//
// ponytail: the input subquery is emitted twice (outer drain + inner
// ranking). The window predicate keeps each scan cheap; lift to a single
// `WITH src AS (...)` CTE only if the double scan shows up on the perf gate.
func (e *emitter) emitSearchTraceLimit(n *chplan.SearchTraceLimit) error {
	if n.TraceIDColumn == "" || n.TimestampColumn == "" {
		return fmt.Errorf("%w: SearchTraceLimit column names unset", ErrUnsupported)
	}
	if n.TraceLimit <= 0 {
		// Defensive: the lowering never builds the node with a non-positive
		// limit. Emit the input unchanged rather than a degenerate LIMIT.
		return e.emitNode(n.Input)
	}

	outerSub, err := e.subqueryFrag(n.Input)
	if err != nil {
		return err
	}
	innerSub, err := e.subqueryFrag(n.Input)
	if err != nil {
		return err
	}

	topN := NewQuery().
		Select(Col(n.TraceIDColumn)).
		From(innerSub).
		GroupBy(Col(n.TraceIDColumn)).
		OrderBy(Call("min", Col(n.TimestampColumn)), true).
		OrderBy(Col(n.TraceIDColumn), false).
		Limit(n.TraceLimit).
		Frag()

	sb := NewQuery().
		Select(verbatim("s.*")).
		From(aliasedFrag(outerSub, "s")).
		Where(InSubquery(Col(n.TraceIDColumn), topN))
	e.emitSelect(sb)
	return nil
}
