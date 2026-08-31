package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeGridInstantStepSeconds is the step parameter threaded into the
// degenerate one-point grid this emitter builds (both the aggregate's own
// (start, end, step, window) parametric tuple and the parallel
// timeSeriesRange axis it would use if it needed one). Its VALUE is
// arbitrary and load-bearing only in being positive: with start == end,
// ClickHouse's timeSeries*ToGrid family and timeSeriesRange both return
// exactly one grid point regardless of step (proven against a real
// ClickHouse 26.5.1.1 substrate — step=1, step=300 and step=99999 all
// produced the identical single-element result on the same fixture; see
// cerberus issue #2748). 1 is chosen as the smallest legal literal, so nothing
// downstream needs to reason about its magnitude.
const nativeGridInstantStepSeconds = 1

// emitRangeWindowGridNativeInstant renders a
// chplan.RangeWindowGridNativeInstant — the instant-mode sibling of
// [emitter.emitRangeWindowGridNative]. It feeds the SAME timeSeries*ToGrid
// aggregate a DEGENERATE one-point grid (start == end == r.Anchor) instead of
// the materialised query_range grid, so the family's flat-memory native
// aggregation applies to a bare instant query exactly as it already does to
// a query_range one:
//
//	SELECT <group cols>,
//	       toFloat64(assumeNotNull(grid_val)) AS <ValueColumn>
//	FROM (
//	  SELECT <group cols>,
//	         timeSeries<Fn>ToGrid(<anchor>, <anchor>, 1, <window_s>)(<ts>, <val>) AS grid
//	  FROM (<inner Scan/Filter>)
//	  GROUP BY <group cols>
//	)
//	ARRAY JOIN grid AS grid_val
//	WHERE grid_val IS NOT NULL
//
// Unlike the matrix emitter's outer SELECT, there is no parallel
// timeSeriesRange anchor axis and no anchor_ts column at all: the instant
// row shape ([chplan.ReducedWindowRowShape], see RowShapeOf) has no per-row
// timestamp to publish — the query's single eval instant is reported
// externally by the HTTP layer, exactly as the fan-out's own instant emit
// (RangeWindow with OuterRange == 0) already does. `ARRAY JOIN grid AS
// grid_val` still explodes the (always length-1) array so the `WHERE
// grid_val IS NOT NULL` filter turns a NULL cell (< 2 in-window samples, or
// — for changes/resets — 0 samples) into an ABSENT row, matching PromQL's
// drop-series / staleness-gap contract ("no sample -> no row") exactly.
//
// Required ClickHouse setting: identical to the matrix emitter — the engine
// detects this node via planHasTSGridNative and stamps
// allow_experimental_time_series_aggregate_functions=1 on the query.
func (e *emitter) emitRangeWindowGridNativeInstant(r *chplan.RangeWindowGridNativeInstant) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindowGridNativeInstant.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindowGridNativeInstant.ValueColumn unset", ErrUnsupported)
	}
	if r.Anchor.IsZero() {
		return fmt.Errorf("%w: RangeWindowGridNativeInstant requires Anchor to be pinned (instant mode)", ErrUnsupported)
	}
	agg, ok := nativeTSGridFn[r.Func]
	if !ok {
		return fmt.Errorf("%w: RangeWindowGridNativeInstant func %q (supported: rate, changes, resets, deriv, predict_linear)", ErrUnsupported, r.Func)
	}
	if r.Func == "increase" || r.Func == "delta" {
		// The lowering never builds an instant node for these — see the
		// chplan type's own doc for the scope exclusion — but the emitter
		// fails loudly rather than silently accepting a shape nothing has
		// differentially proven, should a future caller reach here anyway.
		return fmt.Errorf("%w: RangeWindowGridNativeInstant func %q is out of scope (instant coverage excludes increase/delta — cerberus issue #2748)", ErrUnsupported, r.Func)
	}
	if r.Func == "predict_linear" {
		if len(r.Scalars) != 1 {
			return fmt.Errorf("%w: RangeWindowGridNativeInstant predict_linear requires exactly 1 scalar (t), got %d", ErrUnsupported, len(r.Scalars))
		}
	} else if len(r.Scalars) != 0 {
		return fmt.Errorf("%w: RangeWindowGridNativeInstant func %q takes no scalar, got %d", ErrUnsupported, r.Func, len(r.Scalars))
	}

	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Both grid bounds equal the SAME offset-shifted anchor, collapsing the
	// aggregate's membership window to exactly
	// (Anchor - Offset - Range, Anchor - Offset] — a single anchor of the
	// matrix emitter's own walk. See nativeGridInstantStepSeconds for why the
	// step argument's value does not matter here.
	offsetNS := r.Offset.Nanoseconds()
	anchorFrag := nativeGridTimeBoundFrag(r.Anchor, offsetNS)
	windowSeconds := int64(r.Range.Seconds())
	gridParams := []Frag{anchorFrag, anchorFrag, InlineLit(int64(nativeGridInstantStepSeconds)), InlineLit(windowSeconds)}
	if r.Func == "predict_linear" {
		gridParams = append(gridParams, InlineLit(int64(r.Scalars[0])))
	}
	tsAxis := nativeGridTsAxisFrag(r.Func, r.TimestampColumn)

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	inner := NewQuery().From(innerSub)
	inner.Select(groupFrags...)
	inner.Select(As(Parametric(agg.Fn, gridParams, tsAxis, Col(r.ValueColumn)), nativeGridArrayAlias))
	// Prune the inner scan to the SAME single-window bound the matrix
	// emitter uses (Anchor for both the start and end of the pruning span),
	// so ClickHouse skips granules outside the eval window instead of
	// scanning the series' full retention — the memory/perf win this
	// feature exists for (cerberus issue #2748).
	maybePushRangeScanTimeBound(inner, r.TimestampColumn, r.Anchor, r.Anchor, offsetNS, r.Range.Nanoseconds())
	inner.GroupBy(groupFrags...)

	outer := NewQuery().From(inner.Frag())
	outer.Select(groupFrags...)
	outer.Select(As(nativeGridValueExprFor(r.Func, r.Range.Seconds()), r.ValueColumn))
	outer.ArrayJoin(As(Col(nativeGridArrayAlias), nativeGridValAlias))
	outer.Where(IsNotNull(Col(nativeGridValAlias)))

	return e.emitSelect(outer)
}
