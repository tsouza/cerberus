package ast

import (
	"strconv"
	"strings"
)

// FirstStageElement is the first stage of a metrics query — the function
// that turns spansets into a raw time series (rate, *_over_time, compare).
type FirstStageElement interface {
	Element
	isFirstStage()
}

// SecondStageElement is an optional later stage that post-processes the
// series produced by the first stage (a metrics filter, topk/bottomk, or a
// chain of those).
type SecondStageElement interface {
	Element
	isSecondStage()
}

// MetricsAggregate is the standard first stage: rate / count_over_time /
// min|max|avg|sum_over_time / quantile_over_time / histogram_over_time,
// optionally grouped by attributes. Fields are private and reached through
// accessors used by lowering.
type MetricsAggregate struct {
	op        MetricsAggregateOp
	attr      Attribute
	by        []Attribute
	quantiles []float64
}

func newMetricsAggregate(op MetricsAggregateOp, attr Attribute, by []Attribute, quantiles []float64) *MetricsAggregate {
	return &MetricsAggregate{op: op, attr: attr, by: by, quantiles: quantiles}
}

// Op returns the metrics function.
func (a *MetricsAggregate) Op() MetricsAggregateOp { return a.op }

// Attribute returns the aggregated attribute (zero Attribute for rate /
// count_over_time, which take no argument).
func (a *MetricsAggregate) Attribute() Attribute { return a.attr }

// GroupBy returns the `by(...)` attributes.
func (a *MetricsAggregate) GroupBy() []Attribute { return a.by }

// Quantiles returns the phi values for quantile_over_time.
func (a *MetricsAggregate) Quantiles() []float64 { return a.quantiles }

func (*MetricsAggregate) isFirstStage() {}

func (a MetricsAggregate) String() string {
	var b strings.Builder
	b.WriteString(a.op.String())
	b.WriteByte('(')
	switch a.op {
	case MetricsAggregateRate, MetricsAggregateCountOverTime:
		// no argument
	case MetricsAggregateQuantileOverTime:
		b.WriteString(a.attr.String())
		for _, q := range a.quantiles {
			b.WriteString(", ")
			b.WriteString(strconv.FormatFloat(q, 'g', -1, 64))
		}
	default:
		b.WriteString(a.attr.String())
	}
	b.WriteByte(')')
	if len(a.by) > 0 {
		parts := make([]string, len(a.by))
		for i, at := range a.by {
			parts[i] = at.String()
		}
		b.WriteString(" by(")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// AverageOverTimeAggregator is the dedicated first stage the language uses
// for `| avg_over_time(attr)`, kept distinct from MetricsAggregate because
// the reference engine evaluates it with a weighted-average algorithm. For
// lowering it carries the same attribute + group-by shape.
type AverageOverTimeAggregator struct {
	attr Attribute
	by   []Attribute
}

func newAverageOverTimeAggregator(attr Attribute, by []Attribute) *AverageOverTimeAggregator {
	return &AverageOverTimeAggregator{attr: attr, by: by}
}

// Attribute returns the averaged attribute.
func (a *AverageOverTimeAggregator) Attribute() Attribute { return a.attr }

// GroupBy returns the `by(...)` attributes.
func (a *AverageOverTimeAggregator) GroupBy() []Attribute { return a.by }

func (*AverageOverTimeAggregator) isFirstStage() {}

func (a AverageOverTimeAggregator) String() string {
	s := MetricsAggregate{op: MetricsAggregateAvgOverTime, attr: a.attr, by: a.by}
	return s.String()
}

// MetricsCompare is the `| compare({...}, topN, start, end)` first stage.
type MetricsCompare struct {
	filter *SpansetFilter
	topN   int
	start  int
	end    int
}

func newMetricsCompare(filter *SpansetFilter, topN, start, end int) *MetricsCompare {
	return &MetricsCompare{filter: filter, topN: topN, start: start, end: end}
}

// Filter returns the comparison's baseline-vs-selection spanset filter.
func (m *MetricsCompare) Filter() *SpansetFilter { return m.filter }

// TopN returns the requested number of distinguishing attributes.
func (m *MetricsCompare) TopN() int { return m.topN }

// Start returns the comparison window start (unix seconds, 0 when unset).
func (m *MetricsCompare) Start() int { return m.start }

// End returns the comparison window end (unix seconds, 0 when unset).
func (m *MetricsCompare) End() int { return m.end }

func (*MetricsCompare) isFirstStage() {}

func (m MetricsCompare) String() string {
	var b strings.Builder
	b.WriteString("compare(")
	if m.filter != nil {
		b.WriteString(m.filter.String())
	}
	if m.topN > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(m.topN))
	}
	if m.start != 0 || m.end != 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(m.start))
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(m.end))
	}
	b.WriteByte(')')
	return b.String()
}

// MetricsFilter is a `| > 0.5`-style second stage that drops series whose
// value fails the comparison against a constant.
type MetricsFilter struct {
	op    Operator
	value float64
}

func newMetricsFilter(op Operator, value float64) *MetricsFilter {
	return &MetricsFilter{op: op, value: value}
}

// Op returns the comparison operator.
func (m *MetricsFilter) Op() Operator { return m.op }

// Value returns the right-hand constant.
func (m *MetricsFilter) Value() float64 { return m.value }

func (*MetricsFilter) isSecondStage() {}

func (m MetricsFilter) String() string {
	return m.op.String() + " " + strconv.FormatFloat(m.value, 'g', -1, 64)
}

// TopKBottomK is a `| topk(N)` / `| bottomk(N)` second stage.
type TopKBottomK struct {
	op    SecondStageOp
	limit int
}

func newTopKBottomK(op SecondStageOp, limit int) *TopKBottomK {
	return &TopKBottomK{op: op, limit: limit}
}

// Op returns topk vs bottomk.
func (m *TopKBottomK) Op() SecondStageOp { return m.op }

// Limit returns N.
func (m *TopKBottomK) Limit() int { return m.limit }

func (*TopKBottomK) isSecondStage() {}

func (m TopKBottomK) String() string {
	return m.op.String() + "(" + strconv.Itoa(m.limit) + ")"
}

// ChainedSecondStage threads several second-stage elements together,
// recording the textual separator between each so the query round-trips.
type ChainedSecondStage struct {
	elements   []SecondStageElement
	separators []string
}

func newChainedSecondStage() *ChainedSecondStage { return &ChainedSecondStage{} }

// Append adds an element and the separator that precedes it.
func (c *ChainedSecondStage) Append(element SecondStageElement, separator string) {
	c.elements = append(c.elements, element)
	c.separators = append(c.separators, separator)
}

// Elements returns the chained second-stage elements in order.
func (c ChainedSecondStage) Elements() []SecondStageElement { return c.elements }

// Separators returns the separators, index-aligned with Elements.
func (c ChainedSecondStage) Separators() []string { return c.separators }

func (ChainedSecondStage) isSecondStage() {}

func (c ChainedSecondStage) String() string {
	var b strings.Builder
	for i, e := range c.elements {
		if i < len(c.separators) {
			b.WriteString(c.separators[i])
		}
		b.WriteString(e.String())
	}
	return b.String()
}
