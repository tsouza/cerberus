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
//	WHERE `TraceId` GLOBAL IN (
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
// GLOBAL IN, not IN (cerberus issue #3128, real multi-data-shard evidence):
// once `otel_traces` is a `Distributed` wrapper (epic #3074), both arms
// read it through a DERIVED table — `(<input>)` — and ClickHouse's
// `distributed_product_mode=global` rewrite (pinned by internal/chclient)
// does not reach an IN whose subquery reads the Distributed table that way;
// it only rewrites a subquery whose FROM is the Distributed table directly.
// Left as a plain IN, every shard the outer drain fans out to re-executed
// the ranking subquery as a distributed query of its own: one dispatch
// produced DataShardCount²-1 per-shard Select statements, up to
// DataShardCount²/2 of them concurrent — 3 children at N=2 and 15 at N=4
// (internal peaks 6-10 against a 4-wide shard set) in e2e runs 34055887965
// / 34055025272 — which no per-dispatch admission weight could bound. An
// explicit GLOBAL is honoured regardless of nesting: the initiator runs the
// ranking subquery once (DataShardCount children), then broadcasts the
// at-most-TraceLimit trace ids as a temporary table with the outer drain
// (DataShardCount more), two sequential phases of DataShardCount each. On a
// single-node deployment GLOBAL IN behaves exactly as IN. The broadcast
// payload is the LIMIT-bounded id set, never the scan.
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
		Where(GlobalInSubquery(Col(n.TraceIDColumn), topN))
	return e.emitSelect(sb)
}
