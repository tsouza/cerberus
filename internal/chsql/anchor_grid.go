package chsql

import (
	"fmt"
	"math"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// anchorGrid is the arithmetic every grid fan-out places its anchors and
// its samples with: the i-th anchor a whole number of steps from a base, a
// timestamp's distance from a base, and the anchor index such a distance
// falls on.
//
// That arithmetic runs in Int64 nanoseconds, which hold about 292 years. A
// grid whose anchors, together with the window each one reaches back over,
// span further than that runs it in Int64 milliseconds instead, provided
// its step and reach are whole milliseconds: the unit of a PromQL instant,
// which every PromQL grid and window is a whole number of. A timestamp's
// sub-millisecond fraction then drops out of its distance, as it does from
// every timestamp Prometheus reads.
type anchorGrid struct {
	// unit is the nanoseconds in one unit of the arithmetic: one, or a
	// million when it runs in milliseconds.
	unit int64
	// step is the grid's step in units.
	step int64
}

// nanosPerMilli is the nanoseconds in a millisecond, the unit of a wide
// grid's arithmetic.
const nanosPerMilli = int64(time.Millisecond)

// newAnchorGrid returns the arithmetic of a grid of numAnchors anchors
// stepNS apart, each reaching reachNS back from or ahead of its anchor.
func newAnchorGrid(stepNS, reachNS, numAnchors int64) anchorGrid {
	if spanFitsNanos(stepNS, reachNS, numAnchors) || stepNS%nanosPerMilli != 0 || reachNS%nanosPerMilli != 0 {
		return anchorGrid{unit: 1, step: stepNS}
	}
	return anchorGrid{unit: nanosPerMilli, step: stepNS / nanosPerMilli}
}

// spanFitsNanos reports whether (numAnchors − 1) · stepNS + |reachNS|, the
// furthest a grid's arithmetic reaches from its base, fits Int64.
func spanFitsNanos(stepNS, reachNS, numAnchors int64) bool {
	if reachNS < 0 {
		reachNS = -reachNS
	}
	steps := max(numAnchors-1, 0)
	if stepNS > 0 && steps > (math.MaxInt64-reachNS)/stepNS {
		return false
	}
	return true
}

// millis reports whether the arithmetic runs in milliseconds.
func (g anchorGrid) millis() bool { return g.unit != 1 }

// units converts ns, a whole number of the grid's units, to them.
func (g anchorGrid) units(ns int64) int64 { return ns / g.unit }

// instantUnits renders the instant ts in the grid's units since the epoch.
func (g anchorGrid) instantUnits(ts Frag) Frag {
	return ToUnixMillis(ts)
}

// fromUnits renders the DateTime64(9) instant units grid units after the
// epoch.
func (g anchorGrid) fromUnits(units Frag) Frag {
	return Call("fromUnixTimestamp64Nano", Mul(units, InlineLit(g.unit)))
}

// back renders the anchor idx steps before base:
// `<base> - toIntervalNanosecond(<idx> * <step>)`, or for a wide grid
// `fromUnixTimestamp64Nano((toUnixTimestamp64Milli(<base>) - <idx> * <step>) * 1000000)`.
func (g anchorGrid) back(base, idx Frag) Frag {
	if !g.millis() {
		return Sub(base, Call("toIntervalNanosecond", Mul(idx, InlineLit(g.step))))
	}
	return g.fromUnits(Paren(Sub(g.instantUnits(base), Mul(idx, InlineLit(g.step)))))
}

// earliest renders the last of numAnchors anchors walking back from base:
// `<base> - toIntervalNanosecond(<(numAnchors − 1) · step>)`, or the wide
// grid's millisecond form of the same instant. numAnchors is the count g was
// built for, so the product fits Int64 in either unit.
func (g anchorGrid) earliest(base Frag, numAnchors int64) Frag {
	back := (numAnchors - 1) * g.step
	if !g.millis() {
		return Sub(base, Call("toIntervalNanosecond", InlineLit(back)))
	}
	return g.fromUnits(Paren(Sub(g.instantUnits(base), InlineLit(back))))
}

// ahead renders the anchor idx steps after base, the forward sibling of
// [anchorGrid.back].
func (g anchorGrid) ahead(base, idx Frag) Frag {
	if !g.millis() {
		return Add(base, Call("toIntervalNanosecond", Mul(idx, InlineLit(g.step))))
	}
	return g.fromUnits(Paren(Add(g.instantUnits(base), Mul(idx, InlineLit(g.step)))))
}

// dist renders the distance, in units, from the instant from to the
// instant to: `dateDiff('nanosecond', <from>, <to>)`, or for a wide grid
// `toUnixTimestamp64Milli(<to>) - toUnixTimestamp64Milli(<from>)`.
func (g anchorGrid) dist(from, to Frag) Frag {
	if !g.millis() {
		return Call("dateDiff", InlineLit("nanosecond"), from, to)
	}
	return Paren(Sub(g.instantUnits(to), g.instantUnits(from)))
}

// floorIdx renders `floor((<dist> + <add>) / step) + 1`, add in units
// ([anchorGridFloorIdxFrag]).
func (g anchorGrid) floorIdx(dist Frag, add int64) Frag {
	return anchorGridFloorIdxFrag(dist, add, g.step)
}

// ceilIdx renders `ceil((<dist> + <add>) / step)`, add in units
// ([anchorGridCeilIdxFrag]).
func (g anchorGrid) ceilIdx(dist Frag, add int64) Frag {
	return anchorGridCeilIdxFrag(dist, add, g.step)
}

// gridAnchorCount returns the end-inclusive number of anchors step apart
// across [start, end]: one when either bound is unset, the now64(9) shape
// whose single anchor is the only deterministic choice, and an error when
// start lies after end. node names the plan node in that error.
func gridAnchorCount(start, end time.Time, step time.Duration, node string) (int64, error) {
	if start.IsZero() || end.IsZero() {
		return 1, nil
	}
	if start.After(end) {
		return 0, fmt.Errorf("%w: %s.Start > End", ErrUnsupported, node)
	}
	return chplan.GridAnchors(start, end, step), nil
}

// outerAnchorCount returns the end-inclusive number of anchors across the
// outer span of a window evaluated on a grid ([chplan.RangeWindow.OuterSteps]).
func outerAnchorCount(r *chplan.RangeWindow) int64 {
	steps, _ := r.OuterSteps()
	return steps + 1
}
