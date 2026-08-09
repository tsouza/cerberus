package chplan

// The Histogram*Column constants are the fixed output-column aliases
// [HistogramProjection]'s emitter gives its nine histogram fields — the
// IR-level contract between the emitter (which produces the columns)
// and a future decode-side consumer (chclient.Sample.Histogram) that
// reads them back by name, the same pattern [RangeWindowAnchorColumn]
// establishes for the matrix-window anchor.
//
// chclient may not import chplan (see .go-arch-lint.yml: chclient
// declares no internal dependencies), so its decode-side probe
// duplicates these exact string literals locally rather than importing
// this symbol — the same pairing this codebase already has between
// chclient's local `metadataColumn` constant and the literal "Metadata"
// alias internal/logql/lang.go emits. Changing a value here requires
// changing the matching literal in internal/chclient/cursor.go too.
const (
	HistogramCountColumn                = "HistogramCount"
	HistogramSumColumn                  = "HistogramSum"
	HistogramScaleColumn                = "HistogramScale"
	HistogramZeroThresholdColumn        = "HistogramZeroThreshold"
	HistogramZeroCountColumn            = "HistogramZeroCount"
	HistogramPositiveOffsetColumn       = "HistogramPositiveOffset"
	HistogramPositiveBucketCountsColumn = "HistogramPositiveBucketCounts"
	HistogramNegativeOffsetColumn       = "HistogramNegativeOffset"
	HistogramNegativeBucketCountsColumn = "HistogramNegativeBucketCounts"
)

// HistogramProjection projects the OTel exponential (native) histogram's
// raw structural columns — Count, Sum, Scale, ZeroThreshold, ZeroCount,
// PositiveOffset, PositiveBucketCounts, NegativeOffset,
// NegativeBucketCounts — as FINAL output columns of a query, instead of
// collapsing them into a scalar the way [HistogramQuantileNative]'s inner
// subtree does en route to a quantile.
//
// It exists because `rate()`, `increase()`, `sum()`, and a bare selector
// over a native histogram all return a HISTOGRAM-VALUED sample in
// reference Prometheus (see promql/functions.go's `extrapolatedRate`:
// `Sample{F: resultFloat, H: resultHistogram}`), and cerberus's plan IR
// had nowhere to put that answer — every existing node either emits a
// scalar Value or, like HistogramQuantileNative, folds the bucket
// structure away before it ever reaches a projection. This node is the
// shared shape a future rate()/sum()/bare-selector lowering (issue
// #1926 step 4, tracked separately — see docs/specs or the tracking
// issue referenced from the PR that added this node) will wrap its
// Input with once it exists, so its answer has a correct plan-level
// home from day one.
//
// No lowering builds this node yet. It is reachable only from
// hand-constructed test plans until #1926 step 4 lands; see
// internal/chclient.Sample.Histogram (the decode-side landing spot)
// and internal/api/prom's `histogram` / `histograms` wire keys (the
// HTTP-side landing spot) for the rest of the shared prerequisite.
//
// Column-name contract: every *Column field is REQUIRED except
// ZeroThresholdColumn, mirroring HistogramQuantileNative — an empty
// ZeroThresholdColumn means the physical schema does not persist the
// OTLP zero_threshold field (the default OTel-CH DDL doesn't), and the
// emitter projects a literal `0.` in its place so the OUTPUT shape
// (nine named histogram columns, always present under the
// Histogram*Column aliases) never varies with the input schema — a
// consumer decoding the result never needs to know whether the source
// table persisted a real zero-threshold.
//
// GroupBy + GroupByAliases: same shape as HistogramQuantileNative — the
// per-series projection from the inner subquery, aliased by the
// lowering to whatever column names it needs downstream (typically
// including the series' MetricName / Attributes / Timestamp).
//
// MetricNameColumn, AttributesColumn, TimestampColumn are bookkeeping
// surfaced for the wrapping Sample-row projection the caller applies
// downstream, mirroring the identically-named fields on
// HistogramQuantileNative — the emitter itself does not read them.
type HistogramProjection struct {
	Input Node

	CountColumn                string
	SumColumn                  string
	ScaleColumn                string
	ZeroCountColumn            string
	ZeroThresholdColumn        string
	PositiveOffsetColumn       string
	PositiveBucketCountsColumn string
	NegativeOffsetColumn       string
	NegativeBucketCountsColumn string

	GroupBy        []Expr
	GroupByAliases []string

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
}

func (*HistogramProjection) planNode() {}

func (h *HistogramProjection) Children() []Node { return []Node{h.Input} }

func (h *HistogramProjection) Equal(other Node) bool {
	o, ok := other.(*HistogramProjection)
	if !ok {
		return false
	}
	if h.CountColumn != o.CountColumn ||
		h.SumColumn != o.SumColumn ||
		h.ScaleColumn != o.ScaleColumn ||
		h.ZeroCountColumn != o.ZeroCountColumn ||
		h.ZeroThresholdColumn != o.ZeroThresholdColumn ||
		h.PositiveOffsetColumn != o.PositiveOffsetColumn ||
		h.PositiveBucketCountsColumn != o.PositiveBucketCountsColumn ||
		h.NegativeOffsetColumn != o.NegativeOffsetColumn ||
		h.NegativeBucketCountsColumn != o.NegativeBucketCountsColumn ||
		h.MetricNameColumn != o.MetricNameColumn ||
		h.AttributesColumn != o.AttributesColumn ||
		h.TimestampColumn != o.TimestampColumn ||
		len(h.GroupBy) != len(o.GroupBy) ||
		len(h.GroupByAliases) != len(o.GroupByAliases) {
		return false
	}
	for i := range h.GroupBy {
		if !h.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	for i := range h.GroupByAliases {
		if h.GroupByAliases[i] != o.GroupByAliases[i] {
			return false
		}
	}
	if h.Input == nil || o.Input == nil {
		return h.Input == o.Input
	}
	return h.Input.Equal(o.Input)
}
