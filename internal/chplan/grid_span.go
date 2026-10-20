package chplan

import (
	"math"
	"math/big"
	"time"
)

// saturatedDuration is what time.Time.Sub returns for a distance longer
// than a time.Duration holds, about 292 years: the field a grid node
// records that distance in then names no exact span.
const saturatedDuration = time.Duration(math.MaxInt64)

// StepsBetween returns how many whole steps fit in end − start and the
// remainder, exactly for any two instants: a time.Duration holds only about
// 292 years, which a grid or a window can span. A negative distance yields a
// negative count with a remainder in [0, step). step must be positive.
func StepsBetween(start, end time.Time, step time.Duration) (int64, time.Duration) {
	nanos := new(big.Int).Mul(big.NewInt(end.Unix()-start.Unix()), big.NewInt(int64(time.Second)))
	nanos.Add(nanos, big.NewInt(int64(end.Nanosecond()-start.Nanosecond())))
	steps, rem := new(big.Int).DivMod(nanos, big.NewInt(int64(step)), new(big.Int))
	return steps.Int64(), time.Duration(rem.Int64())
}

// GridAnchors returns the number of instants step apart from start to end,
// both included: (end − start)/step + 1, or 0 when either bound is unset or
// step is not positive.
func GridAnchors(start, end time.Time, step time.Duration) int64 {
	if start.IsZero() || end.IsZero() || step <= 0 {
		return 0
	}
	steps, _ := StepsBetween(start, end, step)
	return steps + 1
}

// SpanSteps returns how many whole steps fit in the outer span of a grid
// node evaluated across [start, end] with outer range outerRange, and the
// remainder. outerRange is the span, except where it saturated because start
// and end lie further apart than a time.Duration holds: the span is then
// end − start.
func SpanSteps(outerRange time.Duration, start, end time.Time, step time.Duration) (int64, time.Duration) {
	if outerRange == saturatedDuration && !start.IsZero() && !end.IsZero() {
		return StepsBetween(start, end, step)
	}
	return int64(outerRange / step), outerRange % step
}

// GridInstant returns start + k·step, exactly for any k: the k-th instant of
// a grid laid from start, which may lie further from start than a
// time.Duration holds.
func GridInstant(start time.Time, step time.Duration, k int64) time.Time {
	nanos := new(big.Int).Mul(big.NewInt(k), big.NewInt(int64(step)))
	secs, rem := new(big.Int).DivMod(nanos, big.NewInt(int64(time.Second)), new(big.Int))
	return time.Unix(start.Unix()+secs.Int64(), int64(start.Nanosecond())+rem.Int64()).In(start.Location())
}
