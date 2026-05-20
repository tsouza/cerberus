package chplan

// HistogramQuantileNative is the lowered form of PromQL's
// `histogram_quantile(phi, <native-histogram-selector>)` against the
// OTel-CH exponential (native) histogram table
// (`otel_metrics_exp_histogram`).
//
// The OTel exponential-histogram representation stores buckets at
// log-scale resolution rather than as explicit upper bounds:
//
//   - Scale (Int32): controls bucket resolution; base = 2^(2^-Scale).
//     Higher scale = finer buckets.
//   - PositiveOffset (Int32): the index of the first positive bucket.
//     PositiveBucketCounts[i] holds the count for the bucket whose
//     range is (base^(PositiveOffset+i), base^(PositiveOffset+i+1)].
//   - NegativeOffset / NegativeBucketCounts mirror the positive side
//     for negative observations (covering [-base^(idx+1), -base^idx)).
//   - ZeroCount (UInt64): observations at or below ZeroThreshold.
//   - ZeroThreshold (Float64): the upper edge of the zero bucket.
//
// The emitter walks Negative (reversed) → ZeroCount → Positive when
// building the cumulative-sum array so the quantile is taken over the
// natural ordering of the distribution. NegativeOffset /
// NegativeBucketCounts are required on the IR node; distributions with
// no negative observations carry empty arrays and the walk collapses
// to the original positive-only shape via arrayReverse([]) = [].
// ZeroCount is folded as the contiguous "zero band" between the
// negative and positive walks so quantiles landing in the zero
// segment linearly interpolate between -ZeroThreshold and
// +ZeroThreshold.
//
// IR contract (mirrors HistogramQuantile):
//
//   - Input produces rows surfacing the exp-histogram columns
//     (Scale, ZeroCount, PositiveOffset, PositiveBucketCounts,
//     NegativeOffset, NegativeBucketCounts). Typically Scan or
//     Filter over otel_metrics_exp_histogram.
//   - Phi is a scalar literal in [0, 1]; computed phi defers to RC3.
//   - GroupBy + GroupByAliases name the per-series projection from
//     the inner subquery. The emitter projects each `<expr> AS <alias>`
//     in the outer SELECT so the wrapping Sample projection can pick
//     them up by name.
//   - MetricNameColumn / AttributesColumn / TimestampColumn are
//     surfaced for the Sample wrapping in lowerHistogramQuantile.
//
// The chsql emitter renders the quantile-lookup as a CH expression
// chain over arrayCumSum / arrayFirstIndex on the PositiveBucketCounts
// array (offset by PositiveOffset), with log-scale midpoint estimation
// inside the selected bucket. Negative-side observations are out of
// scope for the v0.1 emitter and are explicitly excluded from the
// total; the lowering pre-flight rejects only the obviously-unsupported
// case (computed phi). Distributions that do contain negative samples
// will return a quantile over the positive subset only — documented
// in the package doc of internal/chsql/histogram_quantile_native.go.
type HistogramQuantileNative struct {
	Input Node
	Phi   float64

	ScaleColumn                string
	ZeroCountColumn            string
	ZeroThresholdColumn        string
	PositiveOffsetColumn       string
	PositiveBucketCountsColumn string
	NegativeOffsetColumn       string
	NegativeBucketCountsColumn string

	// GroupBy + GroupByAliases: same shape as HistogramQuantile.
	GroupBy        []Expr
	GroupByAliases []string

	// MetricNameColumn, AttributesColumn, TimestampColumn surfaced for
	// the Sample-row wrapping the caller applies downstream.
	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
}

func (*HistogramQuantileNative) planNode() {}

func (h *HistogramQuantileNative) Children() []Node { return []Node{h.Input} }

func (h *HistogramQuantileNative) Equal(other Node) bool {
	o, ok := other.(*HistogramQuantileNative)
	if !ok {
		return false
	}
	if h.Phi != o.Phi ||
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
