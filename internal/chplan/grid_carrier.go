package chplan

import "time"

// This file makes "which plan nodes own an eval grid" a TYPE-LEVEL property of
// the IR rather than a list maintained by each consumer.
//
// Background. Several nodes materialise a query_range evaluation grid — the
// (Start, End, Step) triple whose anchors become the emitted timestamps. Any
// consumer that needs the request's outer grid (routing, cost accounting,
// telemetry) has to find whichever of those nodes the plan happens to carry.
// Written as a type switch over concrete kinds, that consumer silently reports
// a ZERO grid for every plan built from a node kind the switch does not list —
// and a zero grid is indistinguishable from a genuine instant query, so a
// range query gets misreported as instant rather than producing an error.
//
// The failure mode is not hypothetical: it is a pure omission bug, so it costs
// nothing to introduce (add a node, forget a consumer) and produces no signal
// when it happens. Enumerating concrete kinds at the consumer is therefore the
// wrong shape. GridCarrier inverts it: the NODE declares its grid, and every
// consumer dispatches on the interface, so a node that implements it is
// automatically visible to all consumers at once.
//
// The remaining hole — a node that declares a grid but forgets to implement
// GridCarrier — is closed by the completeness ratchet in
// grid_carrier_completeness_test.go, which parses this package's own source and
// fails when the set of grid-declaring structs differs from the set of
// GridCarrier implementations. There is no allow-list: a new grid-bearing node
// either implements the interface or turns that test red.

// GridCarrier is implemented by every plan node that owns an EVAL GRID: the
// (Start, End, Step) triple defining the anchors the node's output is
// materialised at.
//
// Step is the discriminator between the two evaluation modes, and it is the
// only field a consumer may branch on:
//
//   - Step > 0 — range mode. Start and End are pinned and the node materialises
//     anchors Start, Start+Step, …, End.
//   - Step == 0 — instant mode (or, for the subquery-internal shapes, a grid
//     the emitter derives at emit time). There is no materialised anchor grid,
//     and Start / End carry no request-grid meaning.
//
// Implementations return their fields verbatim; EvalGrid performs no
// normalisation, so a caller sees exactly what the emitter will read.
type GridCarrier interface {
	Node

	// EvalGrid returns the node's eval grid. See the interface doc for the
	// Step == 0 contract.
	EvalGrid() (start, end time.Time, step time.Duration)

	// AnchorGridDivides reports whether slicing the anchor grid K ways
	// divides this carrier's PEAK intermediate working set by K.
	//
	// It answers the question the sharded-pushdown solver's cost proxy
	// actually needs and cannot otherwise see. That proxy gates on
	// F = Lookback/Step, treating a large fan-out as evidence that slicing
	// will pay. For a carrier whose peak is Theta(rows x Lookback/Step) —
	// CONSTANT IN N, the grid width — F is not a divisor at all but a
	// REDUNDANCY MULTIPLIER: every shard rebuilds the same per-(series,
	// anchor) fold over its own window, and adjacent shards' windows overlap
	// by Lookback. The proxy's sign is inverted for those carriers, so the
	// very thing that clears the gate is the thing that makes slicing lose.
	//
	// Measured on ClickHouse 26.6 for a classic-histogram
	// RangeBucketFanout spine (Step=15s, Lookback=5m, OuterRange=1h, K=12):
	// route A 1 query / 8,070 ms / 93,608 rows / 4.01 GB peak; route B
	// 12 queries / 185,101 ms / 3,343,211 rows / 3.69 GB peak. Slicing
	// twelve ways recovered 8.7% of a perfectly-divisible peak while costing
	// 23x the ClickHouse work.
	//
	// This is a policy declaration about the EMITTER's peak locus, in the
	// same category as IsSliceInvariant — not a measurement, and not a
	// threshold. It lives on the interface so the compiler forces every
	// carrier kind to answer, and the completeness ratchet closes the hole
	// for free rather than needing a parallel list that can drift.
	AnchorGridDivides() bool
}

// Compile-time proof that every grid-bearing node in this package implements
// GridCarrier. The completeness ratchet proves the converse — that this list
// omits no grid-bearing node.
var _ = []GridCarrier{
	(*StepGrid)(nil),
	(*RangeWindow)(nil),
	(*RangeWindowGridNative)(nil),
	(*RangeWindowStaleResample)(nil),
	(*RangeLWR)(nil),
	(*RangeBucketFanout)(nil),
	(*RangeBucketGridNative)(nil),
	(*AbsentOverTime)(nil),
}

func (s *StepGrid) EvalGrid() (time.Time, time.Time, time.Duration) {
	return s.Start, s.End, s.Step
}

func (r *RangeWindow) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeWindowGridNative) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeWindowStaleResample) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeLWR) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeBucketFanout) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

// EvalGrid: the native bucket-ladder aggregate is handed the same
// (Start, End, Step) grid its fan-out sibling walks.
func (r *RangeBucketGridNative) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (a *AbsentOverTime) EvalGrid() (time.Time, time.Time, time.Duration) {
	return a.Start, a.End, a.Step
}

// AnchorGridDivides: a StepGrid's per-anchor row set is the grid itself, so
// halving the grid halves the work.
func (s *StepGrid) AnchorGridDivides() bool { return true }

// AnchorGridDivides: the windowed-array matrix is Theta(rows x N), linear in
// the grid width, so slicing partitions it.
func (r *RangeWindow) AnchorGridDivides() bool { return true }

// AnchorGridDivides: the CH-native timeSeries*ToGrid aggregate materialises one
// grid per series, linear in N.
func (r *RangeWindowGridNative) AnchorGridDivides() bool { return true }

// AnchorGridDivides: the re-gridding resample carries one row per anchor.
func (r *RangeWindowStaleResample) AnchorGridDivides() bool { return true }

// AnchorGridDivides: the last-write-wins fan-out keeps one sample per
// (series, anchor), so its intermediate is linear in N.
func (r *RangeLWR) AnchorGridDivides() bool { return true }

// AnchorGridDivides reports the negation of PeakIndependentOfGrid — see that
// field for why this is per-construction-site rather than per node type.
//
// The classic bucket-ladder lowering sets the flag: its intermediate is
// rows x (Lookback/Step), which does NOT shrink when the grid is cut, because
// each shard rebuilds the whole per-(series, anchor) fold over its own window
// and neighbouring shards re-read the Lookback overlap (23x the ClickHouse work
// for 8.7% of the peak — see the interface doc). The exponential/native
// lowerings deliberately do NOT set it: route A is where #2385's 19 observed
// OOMs happened, so slicing is what bounds their memory, and nothing has
// measured otherwise.
func (r *RangeBucketFanout) AnchorGridDivides() bool { return !r.PeakIndependentOfGrid }

// AnchorGridDivides: the native aggregate materialises one Array of N grid
// points per (series, `le` rung), so its intermediate is linear in the grid
// width exactly as RangeWindowGridNative's is. It is reported honestly even
// though slicing never reaches this node.
//
// TRUE here is the solver's LICENCE TO SHARD, so what keeps the honest answer
// safe is that two independent refusals stop the plan before any threshold
// reads it: the kind is deliberately absent from sliceInvariantKinds (no
// slice-invariance proof has been argued for it), and internal/solver's
// carrierGeometryOf reports it non-re-anchorable (ReanchorRange has no arm
// that re-grids it). Neither is left as prose — both, and the routing outcome
// they produce, are pinned by internal/solver's
// TestRangeBucketGridNative_SlicingRefusedAtBothGates /
// _NeverRoutesUnderAuto, which fail if either refusal is removed. Registering
// this kind as slice-invariant therefore fails loudly rather than silently
// handing the solver a licence nobody re-derived.
func (r *RangeBucketGridNative) AnchorGridDivides() bool { return true }

// AnchorGridDivides: absent_over_time emits one row per anchor.
func (a *AbsentOverTime) AnchorGridDivides() bool { return true }
