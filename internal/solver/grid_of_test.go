package solver

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// GridOf derives the request's outer eval grid from the PLAN, and the Planner
// treats a zero grid as proof the query is instant — it short-circuits on
// `Step <= 0` before analyze() runs, so a plan whose grid GridOf cannot see is
// recorded as an instant query with no cost grid at all. That failure is
// silent: no error, no wrong result, just a range query filed under the wrong
// evaluation mode in every downstream consumer of the classification.
//
// The tests below pin that GridOf sees the grid through the
// [chplan.GridCarrier] interface, so it reads EVERY carrier rather than a
// hand-kept subset of node kinds. chplan's own completeness ratchet keeps the
// carrier set closed; these tests keep this consumer honest about reading it.

// gridOfCarriers is one instance of every grid-bearing plan node, each pinned
// to the same grid. Constructing them as []chplan.GridCarrier means a node that
// stops carrying a grid fails to compile here.
func gridOfCarriers() []chplan.GridCarrier {
	return []chplan.GridCarrier{
		&chplan.StepGrid{Start: gridStart, End: gridEnd, Step: gridStep},
		&chplan.RangeWindow{
			Input: leafScan(), Func: "rate", Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
			TimestampColumn: "TimeUnix", ValueColumn: "Value",
		},
		&chplan.RangeWindowNative{
			Input: leafScan(), Func: "rate", Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
			TimestampColumn: "TimeUnix", ValueColumn: "Value",
		},
		&chplan.RangeWindowResample{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
			TimestampCol: "TimeUnix", ValueCol: "Value",
		},
		&chplan.RangeLWR{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
		},
		&chplan.RangeBucketFanout{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
		},
		&chplan.AbsentOverTime{
			Input: leafScan(), Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: gridStep,
		},
	}
}

// TestGridOf_ReadsEveryCarrier pins that GridOf recovers the request grid from
// a plan rooted on ANY grid carrier. Before GridOf dispatched on the interface
// it enumerated concrete kinds, and the carriers it omitted reported a zero
// grid — i.e. reported "instant" for a range query.
func TestGridOf_ReadsEveryCarrier(t *testing.T) {
	t.Parallel()

	for _, carrier := range gridOfCarriers() {
		name := reflect.TypeOf(carrier).Elem().Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Wrap it the way a real plan does (the sample projection every
			// PromQL plan gets) so the carrier is found below the root.
			plan := &chplan.Project{Input: carrier}

			start, end, step := GridOf(plan)
			if step != gridStep {
				t.Errorf("GridOf step = %s, want %s — this carrier's range queries would be "+
					"classified as instant", step, gridStep)
			}
			if !start.Equal(gridStart) {
				t.Errorf("GridOf start = %s, want %s", start, gridStart)
			}
			if !end.Equal(gridEnd) {
				t.Errorf("GridOf end = %s, want %s", end, gridEnd)
			}
		})
	}
}

// gridOfInstantCarriers mirrors gridOfCarriers with Step: 0 on every carrier —
// a separate literal set rather than reflectively zeroing gridOfCarriers'
// output (reflect.Value.FieldByName is forbidden project-wide, see
// CLAUDE.md), built the same way gridOfCarriers already is: one struct literal
// per kind, so a carrier that stops compiling here is a carrier this test
// stops covering, not a silent reflective no-op.
func gridOfInstantCarriers() []chplan.GridCarrier {
	return []chplan.GridCarrier{
		&chplan.StepGrid{Start: gridStart, End: gridEnd, Step: 0},
		&chplan.RangeWindow{
			Input: leafScan(), Func: "rate", Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
			TimestampColumn: "TimeUnix", ValueColumn: "Value",
		},
		&chplan.RangeWindowNative{
			Input: leafScan(), Func: "rate", Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
			TimestampColumn: "TimeUnix", ValueColumn: "Value",
		},
		&chplan.RangeWindowResample{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
			TimestampCol: "TimeUnix", ValueCol: "Value",
		},
		&chplan.RangeLWR{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
		},
		&chplan.RangeBucketFanout{
			Input: leafScan(), Lookback: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
		},
		&chplan.AbsentOverTime{
			Input: leafScan(), Range: 5 * time.Minute,
			Start: gridStart, End: gridEnd, Step: 0,
		},
	}
}

// TestGridOf_InstantCarrierReportsZeroGrid pins the other half of the contract:
// a carrier in INSTANT mode (Step == 0) must still report a zero grid, so the
// Planner's instant guard keeps firing for genuinely instant queries. Widening
// GridOf's reach must not turn instant plans into range plans.
func TestGridOf_InstantCarrierReportsZeroGrid(t *testing.T) {
	t.Parallel()

	for _, carrier := range gridOfInstantCarriers() {
		name := reflect.TypeOf(carrier).Elem().Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, _, step := GridOf(&chplan.Project{Input: carrier}); step != 0 {
				t.Errorf("GridOf step = %s for an instant %s, want 0", step, name)
			}
		})
	}
}

// TestGridOf_PrefersOutermostCarrier pins that the OUTER grid wins when a plan
// nests carriers (a PromQL subquery: an inner window feeding an outer one). The
// Planner re-walks the spine itself for inner-grid commensurability; GridOf's
// contract is the outer bounds only, and reading an inner grid instead would
// mis-size every downstream cost estimate.
func TestGridOf_PrefersOutermostCarrier(t *testing.T) {
	t.Parallel()

	const innerStep = 5 * time.Second
	inner := &chplan.RangeWindowNative{
		Input: leafScan(), Func: "rate", Range: time.Minute,
		Start: gridStart, End: gridEnd, Step: innerStep,
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
	outer := &chplan.RangeWindow{
		Input: inner, Func: "max_over_time", Range: 5 * time.Minute,
		Start: gridStart, End: gridEnd, Step: gridStep,
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}

	if _, _, step := GridOf(&chplan.Project{Input: outer}); step != gridStep {
		t.Errorf("GridOf step = %s, want the OUTER grid %s (not the inner %s)", step, gridStep, innerStep)
	}
}
