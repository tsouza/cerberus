package chplan

// RowShape summarizes a node's physical output schema in the sample
// vocabulary. It is diagnostic: it does not prove which sample kinds remain
// live after filtering, or whether a wrapper should preserve metric names.
// Consumers that need specific columns must resolve and validate their roles.
type RowShape int

const (
	// SampleRowShape covers canonical samples and is also the documentation
	// default for opaque, incomplete, open, or invalid outputs. It is not a
	// float-only proof.
	SampleRowShape RowShape = iota

	// GridWindowRowShape carries attributes, a value, and a per-step anchor.
	// A metric-name column may also be present.
	GridWindowRowShape

	// ReducedWindowRowShape carries attributes and a value without a timestamp
	// or anchor. A metric-name column may also survive grouping.
	ReducedWindowRowShape

	// HistogramRowShape carries the complete canonical histogram payload.
	// Additional columns do not change the physical payload classification.
	HistogramRowShape

	// MixedRowShape carries canonical float and histogram payloads plus one
	// discriminator. A float-narrowing Filter retains this physical shape even
	// when its live rows are proven float-only.
	MixedRowShape
)

// String names the shape for diagnostics without exposing enum ordinals.
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

// RowShapeOf folds the node's own compositional schema. There is no independent
// node-kind classifier or Filter/TopK shape declaration to override it.
func RowShapeOf(n Node) RowShape {
	if n == nil {
		return SampleRowShape
	}
	return RowShapeFromSchema(n.RowType())
}
