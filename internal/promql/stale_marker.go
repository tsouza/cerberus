package promql

import (
	"slices"

	"github.com/prometheus/prometheus/model/value"

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

// staleMarkerRowExpr is true on a raw metric row that carries the
// NoRecordedValue flag.
func staleMarkerRowExpr(s schema.Metrics) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpNe,
		Left: &chplan.FuncCall{
			Fn:   chplan.FnBitAnd,
			Args: []chplan.Expr{&chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()}, &chplan.LitInt{V: schema.NoRecordedValueFlag}},
		},
		Right: &chplan.LitInt{V: 0},
	}
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
	keep := &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{staleMarkerRowExpr(s)}}
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
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn:   chplan.FnBitAnd,
			Args: []chplan.Expr{flags, &chplan.LitInt{V: schema.NoRecordedValueFlag}},
		},
		Right: &chplan.LitInt{V: 0},
	}
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
// sorted union of every bound its series carried in the scanned rows, and
// BucketCounts sized to that layout plus the `+Inf` bucket. The fanned rows'
// Value is the encoded marker ([staleMarkerValueExpr]), so each `le` series
// ends at the marker in the latest-sample pick. Every other row passes
// through unchanged.
func staleMarkerBucketLayout(input chplan.Node, s schema.Metrics) chplan.Node {
	bounds := &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}
	seriesBounds := &chplan.FuncCall{
		Fn: chplan.FnArraySort,
		Args: []chplan.Expr{&chplan.FuncCall{
			Fn: chplan.FnArrayDistinct,
			Args: []chplan.Expr{&chplan.FuncCall{
				Fn: chplan.FnArrayFlatten,
				Args: []chplan.Expr{&chplan.WindowExpr{
					Fn:   chplan.FnGroupArray,
					Args: []chplan.Expr{bounds},
					PartitionBy: []chplan.Expr{
						&chplan.ColumnRef{Name: s.MetricNameColumn},
						histogramIdentityExpr(s),
					},
				}},
			}},
		}},
	}
	markerCounts := &chplan.FuncCall{
		Fn: chplan.FnArrayResize,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.BucketCountsColumn},
			addExpr(&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{seriesBounds}}, &chplan.LitInt{V: 1}),
		},
	}
	marker := staleMarkerRowExpr(s)
	return &chplan.Project{
		Roles: metricRoles(s),
		Input: input,
		Replacements: []chplan.Projection{
			{
				Expr:  &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{marker, markerCounts, &chplan.ColumnRef{Name: s.BucketCountsColumn}}},
				Alias: s.BucketCountsColumn,
			},
			{
				Expr:  &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{marker, seriesBounds, bounds}},
				Alias: s.ExplicitBoundsColumn,
			},
		},
	}
}
