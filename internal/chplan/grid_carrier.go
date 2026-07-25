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
}

// Compile-time proof that every grid-bearing node in this package implements
// GridCarrier. The completeness ratchet proves the converse — that this list
// omits no grid-bearing node.
var _ = []GridCarrier{
	(*StepGrid)(nil),
	(*RangeWindow)(nil),
	(*RangeWindowNative)(nil),
	(*RangeWindowResample)(nil),
	(*RangeLWR)(nil),
	(*RangeBucketFanout)(nil),
	(*AbsentOverTime)(nil),
}

func (s *StepGrid) EvalGrid() (time.Time, time.Time, time.Duration) {
	return s.Start, s.End, s.Step
}

func (r *RangeWindow) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeWindowNative) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeWindowResample) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeLWR) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (r *RangeBucketFanout) EvalGrid() (time.Time, time.Time, time.Duration) {
	return r.Start, r.End, r.Step
}

func (a *AbsentOverTime) EvalGrid() (time.Time, time.Time, time.Duration) {
	return a.Start, a.End, a.Step
}
