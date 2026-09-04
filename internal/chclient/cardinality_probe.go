package chclient

import (
	"context"
	"fmt"
)

// cardinality_probe.go closes the chclient half of cerberus issue #2788: the
// round-trip helper the engine calls, for a ModeAuto-eligible candidate ONLY,
// to learn what a REAL (not estimated) bounded aggregate over the plan's
// already-pruned scan window reports about its row count and series
// cardinality, before committing to a fan-out factor K or a per-rung
// admission verdict. internal/engine's cardinality_probe_wiring.go is the
// other half — it gates, caches per (plan shape, metric), and skips the call
// entirely once the route memo or the per-rung admission learner already
// holds a verdict, mirroring explain_estimate_wiring.go's own three
// narrowings for the identical "no new live round-trip on every per-rung
// request" cost constraint per_rung_admission.go itself documents.
//
// Unlike EXPLAIN ESTIMATE (internal/chclient/explain_estimate.go), this DOES
// execute against real data — count(), uniqUpTo(100)(...) and
// uniqCombined64(...) over the caller-bounded scan window — so it answers a
// question EXPLAIN ESTIMATE's granule-resolution upper bound cannot: how
// many DISTINCT series actually back the window, not merely how many
// granules the index analysis could not prune. See docs/solver.md's
// "Bounded cardinality pre-probe" section for the
// two mechanisms' full division of labour.

// CardinalityEstimate is the parsed result of one bounded cardinality-probe
// statement: a real row count plus a real (up to the uniqUpTo cap) distinct
// series count over the plan's already-pruned scan window, plus an uncapped
// approximate distinct-series reading for when that cap saturates.
type CardinalityEstimate struct {
	// Rows is the exact count() of rows the probe's bounded window scanned —
	// unlike chclient.ScanEstimate.Rows (EXPLAIN ESTIMATE's granule-resolution
	// UPPER BOUND), this is a real post-execution count.
	Rows uint64
	// DistinctSeries is uniqUpTo(100)(...)'s exact distinct-series count, up
	// to 100 — see chplan.FnUniqUpTo's own doc for the saturation behaviour
	// above that cap (reported as 101, never silently wrong).
	DistinctSeries uint64
	// DistinctSeriesApprox is uniqCombined64(...)'s APPROXIMATE, uncapped
	// distinct-series count over the SAME window and series key — issue
	// #2840's answer to uniqUpTo(100) saturating at 101 on every dense real
	// window (test/perf/smoke/testdata/samples/): a consumer that needs to
	// know how far past the cap the true count lies reads this field once
	// DistinctSeries reports the saturation value, rather than treating 101
	// as if it were the true count. Always populated alongside DistinctSeries
	// — never a separate round trip — see chplan.FnUniqCombined64's own doc.
	DistinctSeriesApprox uint64
}

// ProbeCardinality runs sql — a bounded
// count()/uniqUpTo(100)(...)/uniqCombined64(...) aggregate internal/engine's
// cardinality_probe_wiring.go builds via chsql.Emit over the plan's
// already-pruned scan window — and returns the single summary row it
// produces. sql MUST already be a chsql.Emit-rendered statement (checked
// against chsql's own emit_size_bound ceiling), exactly like
// [Client.ExplainEstimate]'s own contract.
//
// Guarded by the circuit breaker (see [Client] doc) like every other query
// method here. Unlike ExplainEstimate, this DOES read data parts — it is a
// real, if narrowly bounded, execution — so callers MUST thread a strict
// per-call deadline (internal/engine's own cardinalityProbeTimeout) via
// [WithQueryTimeout] and a context deadline, so a slow or loaded cluster
// cannot stall an admission decision waiting on this optional signal.
//
// A probe whose bounded window matched no rows returns a zero-value
// CardinalityEstimate with a nil error — the caller (maybeSeedPerRungPrior's
// sibling in cardinality_probe_wiring.go) treats that identically to a
// genuinely near-empty window, which is the correct reading: no rows means
// no rows, whichever aggregate discovered it.
func (c *Client) ProbeCardinality(ctx context.Context, sql string, args ...any) (CardinalityEstimate, error) {
	if !c.br.allow() {
		return CardinalityEstimate{}, c.br.openErr("chclient: probe cardinality")
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return CardinalityEstimate{}, fmt.Errorf("chclient: probe cardinality: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out CardinalityEstimate
	var found bool
	for rows.Next() {
		// rowsFloat: chsql's emitAggregate wraps count() in toFloat64(...)
		// (internal/chsql/emit_node.go's intReturningAggregates) — the SAME
		// UInt64→Float64 guard every other count()-family column this plan
		// emitter produces already carries, so the `rows` column arrives as
		// Float64 here too. uniqUpTo / uniqCombined64 are NOT in that set
		// (neither is the column-scan case the guard exists for), so
		// `distinct_series` and `distinct_series_approx` both arrive as
		// ClickHouse's native UInt64. A real row count never approaches
		// float64's 2^53 exact-integer ceiling. Positional, in the SAME
		// (rows, distinct_series, distinct_series_approx) order
		// buildCardinalityProbePlan's AggFuncs list them.
		var rowsFloat float64
		if err := rows.Scan(&rowsFloat, &out.DistinctSeries, &out.DistinctSeriesApprox); err != nil {
			return CardinalityEstimate{}, fmt.Errorf("chclient: probe cardinality scan: %w", err)
		}
		out.Rows = uint64(rowsFloat)
		found = true
	}
	if err := rows.Err(); err != nil {
		return CardinalityEstimate{}, fmt.Errorf("chclient: probe cardinality rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	if !found {
		// The probe's own guarded-aggregate shape (chplan.Aggregate with
		// DropEmptyOnNoGroup) emits ZERO rows for an empty window rather than
		// ClickHouse's default one-row-of-zeros — see
		// cardinality_probe_wiring.go's buildCardinalityProbePlan doc. The
		// zero CardinalityEstimate is already the correct reading.
		return CardinalityEstimate{}, nil
	}
	return out, nil
}
