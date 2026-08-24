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
//     [RangeWindowStaleResample] and RangeLWR likewise emit all four by
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
// the same row shape: [RangeWindowGridNative] publishes byte-for-byte the
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
	// does). It answers TRUE to every `shape != SampleRowShape` check
	// the PromQL forwarders (projectValueOverInner,
	// projectAttributesOverInner) make, so it would take their WINDOWED
	// branch — which is wrong, not merely unhandled: that branch
	// forwards `Value`, and a HistogramProjection publishes `Value`
	// only as the placeholder it binds alongside the nine real
	// Histogram*Column outputs. The projection would therefore succeed
	// in ClickHouse and answer 0, dropping the histogram. Neither
	// forwarder builds a column list a histogram-shaped row satisfies,
	// so a histogram-valued lowering that needs them must teach them
	// this shape first; until then both assert against it on entry
	// (internal/promql/histogram_shape_guard.go). See also the doc
	// comment on HistogramProjection.
	//
	// Three lowerings build this node today (internal/promql's
	// histogram_native_bare.go, histogram_native_sum.go and
	// histogram_native_range_fn.go). Generic forwarders still cannot
	// consume it directly, and issue #2296 asked whether every wrapping
	// shape PromQL can put around a histogram-valued result reaches one of
	// them anyway. It does not: histogram-aware label transforms
	// (label_replace / label_join) and scalar/histogram or
	// histogram/histogram arithmetic recognise their operand directly,
	// composing `sum`/`avg` over an ALREADY histogram-valued result (for
	// example `sum(rate(m_exp_hist[5m]))`) is recognised recursively by
	// internal/promql's mergeableExpHistogramAggregate before it would ever
	// reach a forwarder, and float-only functions drop every histogram
	// sample and re-project an empty canonical float shape (#2245 closed
	// #2296 on that basis). Every remaining wrapping shape stays behind
	// expHistogramSelectorRouting until it grows its own histogram-aware
	// lowering or an extension to that recognizer set.
	//
	// [VectorSetOp] / [NaryVectorSetOp] also answer this shape when their
	// own Histogram field is set — internal/promql's
	// lowerExpHistogramSetOp builds one from two HistogramProjection
	// operands for `and`/`or`/`unless` (cerberus issue #2324). Unlike the
	// three lowerings above, neither node IS a HistogramProjection; the
	// chsql emitter widens their projection to the same nine-column
	// output because both arms publish it under the fixed
	// Histogram*Column aliases regardless. [RowShapeOf] reports the
	// SHAPE a node's own SELECT publishes, not which Go type built it, so
	// this is still the correct answer here.
	//
	// [InfoJoin] answers it too when its own Histogram field is set —
	// internal/promql's lowerInfo builds one when `info(v[, …])`'s base
	// vector v is histogram-valued (cerberus issue #2509): reference
	// Prometheus's info() preserves a histogram sample in the base vector
	// rather than dropping it, so the join's own SELECT widens to carry
	// Input's nine Histogram*Column outputs through alongside the
	// canonical quartet. See [InfoJoin]'s own doc comment.
	HistogramRowShape

	// MixedRowShape is [VectorSetOp]'s answer when its Mixed field is
	// set — internal/promql's lowerMixedExpHistogramSetOp builds one for
	// `or` between a float-valued operand and a histogram-valued operand
	// (cerberus issue #2330). The SELECT it publishes is the canonical
	// quartet, the nine Histogram*Column outputs, AND a trailing
	// discriminator column (unlike [HistogramRowShape]'s thirteen), so a
	// generic forwarder that reads `Value` unconditionally — the same
	// hazard [HistogramRowShape]'s doc comment describes — is just as
	// wrong here: on a histogram-shaped ROW within this result, Value is
	// still only the placeholder [HistogramProjection] always carries.
	// [assertValueShapedInput] (internal/promql/histogram_shape_guard.go)
	// panics on this shape for exactly that reason. Every consumer that
	// builds a Mixed-shaped node keeps it at the query ROOT — see that
	// lowering's own doc comment for why nesting one under another
	// PromQL wrapper is deliberately left unrecognised (falls through to
	// the pre-existing exp-histogram-selector rejection) rather than
	// silently forwarding a column whose meaning depends on a per-row
	// discriminator no generic consumer reads.
	MixedRowShape
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
	case MixedRowShape:
		return "mixed"
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
// [RangeWindowGridNative] is ALWAYS the matrix case — the lowering builds it
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
// SELECT exposes. The one apparent exception, [*Project], is not a
// walk-into-children case either — it reads only the Project's OWN
// projection list (never its Input) to see whether n's own SELECT
// republishes [MixedDiscriminatorColumn] as an output, the same
// derive-from-what-this-node-actually-selects approach every other case
// here uses. internal/promql's `label_replace`/`label_join` composition
// over a mixed `or` (cerberus issue #2449) is the one lowering that ends
// in a Project forwarding that column: [projectAttributesOverInner]'s
// MixedRowShape branch and [guardLabelRewriteCollision]'s matching
// re-projection (both label_fns.go / duplicate_labelset_guard.go) always
// carry it through by construction, so a Project publishing it really is
// still Mixed-shaped, not merely built from one. No other lowering in
// the tree ever names an output [MixedDiscriminatorColumn], so this
// cannot misclassify a Project nothing here wrote.
func RowShapeOf(n Node) RowShape {
	switch v := n.(type) {
	case *Project:
		for _, proj := range v.Projections {
			if ProjectionOutputsColumn(proj, MixedDiscriminatorColumn) {
				return MixedRowShape
			}
		}
	case *RangeWindow:
		if v.OuterRange > 0 {
			return GridWindowRowShape
		}
		return ReducedWindowRowShape
	case *RangeWindowGridNative:
		return GridWindowRowShape
	case *HistogramProjection:
		return HistogramRowShape
	case *VectorSetOp:
		if v.Histogram {
			return HistogramRowShape
		}
		if v.Mixed {
			return MixedRowShape
		}
	case *NaryVectorSetOp:
		if v.Histogram {
			return HistogramRowShape
		}
	case *InfoJoin:
		if v.Histogram {
			return HistogramRowShape
		}
	case *TopK:
		// limitk over a histogram-valued input (cerberus issue #2518):
		// Histogram is only ever set alongside an empty Columns list (see
		// [TopK]'s doc comment), so the outer SELECT is a bare `SELECT *`
		// forwarding Input's own thirteen-column shape verbatim.
		if v.Histogram {
			return HistogramRowShape
		}
	case *Filter:
		// limit_ratio over a histogram-valued input (cerberus issue
		// #2518): Filter's own SELECT is always a passthrough of every
		// column Input publishes, so a histogram-valued Input keeps
		// publishing the full thirteen-column shape through the WHERE.
		if v.Histogram {
			return HistogramRowShape
		}
	case *HistogramVectorJoin:
		// Its own SELECT exposes `_hq_L_*`/`_hq_R_*` aliases, not the
		// canonical four names at all — no generic forwarder is ever
		// placed directly over it (internal/promql's
		// histogram_native_binop_card.go always wraps it in an
		// explicit-column Project first), so SampleRowShape here is a
		// documentation default rather than a claim about live columns.
		// A hypothetical future direct consumer referencing `Value` or
		// `MetricName` against it fails loudly with ClickHouse code 47
		// rather than silently reading the wrong data — see
		// [IsDerivedShape]'s HistogramVectorJoin arm for the same
		// reasoning from the MetricName side.
		return SampleRowShape
	case *HistogramFloatVectorJoin:
		// Its own SELECT genuinely does publish the canonical
		// MetricName/Attributes/TimeUnix names plus the nine fixed
		// Histogram*Column fields — unlike HistogramVectorJoin's
		// `_hq_L_*`/`_hq_R_*`-prefixed shape above — but ALSO an extra
		// Value column no HistogramRowShape consumer expects, so
		// neither fixed shape is an exact fit. SampleRowShape here is
		// likewise a documentation default rather than a claim about
		// live columns: no generic forwarder is ever placed directly
		// over it — the calling lowering (internal/promql's
		// histogram_native_float_vector_scaling_binop.go) always wraps
		// it in an explicit-column Project first, exactly as
		// HistogramVectorJoin's callers do.
		return SampleRowShape
	case *MixedVectorJoin:
		// Its own SELECT publishes `_mvj_L_*`/`_mvj_R_*`-prefixed aliases
		// (internal/chsql/mixed_vector_join.go), not any of the fixed
		// shapes above — like HistogramVectorJoin/HistogramFloatVectorJoin
		// above, SampleRowShape here is a documentation default rather
		// than a claim about live columns: no generic forwarder is ever
		// placed directly over it. internal/promql's
		// histogram_native_mixed_or_vector_arithmetic.go always wraps it
		// in an explicit-column Filter+Project before any consumer sees
		// it; THAT Project is what RowShapeOf's own *Project case above
		// classifies as MixedRowShape (MUL/DIV) or leaves as the plain
		// canonical SampleRowShape (ADD/SUB, whose kept rows are always
		// float-valued — see that file's header for why).
		return SampleRowShape
	case *RangeWindowStaleResample:
		// Spelled out because it is the third `RangeWindow*` node and the
		// one most likely to be swept in here by analogy. Its emitter
		// publishes `[MetricName, Attributes, anchor_ts AS TimeUnix,
		// Value]` — byte-for-byte what the RangeLWR it substitutes for
		// publishes — so the canonical row genuinely IS its answer, and
		// letting it fall through would leave that looking like an
		// oversight.
		return SampleRowShape
	case *OrderBy:
		// emitOrderBy renders `SELECT * FROM (<input>) ORDER BY ...` — a
		// genuine passthrough of every column its input publishes, sort
		// keys included. Its own SELECT therefore exposes exactly the
		// shape its input does, whichever one that is (a plain sample
		// selector's OrderBy, PromQL `sort_by_label()`'s ordinary case,
		// stays SampleRowShape through the default branch below; wrapping
		// a [HistogramProjection] — PromQL `sort_by_label()` /
		// `sort_by_label_desc()` over an exp-histogram-valued vector,
		// cerberus issue #2462 — must report HistogramRowShape or the
		// wire layer's wrapWithSampleProjection re-projects only the
		// canonical quartet and silently drops the nine histogram
		// columns). Forwarding is recursive so a further OrderBy over an
		// OrderBy (never built today) still resolves correctly.
		return RowShapeOf(v.Input)
	}
	return SampleRowShape
}
