package promql

import (
	"slices"
	"time"

	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// A Prometheus stale marker reaches the OTel ClickHouse schema as a data
// point whose Flags column carries the OTel NoRecordedValue bit and whose
// Value column holds the exporter's empty-value placeholder, 0. Read as an
// ordinary sample that placeholder is a real 0: a counter reset for rate /
// increase, a spurious point for a gauge, and a live series for the whole
// instant lookback after its target disappeared.
//
// The PromQL lowering applies Prometheus's two stale-marker rules instead:
//
//   - a range selection (`x[5m]`, every range function, every range-vector
//     consumer) never contains a stale marker — the marker row is dropped at
//     the scan, before any window sees it;
//   - an instant selection (the latest sample within the lookback, in
//     instant mode, range mode, under an absolute `@`, and at each inner
//     step of a subquery over a bare selector) produces no sample
//     for a series whose latest sample is a stale marker — the marker takes
//     part in the latest-sample pick, then the series is dropped at that
//     step when the marker wins.
//
// For the instant rule the marker has to survive the latest-sample pick,
// which sees only the canonical Sample quadruple (MetricName, Attributes,
// TimeUnix, Value). So the arm that reads a raw metric table rewrites a
// marker row's Value to Prometheus's own stale-marker NaN,
// [value.StaleNaN], bit for bit, and the latest-sample wrappers drop every
// row whose picked Value carries exactly those bits. An ordinary NaN sample
// has a different bit pattern and stays an ordinary NaN sample.

// A histogram row has no scalar Value to encode a marker into, so the
// histogram lowerings apply the same two rules to the Flags column itself:
//
//   - a range selection drops the marker row at the scan
//     ([withHistogramRangeStaleDrop]);
//   - an instant selection keeps the marker row in the newest-row pick and
//     drops the series when the marker is the row it picks: a HAVING over
//     `argMax(Flags, TimeUnix)` on an Aggregate ([staleLatestHaving]), or
//     the same aggregate carried as a column of a RangeBucketFanout and
//     filtered above it ([withStaleLatestAgg], [dropStaleLatestHistograms]).
//
// The `_bucket` fan-out is the exception: it turns a histogram row into
// float Sample rows before the latest-sample pick, so a marker row there is
// fanned into one encoded row per bucket bound its series carried
// ([staleMarkerBucketLayout]).

// staleMarkerMode is how a selector arm treats stale-marker rows.
type staleMarkerMode int

const (
	// staleMarkersIgnored reads the raw rows unchanged. It applies to a
	// schema whose Flags column was not established on every metric table
	// (schema.Metrics.StaleMarkerFlagsColumn) and to metadata enumeration,
	// where a series whose only in-window sample is a stale marker still
	// exists.
	staleMarkersIgnored staleMarkerMode = iota
	// staleMarkersDropped removes stale-marker rows at the scan: the range
	// selection rule.
	staleMarkersDropped
	// staleMarkersEncoded rewrites a stale-marker row's Value to
	// [value.StaleNaN] so the latest-sample pick can see it: the instant
	// selection rule.
	staleMarkersEncoded
)

// staleMarkerMode resolves the stale-marker treatment for a selector
// lowered under c against schema s.
func (c lowerCtx) staleMarkerMode(s schema.Metrics) staleMarkerMode {
	switch {
	case s.StaleMarkerFlagsColumn() == "", c.metadataFullRange, c.catalog != nil:
		return staleMarkersIgnored
	case c.inRangeVector && !c.latestSampleWindow:
		return staleMarkersDropped
	default:
		return staleMarkersEncoded
	}
}

// staleMarkerFlagSet is true when flags carries the NoRecordedValue bit.
func staleMarkerFlagSet(flags chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpNe,
		Left: &chplan.FuncCall{
			Fn:   chplan.FnBitAnd,
			Args: []chplan.Expr{flags, &chplan.LitInt{V: schema.NoRecordedValueFlag}},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// staleMarkerRowExpr is true on a raw metric row that carries the
// NoRecordedValue flag.
func staleMarkerRowExpr(s schema.Metrics) chplan.Expr {
	return staleMarkerFlagSet(&chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()})
}

// staleMarkerBitsExpr is [value.StaleNaN]'s bit pattern as a literal. The
// pattern sits below 2^63, so it is exact as an int64.
func staleMarkerBitsExpr() chplan.Expr {
	return &chplan.LitInt{V: int64(value.StaleNaN)}
}

// withStaleMarkerDrop conjoins the range-selection rule onto a raw-scan
// predicate under staleMarkersDropped; any other mode returns pred as is.
// pred may be nil.
func withStaleMarkerDrop(pred chplan.Expr, mode staleMarkerMode, s schema.Metrics) chplan.Expr {
	if mode != staleMarkersDropped {
		return pred
	}
	keep := notStaleMarkerFlags(&chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()})
	if pred == nil {
		return keep
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: keep}
}

// staleMarkerValueExpr is the Value an arm projects from a raw metric row:
// valueExpr itself, or under staleMarkersEncoded, [value.StaleNaN] on a
// stale-marker row.
func staleMarkerValueExpr(valueExpr chplan.Expr, mode staleMarkerMode, s schema.Metrics) chplan.Expr {
	if mode != staleMarkersEncoded {
		return valueExpr
	}
	return &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			staleMarkerRowExpr(s),
			&chplan.FuncCall{Fn: chplan.FnReinterpretAsFloat64, Args: []chplan.Expr{staleMarkerBitsExpr()}},
			valueExpr,
		},
	}
}

// dropStaleLatestSamples is the instant-selection rule's second half: it
// drops every row of a latest-sample collapse whose picked Value is a
// stale marker, so the series has no sample at that step. It is the
// identity for a schema without an established Flags column, whose arms
// never encode a marker.
func dropStaleLatestSamples(latest chplan.Node, s schema.Metrics) chplan.Node {
	if s.StaleMarkerFlagsColumn() == "" {
		return latest
	}
	return &chplan.Filter{
		Input: latest,
		Predicate: &chplan.Binary{
			Op: chplan.OpNe,
			Left: &chplan.FuncCall{
				Fn:   chplan.FnReinterpretAsUInt64,
				Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			},
			Right: staleMarkerBitsExpr(),
		},
	}
}

// withHistogramRangeStaleDrop conjoins the range-selection rule onto the
// raw-scan predicate of a histogram lowering that reads a range selection:
// marker rows never reach the window. pred may be nil.
func withHistogramRangeStaleDrop(pred chplan.Expr, s schema.Metrics) chplan.Expr {
	if s.StaleMarkerFlagsColumn() == "" {
		return pred
	}
	return withStaleMarkerDrop(pred, staleMarkersDropped, s)
}

// histogramInstantStaleMode is the stale-marker mode of a histogram arm
// that projects a float Value per raw row for an instant selection:
// staleMarkersEncoded, or staleMarkersIgnored for a schema without an
// established Flags column.
func histogramInstantStaleMode(s schema.Metrics) staleMarkerMode {
	if s.StaleMarkerFlagsColumn() == "" {
		return staleMarkersIgnored
	}
	return staleMarkersEncoded
}

// staleLatestFlagsExpr is the Flags of the newest row in a group:
// `argMax(Flags, TimeUnix)`.
func staleLatestFlagsExpr(s schema.Metrics) *chplan.FuncCall {
	return &chplan.FuncCall{
		Fn: chplan.FnArgMax,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
	}
}

// notStaleMarkerFlags is true when flags does not carry the
// NoRecordedValue bit.
func notStaleMarkerFlags(flags chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{staleMarkerFlagSet(flags)}}
}

// staleLatestHaving is the HAVING of a newest-row-per-series Aggregate over
// raw histogram rows: it drops the group whose newest row is a stale
// marker. It is nil for a schema without an established Flags column.
func staleLatestHaving(s schema.Metrics) chplan.Expr {
	if s.StaleMarkerFlagsColumn() == "" {
		return nil
	}
	return notStaleMarkerFlags(staleLatestFlagsExpr(s))
}

// staleLatestFlagsAlias names the newest-row Flags a latest-pick
// RangeBucketFanout carries for [dropStaleLatestHistograms].
const staleLatestFlagsAlias = "stale_latest_flags"

// withStaleLatestAgg appends the newest-row Flags aggregate to the
// aggregates of a newest-row-per-(series, anchor) RangeBucketFanout. It
// returns aggs unchanged for a schema without an established Flags column.
func withStaleLatestAgg(aggs []chplan.AggFunc, s schema.Metrics) []chplan.AggFunc {
	if s.StaleMarkerFlagsColumn() == "" {
		return aggs
	}
	latest := staleLatestFlagsExpr(s)
	return append(slices.Clone(aggs), chplan.AggFunc{Fn: latest.Fn, Args: latest.Args, Alias: staleLatestFlagsAlias})
}

// dropStaleLatestHistograms drops every (series, anchor) row of a
// fan-out built with [withStaleLatestAgg] whose newest row is a stale
// marker. It is the identity for a schema without an established Flags
// column.
func dropStaleLatestHistograms(fanout chplan.Node, s schema.Metrics) chplan.Node {
	if s.StaleMarkerFlagsColumn() == "" {
		return fanout
	}
	return &chplan.Filter{
		Input:     fanout,
		Predicate: notStaleMarkerFlags(&chplan.ColumnRef{Name: staleLatestFlagsAlias}),
	}
}

// staleMarkerBucketLayout gives each stale-marker row of a classic
// histogram scan the bucket layout of its series, so the `_bucket` fan-out
// turns the marker into one row per bucket bound instead of into none. A
// marker row carries empty BucketCounts / ExplicitBounds; it takes the
// sorted union of every bound its series carried in the scanned rows
// (`groupUniqArrayArray` over a window partitioned by series), and
// BucketCounts sized to that layout plus the `+Inf` bucket. The fan-out
// reads the arrays from [staleLayoutBoundsAlias] / [staleLayoutCountsAlias]
// and writes the encoded marker ([staleMarkerValueExpr]) as each fanned
// row's Value, so each `le` series ends at the marker in the latest-sample
// pick. Every other row passes through unchanged.
//
// The plan is three stages:
//
//	Project [rows..., stale_layout_bounds, stale_layout_counts]   -- the window
//	  Project [rows..., ExplicitBounds, BucketCounts]              -- row columns
//	    Filter bound
//	      input
//
// rows are the fan-out's own row projections, evaluated once in the middle
// stage so the window stage reads them by name; the window publishes its
// arrays under names of their own because an alias that shadows the column
// its own window reads is a cyclic reference to ClickHouse 24.8.
//
// bound is the time window the consumer reads ([staleLayoutBoundFor]). It
// filters the rows beneath the window, because ClickHouse does not push a
// filter on a non-partition column through a window step: applied only
// above it, the scan would read the metric's whole history.
func staleMarkerBucketLayout(input chplan.Node, rows []chplan.Projection, bound chplan.Expr, s schema.Metrics) chplan.Node {
	rowStage := &chplan.Project{
		Roles: metricRoles(s),
		Input: &chplan.Filter{Input: input, Predicate: bound},
		Projections: append(
			slices.Clone(rows),
			chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
		),
	}
	bounds := &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}
	counts := &chplan.ColumnRef{Name: s.BucketCountsColumn}
	seriesBounds := &chplan.FuncCall{
		Fn: chplan.FnArraySort,
		Args: []chplan.Expr{&chplan.WindowExpr{
			Fn:   chplan.FnGroupUniqArrayArray,
			Args: []chplan.Expr{bounds},
			PartitionBy: []chplan.Expr{
				&chplan.ColumnRef{Name: s.MetricNameColumn},
				&chplan.ColumnRef{Name: s.AttributesColumn},
			},
		}},
	}
	markerCounts := &chplan.FuncCall{
		Fn: chplan.FnArrayResize,
		Args: []chplan.Expr{
			counts,
			addExpr(&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{seriesBounds}}, &chplan.LitInt{V: 1}),
		},
	}
	marker := staleMarkerRowExpr(s)
	projections := make([]chplan.Projection, 0, len(rows)+2)
	for _, p := range rows {
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: p.Alias}, Alias: p.Alias})
	}
	projections = append(
		projections,
		chplan.Projection{
			Expr:  &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{marker, seriesBounds, bounds}},
			Alias: staleLayoutBoundsAlias,
		},
		chplan.Projection{
			Expr:  &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{marker, markerCounts, counts}},
			Alias: staleLayoutCountsAlias,
		},
	)
	return &chplan.Project{Roles: metricRoles(s), Input: rowStage, Projections: projections}
}

// staleLayoutBoundsAlias / staleLayoutCountsAlias name the marker-aware
// ExplicitBounds / BucketCounts [staleMarkerBucketLayout] publishes.
const (
	staleLayoutBoundsAlias = "stale_layout_bounds"
	staleLayoutCountsAlias = "stale_layout_counts"
)

// staleMarkerLayoutBoundFilter returns the bound Filter beneath a layout
// stage [staleMarkerBucketLayout] built, and whether p is one.
func staleMarkerLayoutBoundFilter(p *chplan.Project) (*chplan.Filter, bool) {
	if len(p.Projections) == 0 || p.Projections[len(p.Projections)-1].Alias != staleLayoutCountsAlias {
		return nil, false
	}
	rowStage, ok := p.Input.(*chplan.Project)
	if !ok {
		return nil, false
	}
	f, ok := rowStage.Input.(*chplan.Filter)
	return f, ok
}

// staleLayoutWindowBound is the literal time bound [lo, hi] on col.
func staleLayoutWindowBound(col string, lo, hi time.Time) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: col}, Right: metadataBoundExpr(lo)},
		Right: &chplan.Binary{Op: chplan.OpLe, Left: &chplan.ColumnRef{Name: col}, Right: metadataBoundExpr(hi)},
	}
}

// staleLayoutAnchoredBound is the time bound `(anchor - lookback, anchor]`
// on col, shifted by the anchor's offset.
func staleLayoutAnchoredBound(col string, anchor evalAnchor, lookback time.Duration) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  stalenessLowerBoundExpr(col, anchor, lookback),
		Right: timeBoundExpr(col, anchor),
	}
}

// staleLayoutBoundFor is the time window a `_bucket` selector's consumer
// reads, which bounds the stale-marker layout window beneath the fan-out:
//
//   - an instant selection, or a range-mode selection pinned by an absolute
//     `@`: the lookback ending at the selector's anchor;
//   - a range-mode selection: every step's lookback, from the first step
//     (less one step, for an epoch-aligned grid) to the last;
//   - a subquery's inner selection: [staleLayoutPendingBound], which the
//     subquery lowering replaces with its Identity window's input window
//     ([boundStaleMarkerLayoutsToIdentity]).
func staleLayoutBoundFor(v *parser.VectorSelector, ctx lowerCtx, s schema.Metrics) (chplan.Expr, error) {
	if ctx.inRangeVector {
		return staleLayoutPendingBound(), nil
	}
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}
	if ctx.rangeMode() && !hasAbsoluteAt(v) {
		lo := ctx.start.UTC().Add(-anchor.Offset - instantLookback - ctx.step)
		hi := ctx.end.UTC().Add(-anchor.Offset)
		return staleLayoutWindowBound(s.TimestampColumn, lo, hi), nil
	}
	return staleLayoutAnchoredBound(s.TimestampColumn, anchor, instantLookback), nil
}

// staleLayoutPendingBound is the placeholder bound of a layout whose
// consumer is a subquery's Identity window: that window is built after its
// inner selection, and [boundStaleMarkerLayoutsToIdentity] replaces the
// placeholder with the window's input bound.
func staleLayoutPendingBound() chplan.Expr {
	return &chplan.LitBool{V: true}
}

// boundStaleMarkerLayoutsToIdentity bounds every stale-marker bucket layout
// beneath a subquery's Identity window to the window's input:
// `(End - Offset - OuterRange - Range - Step, End - Offset]`, the step
// covering the epoch-aligned inner grid. [widenSubquerySpine] re-bounds it
// once the grid is widened onto the query range.
func boundStaleMarkerLayoutsToIdentity(rw *chplan.RangeWindow) {
	anchor := evalAnchor{End: rw.End, Offset: rw.Offset}
	rebindStaleMarkerLayouts(rw.Input, staleLayoutAnchoredBound(rw.TimestampColumn, anchor, rw.OuterRange+rw.Range+rw.Step))
}

// rebindStaleMarkerLayouts re-bounds every stale-marker bucket layout on
// n's selector spine to bound — the input window of the subquery Identity
// window n feeds ([boundStaleMarkerLayoutsToIdentity], [widenSubquerySpine]). The walk stops at a windowed node
// ([chplan.GridCarrier]): a nested window owns its own input.
func rebindStaleMarkerLayouts(n chplan.Node, bound chplan.Expr) {
	if _, windowed := n.(chplan.GridCarrier); windowed {
		return
	}
	if p, ok := n.(*chplan.Project); ok {
		if f, ok := staleMarkerLayoutBoundFilter(p); ok {
			f.Predicate = bound
			return
		}
	}
	for _, child := range n.Children() {
		rebindStaleMarkerLayouts(child, bound)
	}
}
