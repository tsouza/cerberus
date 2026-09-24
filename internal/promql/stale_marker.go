package promql

import (
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

// noRecordedValueFlag is the OTel data-point flag bit
// (pmetric.DataPointFlags.NoRecordedValue, `FLAG_NO_RECORDED_VALUE` in the
// OTLP proto) the collector's prometheusreceiver sets on a scraped stale
// marker.
const noRecordedValueFlag = 1

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
			Args: []chplan.Expr{&chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()}, &chplan.LitInt{V: noRecordedValueFlag}},
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
