package chplan

// HistogramQuantile is the lowered form of PromQL's
// `histogram_quantile(phi, <classic-histogram-selector>)` against the
// OTel-CH classic-histogram table.
//
// In the OTel-CH schema, a single histogram row holds the bucket counts
// (BucketCountsColumn, Array(UInt64)) parallel to the explicit upper
// bounds (ExplicitBoundsColumn, Array(Float64)); len(BucketCounts) =
// len(ExplicitBounds)+1 because the trailing bucket is the +Inf overflow.
// This is the inverse of Prometheus's "one row per bucket carrying an `le`
// label" representation; the cerberus emitter therefore does the
// linear-interpolation arithmetic directly over the parallel arrays
// rather than asking the user to pre-aggregate by `le`.
//
// IR contract:
//
//   - Input produces rows that surface BucketCountsColumn and
//     ExplicitBoundsColumn as Array columns (typically a Scan or Filter
//     over the classic-histogram table; for `sum by(...)` aggregation,
//     a chplan.Aggregate that element-wise-sums the arrays via
//     `sumForEach` and groups by GroupBy).
//   - BucketCountsColumn is PER-BUCKET and non-negative by default, and
//     the emitter cumulates it itself with arrayCumSum, which over
//     non-negative elements is monotone non-decreasing by construction.
//     That invariant is what makes Prometheus's
//     ensureMonotonicAndIgnoreSmallDeltas repair unreachable in that mode.
//     BucketCountsCumulative flips the contract for Inputs that already
//     carry a per-`le` ladder — see its own doc.
//   - Phi is a scalar literal; PhiExpr is the computed-phi sibling
//     (typically a ScalarSubquery built from `scalar(<vector>)`) and
//     takes precedence over Phi at emit time. The emitter adds a
//     leading `isNaN(phi) → nan` branch on the PhiExpr path — a
//     runtime-computed phi can be NaN (PromQL `scalar()` over a 0- or
//     multi-series vector) and Prom's bucketQuantile returns NaN for
//     a NaN phi.
//   - GroupBy + GroupByAliases match the wrapping Aggregate / Project
//     contract used elsewhere in the metrics pipeline (PromQL aggregations
//     drop __name__; the Sample wrapping is the caller's responsibility).
//   - PRG ships the classic-histogram path; PR H adds the
//     exponential-histogram variant (otel_metrics_exponential_histogram) as a
//     sibling node so dispatch stays one switch in lowerCall.
//
// The chsql emitter renders the interpolation as a CH expression chain
// over arrayCumSum / arrayFirstIndex with the standard edge cases
// (phi >= 1, phi <= 0, empty, overflow into the +Inf bucket).
type HistogramQuantile struct {
	Input Node
	Phi   float64
	// PhiExpr is the computed-phi alternative to Phi (mutually
	// exclusive; PhiExpr wins when non-nil). See the IR contract above.
	PhiExpr              Expr
	BucketCountsColumn   string
	ExplicitBoundsColumn string

	// BucketCountsCumulative declares that BucketCountsColumn already
	// holds the CUMULATIVE per-`le` ladder (Prometheus's own bucket
	// representation) rather than per-bucket counts, so the emitter must
	// not arrayCumSum it and must read the observation total off the
	// ladder's top rung instead of summing the array.
	//
	// The classic-histogram bucket-layout merge sets this: folding rows
	// whose ExplicitBounds differ is only well-defined in the cumulative
	// domain (a per-bucket count means nothing without the layout it was
	// measured against), so the merge projection emits a ladder over the
	// union of bounds. Because that ladder is derived per `le` rather
	// than accumulated from non-negative counts, the producer owes
	// Prometheus's ensureMonotonicAndIgnoreSmallDeltas repair — a prefix
	// maximum — BEFORE this node sees the array. The emitter then reads
	// the total off the repaired top rung, so total and ladder cannot
	// disagree (see the note in chsql.histogramQuantileValueFrag).
	BucketCountsCumulative bool

	// GroupBy + GroupByAliases name the per-series projection from the
	// inner subquery. The emitter projects each as `<expr> AS <alias>`
	// in the outer SELECT so the wrapping Sample projection can pick
	// them up by name. Typical content: a single MapAccess on the
	// Attributes column when the lowering reads a raw histogram row;
	// expression-shape mirrors chplan.Aggregate.
	GroupBy        []Expr
	GroupByAliases []string

	// MetricNameColumn, AttributesColumn, TimestampColumn are surfaced
	// in the output row for the Sample wrapping that lower.go applies
	// downstream. Wrapping logic in lowerHistogramQuantile fills the
	// Sample contract from these names (mirroring wrapAggregateForSample
	// in lower.go).
	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
}

func (*HistogramQuantile) planNode() {}

func (h *HistogramQuantile) Children() []Node { return []Node{h.Input} }

func (h *HistogramQuantile) Equal(other Node) bool {
	o, ok := other.(*HistogramQuantile)
	if !ok {
		return false
	}
	if (h.PhiExpr == nil) != (o.PhiExpr == nil) {
		return false
	}
	if h.PhiExpr != nil && !h.PhiExpr.Equal(o.PhiExpr) {
		return false
	}
	if h.Phi != o.Phi ||
		h.BucketCountsColumn != o.BucketCountsColumn ||
		h.ExplicitBoundsColumn != o.ExplicitBoundsColumn ||
		h.BucketCountsCumulative != o.BucketCountsCumulative ||
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
