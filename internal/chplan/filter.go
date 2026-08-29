package chplan

// Filter applies a predicate to its input rows, equivalent to a SQL WHERE
// clause. Stacking multiple Filter nodes is permitted; the optimizer fuses
// them via the conjunction-flattening rule.
//
// Histogram marks Input as histogram-valued (chplan.RowShapeOf(Input) ==
// chplan.HistogramRowShape), mirroring [InfoJoin.Histogram] and
// [VectorSetOp.Histogram]. Filter's own emitted SELECT is a bare passthrough
// of every column its input publishes (`SELECT * FROM (<input>) WHERE …`,
// or the fused PREWHERE/WHERE form directly over a Scan), so a Filter over
// a histogram-valued input keeps publishing the full thirteen-column
// contract — [RowShapeOf] must report [HistogramRowShape] for it or a wire
// consumer re-projects down to the canonical quartet and silently drops the
// nine histogram columns (cerberus issue #2518, `limit_ratio(r, v)` over a
// bare exp-histogram selector: unlike topk/bottomk, which drop histogram
// samples, limit_ratio keeps every series its hash-based sampler selects —
// float or histogram — unchanged). Defaults to false everywhere else, so
// every pre-existing Filter construction site is unaffected.
//
// Mixed is Histogram's sibling for a MIXED float/histogram-valued input
// (chplan.RowShapeOf(Input) == chplan.MixedRowShape) — `limit_ratio(r, v)`
// over `v` a mixed float/histogram `or` (cerberus issue #2613). Filter's own
// emission is unaffected either way (still a bare passthrough), so setting
// Mixed changes nothing about the rendered SQL; it exists purely so
// [RowShapeOf] reports the right contract to whatever sits above this
// Filter. Histogram and Mixed are mutually exclusive — an Input is one row
// shape or the other, never both.
type Filter struct {
	Input     Node
	Predicate Expr
	Histogram bool
	Mixed     bool
}

func (*Filter) planNode() {}

func (f *Filter) Children() []Node { return []Node{f.Input} }

func (f *Filter) Equal(other Node) bool {
	o, ok := other.(*Filter)
	if !ok {
		return false
	}
	return f.Predicate.Equal(o.Predicate) && f.Input.Equal(o.Input) &&
		f.Histogram == o.Histogram && f.Mixed == o.Mixed
}
