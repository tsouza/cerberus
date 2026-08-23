package chplan

import "time"

// [RangeBucketGridNative] and [RangeBucketWindowSlide] are two independent
// ClickHouse-native lowerings of the SAME per-(series, anchor) classic-
// histogram carrier shape — one native-aggregate (rate), one anchor-injection
// (sum_over_time) — and currently declare the IDENTICAL field set (Input /
// Start / End / Step / Range / Offset / GroupBy / GroupByAliases /
// AnchorAlias / TimestampCol / BucketCountsCol / ExplicitBoundsCol) with
// byte-identical NumAnchors and Equal bodies. golangci's dupl gate correctly
// flags that: this file is the shared home for the two duplicated bodies.
//
// It deliberately does NOT embed a shared struct into either node type.
// Embedding would change both structs' composite-literal syntax (a Go
// embedded field can only be set via its own type name in a keyed literal,
// e.g. `Foo{Base: Base{X: 1}}` rather than `Foo{X: 1}`), which would break
// every existing `&RangeBucketGridNative{Input: ..., Start: ..., ...}` /
// `&RangeBucketWindowSlide{...}` construction site across chplan, chsql,
// promql and solver — for a lint fix, that blast radius is not worth it. The
// two node types keep their own field declarations (duplicated, but each
// only ~12 lines); only the METHOD BODIES are shared here.
//
// Each node's own Equal keeps its own concrete-type assertion — that is what
// keeps the two kinds distinct at the chplan.Node level, the whole point of
// having two node kinds rather than one polymorphic one — and repackages its
// fields into a bucketGridCarrierFields value only to call the shared
// comparison.
//
// bucketGridCarrierFields' own fields are lowercase, deliberately NOT named
// Start/End/Step: it is a comparison-only snapshot, not a plan node, and
// grid_carrier_completeness_test.go's scan recognises a grid-owning node by
// that exact exported field signature with no exemption mechanism (by
// design — see that test's own doc). Matching the signature here would be a
// false structural claim this type does not make: it carries no plan
// semantics of its own and is never walked, cloned or reanchored as a node.

// bucketGridCarrierNumAnchors is the NumAnchors formula both node kinds
// share: one row per Step across [Start, End] (end-inclusive), i.e.
// (End-Start)/Step + 1, or zero when the grid is not pinned (defence in
// depth — see each node's own NumAnchors doc for why the zero-grid guard is
// kept despite range mode being a real construction precondition).
func bucketGridCarrierNumAnchors(start, end time.Time, step time.Duration) int64 {
	if start.IsZero() || end.IsZero() || step <= 0 {
		return 0
	}
	return end.Sub(start).Nanoseconds()/step.Nanoseconds() + 1
}

// bucketGridCarrierFields is a comparison-only snapshot of the field set
// [RangeBucketGridNative] and [RangeBucketWindowSlide] share — see this
// file's own doc for why it is not embedded into either type, and why its
// own fields are lowercase rather than mirroring Start/End/Step.
type bucketGridCarrierFields struct {
	input             Node
	start             time.Time
	end               time.Time
	step              time.Duration
	rng               time.Duration
	offset            time.Duration
	groupBy           []Expr
	groupByAliases    []string
	anchorAlias       string
	timestampCol      string
	bucketCountsCol   string
	explicitBoundsCol string
}

// equal is the field-by-field comparison both node kinds' Equal methods
// share, after each has type-asserted `other` down to its own concrete kind
// and repackaged both sides' fields into this snapshot.
func (f bucketGridCarrierFields) equal(o bucketGridCarrierFields) bool {
	if !f.start.Equal(o.start) || !f.end.Equal(o.end) {
		return false
	}
	if f.step != o.step || f.rng != o.rng || f.offset != o.offset {
		return false
	}
	if f.anchorAlias != o.anchorAlias || f.timestampCol != o.timestampCol {
		return false
	}
	if f.bucketCountsCol != o.bucketCountsCol || f.explicitBoundsCol != o.explicitBoundsCol {
		return false
	}
	if len(f.groupBy) != len(o.groupBy) || len(f.groupByAliases) != len(o.groupByAliases) {
		return false
	}
	for i := range f.groupByAliases {
		if f.groupByAliases[i] != o.groupByAliases[i] {
			return false
		}
	}
	for i := range f.groupBy {
		if !f.groupBy[i].Equal(o.groupBy[i]) {
			return false
		}
	}
	if f.input == nil || o.input == nil {
		return f.input == nil && o.input == nil
	}
	return f.input.Equal(o.input)
}
