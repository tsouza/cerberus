package chplan

// RowShape names the column set a node publishes to whatever sits directly
// above it, for the one consumer that has to build that "whatever": a
// projection which forwards its input's columns unchanged and rewrites
// exactly one of them.
//
// Such a forwarder cannot list a fixed set of columns. A projection names
// its outputs by REFERENCE, and ClickHouse resolves every reference against
// the scope the subquery below it actually exposes — naming a column that
// scope does not carry is rejected outright with code 47, `Unknown
// expression identifier`, and OMITTING one the layer above still reads
// silently deletes it from the wire. Both failures are 502s or empty
// panels at the HTTP edge, never a plan-level error, so the forwarder has
// to DERIVE the set from its input rather than declare it.
//
// The three shapes a PromQL forwarder can be handed:
//
//   - [SampleRowShape] — the canonical four-column sample row
//     `(MetricName, Attributes, Timestamp, Value)`. Only the NAMES are
//     the contract: a Scan reads all four off storage, a Filter or Limit
//     passes its input's through untouched, and an aggregation
//     re-publishes them from group keys (`? AS MetricName`, the group map
//     as `Attributes`, an evaluation-time `TimeUnix`) — which is why an
//     [Aggregate] is never the node a forwarder sits directly on, its
//     lowering always caps it with exactly that Project.
//     [RangeWindowResample] and RangeLWR likewise emit all four by
//     construction. What each name CONTAINS is a separate question, and
//     [IsDerivedShape] is what answers it.
//   - [GridWindowRowShape] — one row per (series, ANCHOR). A window that
//     materialises a query_range grid publishes that grid axis TWICE: once
//     under [RangeWindowAnchorColumn], which api/prom's matrix wrapper
//     reads back to bucket each series' points, and once under the schema
//     timestamp column, which a step-aligned vector↔vector join reads off
//     each arm. There is no `MetricName`: a range window computes a
//     DERIVED sample, and PromQL drops `__name__` from those, so the
//     window's emitter never selects the column at all. A forwarder that
//     drops either timestamp column breaks the layer above it; one that
//     references `MetricName` gets code 47.
//   - [ReducedWindowRowShape] — one row per series, already reduced to a
//     single point. There is no per-row timestamp to forward at all and no
//     `MetricName`; only the group keys and the value survive.
//
// The classifier exists because the two windowed shapes were previously
// recognised by asserting ONE concrete node kind at each consumer. That
// spelling answers "is this a *RangeWindow", not "what does my input
// expose", and the two questions came apart the moment a second node grew
// the same row shape: [RangeWindowNative] publishes byte-for-byte the
// columns a matrix [RangeWindow] does, so every `label_replace` /
// `label_join` / `abs` over a range-mode `rate()` on the ts_grid path fell
// through to the canonical branch and emitted a reference to a
// `MetricName` its own subquery never exposed. Deriving the shape here
// means a future node inherits the right branch by publishing the right
// columns, not by being added to a list at each consumer.
//
// This answers a DIFFERENT question from [IsDerivedShape], which asks only
// whether `MetricName` is absent and walks through the transparent nodes
// to find out. Here the caller sits directly on the node it is projecting
// over and needs the timestamp columns too, which "derived" does not
// distinguish.
type RowShape int

const (
	// SampleRowShape is the canonical `(MetricName, Attributes, Timestamp,
	// Value)` row. It is the answer for every node that is not a range
	// window, which is what makes it the zero value: a forwarder that
	// cannot recognise its input is safest assuming the shape the vast
	// majority of plans carry.
	SampleRowShape RowShape = iota

	// GridWindowRowShape is the matrix window row: `(group keys…,
	// RangeWindowAnchorColumn, Timestamp, Value)` and no `MetricName`.
	GridWindowRowShape

	// ReducedWindowRowShape is the instant window row: `(group keys…,
	// Value)` — no timestamp of any kind and no `MetricName`.
	ReducedWindowRowShape

	// HistogramRowShape is [HistogramProjection]'s row: `(group keys…,
	// Histogram*Column…)` — nine named histogram columns instead of a
	// `Value`, and no `MetricName` of its own (a wrapping Project adds
	// one, the same way HistogramQuantileNative's wrapping Project
	// does). It answers false to every `shape != SampleRowShape` check
	// the existing PromQL forwarders (projectValueOverInner,
	// projectAttributesOverInner) make, which is correct: neither
	// forwarder builds a column list a histogram-shaped row satisfies
	// (both unconditionally reference `Value` by name), so a future
	// histogram-valued lowering that needs those forwarders to work
	// over a HistogramProjection must teach them this shape first — see
	// the doc comment on HistogramProjection. No lowering builds this
	// node yet, so no forwarder is called with it today.
	HistogramRowShape
)

// String names the shape for test and error output. The names are the
// constant names minus the stutter, so a failure message reads as the
// vocabulary the doc comment uses.
func (s RowShape) String() string {
	switch s {
	case GridWindowRowShape:
		return "grid-window"
	case ReducedWindowRowShape:
		return "reduced-window"
	case HistogramRowShape:
		return "histogram"
	case SampleRowShape:
		return "sample"
	}
	return "unknown"
}

// RowShapeOf reports which [RowShape] n publishes.
//
// A matrix [RangeWindow] (`OuterRange > 0`) fans its samples across the
// anchors of a materialised grid and emits one row per (series, anchor);
// an instant one has already reduced each series to a single row.
// [RangeWindowNative] is ALWAYS the matrix case — the lowering builds it
// only in range mode, with `Step > 0` and both `Start` and `End` pinned
// (see its doc comment), so it has no instant form to distinguish.
//
// Every other node publishes all four canonical names, either by passing
// its input's through or by re-projecting them, so [SampleRowShape] is
// both the remaining answer and the safe default for a node this
// classifier has never seen.
//
// The classifier deliberately does NOT walk into children: its callers
// project directly over n, so the shape that matters is the one n's own
// SELECT exposes.
func RowShapeOf(n Node) RowShape {
	switch v := n.(type) {
	case *RangeWindow:
		if v.OuterRange > 0 {
			return GridWindowRowShape
		}
		return ReducedWindowRowShape
	case *RangeWindowNative:
		return GridWindowRowShape
	case *HistogramProjection:
		return HistogramRowShape
	case *RangeWindowResample:
		// Spelled out because it is the third `RangeWindow*` node and the
		// one most likely to be swept in here by analogy. Its emitter
		// publishes `[MetricName, Attributes, anchor_ts AS TimeUnix,
		// Value]` — byte-for-byte what the RangeLWR it substitutes for
		// publishes — so the canonical row genuinely IS its answer, and
		// letting it fall through would leave that looking like an
		// oversight.
		return SampleRowShape
	}
	return SampleRowShape
}
