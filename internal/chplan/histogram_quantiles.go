package chplan

// HistogramQuantileLevel is one output requested from a shared histogram
// preparation. Label is the OpenMetrics rendering attached by PromQL's
// histogram_quantiles function; duplicate levels are retained because the
// source function retains them too.
type HistogramQuantileLevel struct {
	Phi   float64
	Label string
}

// HistogramQuantiles evaluates several constant quantiles over one classic
// histogram input. Histogram carries the complete single-quantile contract;
// its Phi/PhiExpr fields are ignored. The emitter prepares and materialises
// the bucket ladder once, evaluates Levels together, then expands the results
// while adding LabelName=Level.Label to the Attributes group column.
type HistogramQuantiles struct {
	Histogram *HistogramQuantile
	LabelName string
	Levels    []HistogramQuantileLevel
}

func (*HistogramQuantiles) planNode() {}

func (h *HistogramQuantiles) Children() []Node {
	if h.Histogram == nil {
		return nil
	}
	return []Node{h.Histogram.Input}
}

func (h *HistogramQuantiles) Equal(other Node) bool {
	o, ok := other.(*HistogramQuantiles)
	if !ok || h.LabelName != o.LabelName || len(h.Levels) != len(o.Levels) {
		return false
	}
	if (h.Histogram == nil) != (o.Histogram == nil) {
		return false
	}
	if h.Histogram != nil {
		a, b := *h.Histogram, *o.Histogram
		a.Phi, b.Phi = 0, 0
		a.PhiExpr, b.PhiExpr = nil, nil
		if !a.Equal(&b) {
			return false
		}
	}
	for i := range h.Levels {
		if h.Levels[i] != o.Levels[i] {
			return false
		}
	}
	return true
}

func (h *HistogramQuantiles) RowType() Schema {
	if h.Histogram == nil {
		return Schema{}
	}
	return h.Histogram.RowType()
}

// HistogramQuantilesNative is the exponential-histogram sibling of
// HistogramQuantiles. It shares scale normalisation and bucket preparation
// across every requested level; it never uses ClickHouse's classic-histogram
// aggregate.
type HistogramQuantilesNative struct {
	Histogram *HistogramQuantileNative
	LabelName string
	Levels    []HistogramQuantileLevel
}

func (*HistogramQuantilesNative) planNode() {}

func (h *HistogramQuantilesNative) Children() []Node {
	if h.Histogram == nil {
		return nil
	}
	return []Node{h.Histogram.Input}
}

func (h *HistogramQuantilesNative) Equal(other Node) bool {
	o, ok := other.(*HistogramQuantilesNative)
	if !ok || h.LabelName != o.LabelName || len(h.Levels) != len(o.Levels) {
		return false
	}
	if (h.Histogram == nil) != (o.Histogram == nil) {
		return false
	}
	if h.Histogram != nil {
		a, b := *h.Histogram, *o.Histogram
		a.Phi, b.Phi = 0, 0
		a.PhiExpr, b.PhiExpr = nil, nil
		if !a.Equal(&b) {
			return false
		}
	}
	for i := range h.Levels {
		if h.Levels[i] != o.Levels[i] {
			return false
		}
	}
	return true
}

func (h *HistogramQuantilesNative) RowType() Schema {
	if h.Histogram == nil {
		return Schema{}
	}
	return h.Histogram.RowType()
}
