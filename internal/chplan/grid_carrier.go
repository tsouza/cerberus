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

	// DataWindowEnd returns the latest sample timestamp this carrier's
	// evaluation can read: the right edge of its DATA window, as opposed
	// to the right edge of the request grid EvalGrid reports.
	//
	// The two differ whenever a selector carries an `offset`. Every
	// windowed carrier here evaluates over `(anchor - Offset - <span>,
	// anchor - Offset]`, so the data edge is `End - Offset` — and a
	// NEGATIVE offset (`offset -1h`, which PromQL accepts and which shifts
	// evaluation FORWARD) puts that edge AFTER the request grid's end,
	// possibly in the future. A caller that reasons about whether a
	// query's window has closed must read this, never EvalGrid's End.
	//
	// The zero time is returned when End is zero, preserving the
	// codebase-wide sentinel for "resolve at emit time": such a carrier's
	// window is not fixed at all and no caller may treat it as closed.
	//
	// It lives on the interface for the same reason AnchorGridDivides
	// does: the compiler forces every carrier kind to answer, so a new
	// kind cannot silently inherit a wrong default, and the completeness
	// ratchet closes the hole rather than a parallel list that can drift.
	DataWindowEnd() time.Time
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
// width exactly as RangeWindowGridNative's is.
//
// TRUE here is the solver's LICENCE TO SHARD, and that licence is LIVE: #2677
// argued the stage-by-stage slice-invariance proof (recorded on this kind's
// entry in sliceInvariantKinds — internal/chplan/sliceinvariant.go) and added
// the re-gridding arm (reanchorRangeBucketGridNative, dispatched by
// ReanchorRange), so a wide-window classic-histogram quantile that busts one
// query's memory cap is time-sliced across K shards instead of having no
// relief at all. Both mechanisms are load-bearing and admitted together or not
// at all: the registry entry alone lets the whole-plan walk accept the plan
// with nothing able to re-grid the carrier, and the reanchor arm alone is
// never reached. internal/solver's
// TestRangeBucketGridNative_SlicingAdmittedAtBothGates pins each separately
// (along with this method's answer), so withdrawing either fails there naming
// which one, rather than silently stranding the carrier back on route A.
func (r *RangeBucketGridNative) AnchorGridDivides() bool { return true }

// AnchorGridDivides: absent_over_time emits one row per anchor.
func (a *AbsentOverTime) AnchorGridDivides() bool { return true }

// dataWindowEnd is the shared `End - Offset` arithmetic every windowed
// carrier's data edge uses, with the zero-End sentinel preserved.
func dataWindowEnd(end time.Time, offset time.Duration) time.Time {
	if end.IsZero() {
		return time.Time{}
	}
	return end.Add(-offset)
}

// DataWindowEnd: a StepGrid carries no offset of its own — it is the
// request's anchor grid — so its data edge is its End.
func (s *StepGrid) DataWindowEnd() time.Time { return dataWindowEnd(s.End, 0) }

func (r *RangeWindow) DataWindowEnd() time.Time { return dataWindowEnd(r.End, r.Offset) }

func (r *RangeWindowGridNative) DataWindowEnd() time.Time { return dataWindowEnd(r.End, r.Offset) }

func (r *RangeWindowStaleResample) DataWindowEnd() time.Time {
	return dataWindowEnd(r.End, r.Offset)
}

func (r *RangeLWR) DataWindowEnd() time.Time { return dataWindowEnd(r.End, r.Offset) }

func (r *RangeBucketFanout) DataWindowEnd() time.Time { return dataWindowEnd(r.End, r.Offset) }

func (r *RangeBucketGridNative) DataWindowEnd() time.Time { return dataWindowEnd(r.End, r.Offset) }

// DataWindowEnd: AbsentOverTime's emitter shifts the internal grid by
// Offset and adds it back on the OUTPUT timestamp, so the rows it READS
// still end at End - Offset like every other carrier's.
func (a *AbsentOverTime) DataWindowEnd() time.Time { return dataWindowEnd(a.End, a.Offset) }
