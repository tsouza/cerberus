//go:build chdb

// Package spec — eval-instant sweep harness (chDB-backed).
//
// This file is the reusable driver behind the eval-instant sweep test
// (`promql/eval_instant_sweep_test.go`). It closes the test gap that let
// the rc.8 instant range-vector window bug ship: every other chDB lane
// pins the eval instant to ONE fixed anchor the seeds are aligned to, so
// no test ever varies the requested `time=T` of an instant
// `/api/v1/query` RELATIVE to the sample timestamps. That swept axis is
// exactly where the bug lived — the window silently anchored to
// ClickHouse `now64(9)` wall-clock instead of `time=T`, so an instant
// query at an eval instant ~60-90s into continuous data returned EMPTY.
//
// The driver lowers an instant query at a caller-chosen `time=T` through
// the REAL pipeline — `promql.LowerAtRangeOpts(ctx, expr, schema, T, T,
// 0, …)` (start==end==T, step==0 — byte-identical to what the Prom
// `/api/v1/query` handler's `executeInstant` does) then `chsql.Emit` —
// and executes the emitted SQL against an ephemeral chDB session seeded
// with a continuous sample series. Because the post-fix window bound
// renders as an inline `toDateTime64(T…)` literal (NOT `now64`), varying
// T is a one-line change: pass a different T to the lowering. The driver
// reuses the package's existing chDB idioms (`openChDB`, `applySeed`,
// the Map-projection rewrites, `substituteNow64At`, `decodeCell`,
// `tolerantRowsErr`) so it stays in lock-step with the round-trip lane.
package spec

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// evalInstantResult is one swept-evaluation outcome: the (labels, value)
// rows the instant query produced at `time=T`. Labels are the decoded
// Map cell; Value is the float in the second projection slot.
type evalInstantResult struct {
	T    time.Time
	Rows []evalInstantRow
}

// evalInstantRow is a single (Attributes-map, Value) pair from the
// instant query's two-column projection (`SELECT Attributes, Value`).
type evalInstantRow struct {
	Labels map[string]any
	Value  float64
}

// NonEmpty reports whether the instant query returned at least one row.
func (r evalInstantResult) NonEmpty() bool { return len(r.Rows) > 0 }

// RowCount is the number of result rows (series) the query produced.
func (r evalInstantResult) RowCount() int { return len(r.Rows) }

// Scalar returns the single Value when the result has exactly one row,
// plus an ok flag. Used by the oracle to compare an instant result
// against the query_range value at the same step (both are single-series
// in the sweep's seed).
func (r evalInstantResult) Scalar() (float64, bool) {
	if len(r.Rows) != 1 {
		return 0, false
	}
	return r.Rows[0].Value, true
}

// EvalInstantResult is the exported handle the eval-instant sweep test
// (an external `_test` package) consumes. It wraps the unexported result
// so the driver's internals stay package-private while the test can
// assert NonEmpty / Scalar / RowCount.
type EvalInstantResult = evalInstantResult

// OpenChDBForSweep is the exported entrypoint the eval-instant sweep test
// uses to open the ephemeral chDB session (delegates to [openChDB]).
func OpenChDBForSweep(t *testing.T) *sql.DB { return openChDB(t) }

// ApplySeedForSweep applies a seed script to the sweep's chDB session
// (delegates to [applySeed], so the ResourceAttributes backfill +
// CREATE-OR-REPLACE idempotency apply identically to the round-trip
// lane).
func ApplySeedForSweep(t *testing.T, db *sql.DB, seed string) { applySeed(t, db, seed) }

// RunInstantSweep lowers `expr` as an instant query at `time=T`, emits
// the SQL, executes it against `db`, and returns the decoded result. It
// is the exported façade over [lowerEmitInstant] + [runInstant] for the
// external sweep test.
func RunInstantSweep(t *testing.T, db *sql.DB, expr parser.Expr, T time.Time) EvalInstantResult {
	t.Helper()
	sqlText, args := lowerEmitInstant(t, context.Background(), expr, T)
	return runInstant(t, db, sqlText, args, T)
}

// RunRangeStepSweep lowers `expr` as a single-step range query whose only
// grid point is `time=T` (step>0), emits the SQL, executes it, and
// returns the decoded result. Used for the instant==range cross-check.
func RunRangeStepSweep(t *testing.T, db *sql.DB, expr parser.Expr, T time.Time, step time.Duration) EvalInstantResult {
	t.Helper()
	sqlText, args := lowerEmitRangeStep(t, context.Background(), expr, T, step)
	return runInstant(t, db, sqlText, args, T)
}

// lowerEmitInstant lowers `expr` as an INSTANT query at `time=T`
// (start==end==T, step==0 — the executeInstant contract) and emits the
// ClickHouse SQL + bound args. It drives the SAME lowering + emit path
// the production Prom `/api/v1/query` handler uses, so the window-anchor
// behaviour under test is the real one, not a reconstruction.
func lowerEmitInstant(t *testing.T, ctx context.Context, expr parser.Expr, T time.Time) (string, []any) {
	t.Helper()
	plan, err := promql.LowerAtRangeOpts(ctx, expr, schema.DefaultOTelMetrics(), T, T, 0, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("lower instant @%s: %v", T.Format(time.RFC3339), err)
	}
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit instant @%s: %v", T.Format(time.RFC3339), err)
	}
	return sql, args
}

// lowerEmitRangeStep lowers `expr` as a single-step RANGE query whose
// only grid point is `time=T` (start==end==T, step>0 — the query_range
// contract). The result at that one step is the query_range value the
// oracle pins the instant value against: at a step-aligned T the instant
// and range answers MUST agree, which is the strongest cross-check that
// the instant window anchored where the range window did.
func lowerEmitRangeStep(t *testing.T, ctx context.Context, expr parser.Expr, T time.Time, step time.Duration) (string, []any) {
	t.Helper()
	plan, err := promql.LowerAtRangeOpts(ctx, expr, schema.DefaultOTelMetrics(), T, T, step, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("lower range-step @%s: %v", T.Format(time.RFC3339), err)
	}
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit range-step @%s: %v", T.Format(time.RFC3339), err)
	}
	return sql, args
}

// sweepServerNow is the deterministic stand-in for ClickHouse's
// `now64(9)` wall-clock (serverNow) the sweep splices in place of ANY
// residual now64 the emitted SQL still carries. It is deliberately far
// in the future from the seeded 2026-01-01 data — mirroring production,
// where serverNow is the live present while the queried samples are
// timestamped at eval-relative instants seconds-to-minutes in the past.
//
// This is the load-bearing choice that makes the sweep a real regression
// gate rather than a tautology: the now64 anchor must represent
// serverNow, NOT the swept eval instant T. With the rc.8 fix present the
// window bound renders as an inline `toDateTime64(T…)` literal — there is
// NO now64 in the window, so this anchor never touches the window and the
// `(T-5m, T]` window correctly overlaps the seeded series → non-empty.
// WITHOUT the fix the window bound renders as `now64(9)`; splicing
// sweepServerNow makes the window `(serverNow-5m, serverNow]` — years
// past the data — so the query returns EMPTY and the sweep FAILS. If this
// anchor were instead `chNow64Literal(T)` it would silently re-anchor the
// buggy window back onto T and mask the very bug under test (exactly the
// substituteNow64-pins-one-fixed-literal gap that let rc.8 ship).
var sweepServerNow = time.Date(2027, 6, 16, 12, 0, 0, 0, time.UTC)

// runInstant executes a lowered+emitted instant query against `db`,
// splicing [sweepServerNow] in place of any residual `now64(...)` via
// [substituteNow64At] so now64 faithfully evaluates to serverNow (not the
// eval instant T). With the rc.8 fix the window carries no now64, so this
// only governs an incidental wall-clock projection if one exists; without
// the fix it is what drives the window off the data. Returns the decoded
// (labels, value) rows.
//
// The query goes through the same Map-projection rewrite pipeline
// (`expandStarProjection` → `rewriteMapProjections` → `nestMapOrderBy`)
// the round-trip lane uses, so the Map column scans the same way here.
func runInstant(t *testing.T, db *sql.DB, sqlText string, args []any, T time.Time) evalInstantResult {
	t.Helper()
	query, queryArgs := substituteNow64At(sqlText, args, chNow64Literal(sweepServerNow))
	query = expandStarProjection(query)
	query = rewriteMapProjections(query)
	query = nestMapOrderBy(query)

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		t.Fatalf("instant query @%s failed:\n--- query ---\n%s\n--- err ---\n%v",
			T.Format(time.RFC3339), query, err)
	}
	defer func() { _ = rows.Close() }()

	// The instant range-vector projection is two columns: the Map
	// `Attributes` (rewritten server-side to toJSONString) and the
	// float `Value`.
	const projCols = 2
	out := evalInstantResult{T: T}
	for rows.Next() {
		cells := make([]any, projCols)
		ptrs := make([]any, projCols)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan instant row @%s: %v", T.Format(time.RFC3339), err)
		}
		labels, _ := decodeCell(cells[0], false).(map[string]any)
		val := toFloat64Cell(t, decodeCell(cells[1], false))
		out.Rows = append(out.Rows, evalInstantRow{Labels: labels, Value: val})
	}
	if err := tolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("instant rows.Err @%s: %v", T.Format(time.RFC3339), err)
	}
	return out
}

// toFloat64Cell coerces a decoded Value cell to float64. The chdb-go
// driver hands back float64 for Float64 projections; int families are
// widened defensively so a future projection-type change does not
// silently mis-read the Value.
func toFloat64Cell(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	default:
		t.Fatalf("Value cell is %T, want a numeric type: %#v", v, v)
		return 0
	}
}
