package chplan

import "time"

// RangeWindowGridNativeInstant is the experimental, ClickHouse-native
// lowering of an INSTANT-mode (Step == 0, single evaluation anchor)
// `rate(<counter>[<Range>])` (and changes/resets/deriv/predict_linear)
// query. It is the instant-shaped sibling of [RangeWindowGridNative]:
// instead of feeding a materialised query_range grid to the
// timeSeries*ToGrid aggregate, it feeds a DEGENERATE one-point grid whose
// start and end both equal the query's single evaluation instant (Anchor)
// — the aggregate's own membership window collapses to exactly
// `(Anchor - Offset - Range, Anchor - Offset]`, matching a single anchor of
// [RangeWindowGridNative]'s matrix walk exactly, and its grid array is
// always exactly one element long (proven against a real ClickHouse
// 26.5.1.1 substrate — a start-equals-end grid returns length 1 regardless
// of the step parameter, which exists only because the aggregate's
// signature requires a nonzero one; see
// internal/chsql/range_window_grid_native_instant.go's own doc for the step
// literal it uses).
//
// The node is produced by the PromQL lowering ONLY when ALL of:
//
//   - The wired native strategy for Func is active AND its instant arm is
//     active (the ts_grid_instant feature, resolved from chopt.EnabledSet
//     and injected into the lowering as a RangeLowerers strategy field —
//     never read per query). ts_grid_instant is a pure narrowing of each
//     function's own existing matrix feature (ts_grid_range / ts_grid_changes
//     / ts_grid_resets / ts_grid_deriv / ts_grid_predict_linear): cmd/cerberus
//     only sets a Native*Lowerer's Instant field inside that function's own
//     "matrix feature enabled" branch, so a deployment that has not opted
//     into a function's native MATRIX arm never gets its instant arm either
//     — see chopt.FeatureTSGridInstant's own doc for the full rationale,
//     including why the version floor (26.5) is higher than the matrix
//     family's shared 25.9 floor.
//   - Func is one of "rate", "changes", "resets", "deriv", "predict_linear"
//     — the exact set cerberus issue #2748 scoped this to. "increase" and
//     "delta" are deliberately EXCLUDED: the issue defers their instant
//     coverage to a follow-up once their own native matrix features (already
//     landed as ts_grid_increase / ts_grid_delta) have separately earned an
//     instant arm — a scope decision, not a technical gap.
//   - The query is genuinely instant: Step == 0 and Anchor is pinned (the
//     gridSingleAnchor shape — see internal/promql's rangeGridShapeFor).
//     Range mode (Step > 0) is [RangeWindowGridNative]'s own territory and is
//     never eligible here.
//   - The inner relation is a plain Scan / Filter (optionally wrapped in the
//     canonical selector-attributes Project) — the same row-shape
//     [RangeWindowGridNative] requires.
//
// Every other shape lowers to the fan-out RangeWindow (Step == 0, the
// existing emitWindowedArrayExtrapolated / generic windowed-array path), so
// the default fan-out is structurally untouched when the feature is off.
//
// Row-shape contract. The emitter
// (internal/chsql/range_window_grid_native_instant.go) produces EXACTLY the
// row shape the fan-out's own instant emit produces: one row per series with
// columns [GroupBy..., Value] — NO per-row timestamp column at all (the
// instant HTTP response carries the query's own eval time externally, not a
// SQL column; see [RowShapeOf]'s ReducedWindowRowShape doc). A series whose
// window holds too few in-window samples (< 2 for rate/deriv/predict_linear,
// mirroring the matrix family's own NULL threshold) is absent — not a NULL
// row — matching PromQL's drop-series / staleness-gap semantics exactly.
//
// Required ClickHouse setting. Like [RangeWindowGridNative], a query
// carrying this node must run with
// `allow_experimental_time_series_aggregate_functions=1`; the engine detects
// the node in the emitted plan (planHasTSGridNative) and stamps that setting
// onto the per-query ClickHouse context.
type RangeWindowGridNativeInstant struct {
	Input Node

	// Func names the PromQL range function: "rate", "changes", "resets",
	// "deriv", or "predict_linear" — see internal/chsql.nativeTSGridFn for
	// the aggregate each maps to. Always one of these five; the lowering
	// never builds this node for "increase" or "delta" (see the type doc).
	Func string

	// Range is the PromQL matrix selector's lookback window — the 4th
	// (window_s) parametric arg of the timeSeries*ToGrid aggregate.
	Range time.Duration

	// Anchor is the query's single evaluation instant, BEFORE the Offset
	// shift — mirrors RangeWindow.End for the gridSingleAnchor shape. Always
	// pinned (non-zero) on this node.
	Anchor time.Time

	// Offset is the PromQL `offset` modifier: it shifts the aggregate's
	// membership window (both ends of the degenerate grid) back by Offset
	// without moving Anchor itself. Zero means no offset.
	Offset time.Duration

	// Column names on Input (canonical OTel-CH: Attributes / TimeUnix /
	// Value, plus MetricName only when GroupBy has been widened by the
	// name-collision guard). TimestampColumn / ValueColumn are the two
	// positional arguments of the aggregate's second paren group.
	TimestampColumn string
	ValueColumn     string

	// GroupBy is the per-series identity key — ordinarily just Attributes,
	// widened to also carry MetricName when the caller applies the
	// name-drop collision guard (mirrors RangeWindowGridNative.GroupBy).
	GroupBy []Expr

	// Scalars carries predict_linear's whole-second literal horizon t
	// (empty for rate/changes/resets/deriv). The native emitter threads it
	// into timeSeriesPredictLinearToGrid's 5th parametric arg.
	Scalars []float64
}

func (*RangeWindowGridNativeInstant) planNode() {}

func (r *RangeWindowGridNativeInstant) Children() []Node { return []Node{r.Input} }

// Equal compares two RangeWindowGridNativeInstant nodes field-by-field,
// mirroring [RangeWindowStaleResample.Equal]'s compact shape: a scalar-fields
// conjunction plus a recursive Input compare and a positional GroupBy /
// Scalars compare.
func (r *RangeWindowGridNativeInstant) Equal(other Node) bool {
	o, ok := other.(*RangeWindowGridNativeInstant)
	if !ok {
		return false
	}
	scalarsEqual := r.Func == o.Func && r.Range == o.Range &&
		r.Anchor.Equal(o.Anchor) && r.Offset == o.Offset &&
		r.TimestampColumn == o.TimestampColumn && r.ValueColumn == o.ValueColumn
	if !scalarsEqual {
		return false
	}
	if len(r.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i, g := range r.GroupBy {
		if !g.Equal(o.GroupBy[i]) {
			return false
		}
	}
	if len(r.Scalars) != len(o.Scalars) {
		return false
	}
	for i, v := range r.Scalars {
		if v != o.Scalars[i] {
			return false
		}
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}
