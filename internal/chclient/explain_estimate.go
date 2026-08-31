package chclient

import (
	"context"
	"fmt"
)

// explain_estimate.go closes half of cerberus issue #2787: the round-trip
// helper the engine calls, for a ModeAuto-eligible candidate ONLY, to learn
// what ClickHouse's own index analysis already knows about a plan's inner
// scan before committing to a fan-out factor K. internal/engine's
// explain_estimate_wiring.go is the other half — it gates, caches per plan
// shape, and skips the call entirely once the route memo or the per-rung
// admission learner already holds a verdict, so this never becomes "a new
// live round-trip on every per-rung request" (the cost per_rung_admission.go
// itself documents rejecting).
//
// ScanEstimate is GRANULE-RESOLUTION, not selectivity-aware: EXPLAIN
// ESTIMATE reports how many marks the index analysis could NOT prune, times
// the table's granule size — an UPPER BOUND on matching rows, never an exact
// count. Callers must treat it as an advisory bias input to K-clamping, never
// as a correctness signal — see internal/solver/planner.go's own doc on
// where it is consumed.

// ScanEstimate is the parsed, summed result of one EXPLAIN ESTIMATE
// statement: ClickHouse's no-execution scan estimator (parts / rows / marks
// after index analysis), available since ClickHouse 21.9. A statement
// touching more than one table (a UnionAll spine) reports one row per table;
// every field here is the SUM across those rows, matching how the solver's
// own cost grid already reasons about a plan's total scan, not per-table.
type ScanEstimate struct {
	// Parts is the number of parts the estimate covers.
	Parts uint64
	// Rows is ClickHouse's own upper-bound row estimate: selected marks times
	// the table's granule size (typically 8192), NOT a selectivity-aware
	// count. See this file's own doc.
	Rows uint64
	// Marks is the number of granules the index analysis selected.
	Marks uint64
}

// explainEstimatePrefix is a fixed, compile-time SQL keyword literal — it
// carries no plan- or request-derived data, unlike the statement it
// prefixes. internal/chsql's typed Frag pipeline (invariant 10) builds THAT
// inner statement; EXPLAIN ESTIMATE is a statement-level modifier ClickHouse
// recognizes only as a leading keyword, not an expression the Frag system
// has vocabulary for, so it is prefixed here rather than threaded through
// chsql — mirroring this package's own existing pattern of hand-written
// literal SQL for infra-level probes (ts_grid_probe.go's
// tsGridCapabilityProbeSQL), never internal/chsql's expression-tree API.
const explainEstimatePrefix = "EXPLAIN ESTIMATE "

// ExplainEstimate runs ClickHouse's own no-execution scan estimator over
// sql, summing every returned (database, table, parts, rows, marks) row into
// one ScanEstimate. sql MUST be a statement chsql.Emit already rendered (and
// therefore already checked against chsql's own emit_size_bound /
// CERBERUS_CH_MAX_EMITTED_SQL_BYTES ceiling, internal/chsql/
// emit_size_bound.go) — this renders that SAME statement a second time under
// the EXPLAIN ESTIMATE prefix, which adds a fixed, negligible number of bytes
// against that ceiling and performs no query execution of its own.
//
// Guarded by the circuit breaker (see [Client] doc), exactly like every other
// query method here: EXPLAIN ESTIMATE still does real analysis work against
// ClickHouse (parses the statement and consults the index), it just never
// reads a data part.
func (c *Client) ExplainEstimate(ctx context.Context, sql string, args ...any) (ScanEstimate, error) {
	if !c.br.allow() {
		return ScanEstimate{}, c.br.openErr("chclient: explain estimate")
	}
	ctx = c.queryContext(ctx)
	explainSQL := explainEstimatePrefix + sql
	ctx, span := startExecuteSpan(ctx, explainSQL, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, explainSQL, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return ScanEstimate{}, fmt.Errorf("chclient: explain estimate: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out ScanEstimate
	for rows.Next() {
		var database, table string
		var parts, r, marks uint64
		if err := rows.Scan(&database, &table, &parts, &r, &marks); err != nil {
			return ScanEstimate{}, fmt.Errorf("chclient: explain estimate scan: %w", err)
		}
		out.Parts += parts
		out.Rows += r
		out.Marks += marks
	}
	if err := rows.Err(); err != nil {
		return ScanEstimate{}, fmt.Errorf("chclient: explain estimate rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}
