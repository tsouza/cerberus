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
//	WHERE `<trace-id role>` GLOBAL IN (
//	  SELECT `<trace-id role>` FROM (<input>)
//	  GROUP BY `<trace-id role>`
//	  ORDER BY min(`<timestamp role>`) DESC, `<trace-id role>` ASC
//	  LIMIT <TraceLimit>)
//
// The physical driver names come from the input's closed row schema. Each role
// must identify exactly one named column, and a physical name cannot also
// identify an output with another role. The outer SELECT s.* deliberately
// preserves the child's complete row shape; the drivers are inputs only.
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
// payload is the LIMIT-bounded id set, never the scan. The GLOBAL is not
// spelled here: InSubquery writes it for every subquery that renders a
// physical table scan (its own doc), the rule this shape was the first
// proven instance of and every other self-referencing emitter shares.
//
// The input subquery is emitted twice (outer drain + inner ranking) by
// design. The window predicate keeps each scan cheap, and a `WITH src AS
// (...)` CTE would not remove the second scan: ClickHouse inlines a CTE at
// every reference rather than materialising it, so there is nothing to lift.
func (e *emitter) emitSearchTraceLimit(n *chplan.SearchTraceLimit) error {
	if n.Input == nil {
		return fmt.Errorf("%w: SearchTraceLimit input unset", ErrUnsupported)
	}
	inputSchema := n.Input.RowType()
	traceID, hasTraceID := uniqueSearchTraceLimitInputColumn(inputSchema, chplan.RoleTraceID)
	timestamp, hasTimestamp := uniqueSearchTraceLimitInputColumn(inputSchema, chplan.RoleTimestamp)
	if inputSchema.Open || !hasTraceID || !hasTimestamp {
		return fmt.Errorf("%w: SearchTraceLimit input schema is open, ambiguous, or lacks named identity/timestamp roles", ErrUnsupported)
	}
	if n.TraceLimit <= 0 {
		// The lowering gates node construction on `limit > 0`
		// (internal/traceql/search_limit.go::stampSearchTraceLimit), so the
		// only path here is a programmer error in a lowering or rewrite.
		// Reject it: emitting the input unchanged would silently drop the
		// top-N restriction and drain every matching trace in the window.
		return fmt.Errorf("%w: SearchTraceLimit with non-positive TraceLimit=%d", ErrUnsupported, n.TraceLimit)
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
		Select(Col(traceID.Name)).
		From(innerSub).
		GroupBy(Col(traceID.Name)).
		OrderBy(Call("min", Col(timestamp.Name)), true).
		OrderBy(Col(traceID.Name), false).
		Limit(n.TraceLimit).
		Frag()

	sb := NewQuery().
		Select(verbatim("s.*")).
		From(aliasedFrag(outerSub, "s")).
		Where(InSubquery(Col(traceID.Name), topN))
	return e.emitSelect(sb)
}

func uniqueSearchTraceLimitInputColumn(schema chplan.Schema, role chplan.ColumnRole) (chplan.Column, bool) {
	var found chplan.Column
	seen := false
	for _, column := range schema.Columns {
		if column.Role != role {
			continue
		}
		if seen || column.Name == "" {
			return chplan.Column{}, false
		}
		found, seen = column, true
	}
	if !seen {
		return chplan.Column{}, false
	}
	for _, column := range schema.Columns {
		if column.Name == found.Name && column.Role != role {
			return chplan.Column{}, false
		}
	}
	return found, true
}
