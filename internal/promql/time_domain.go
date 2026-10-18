package promql

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// evaluationDomainMin and evaluationDomainMax bound every instant a
// lowered plan may evaluate at or read over: the eval instants, every
// `@` pin, and every selector and subquery window derived from them.
//
// The domain is the span in which each time representation the lowering
// uses is exact, shrunk by [evaluationDomainMargin] on each side:
//
//   - ClickHouse DateTime64(9) — the storage columns and every time
//     literal — names 1900-01-01T00:00:00Z to 2262-04-11T23:47:16.854775807Z.
//     Below it `parseDateTime64BestEffort` silently returns 1900-01-01;
//     above it the parse fails with DECIMAL_OVERFLOW.
//   - Go's `time.Time.UnixNano()`, which grid and window arithmetic use,
//     names 1677-09-21 to 2262-04-11 and wraps outside it.
//
// The intersection is DateTime64(9)'s span.
var (
	evaluationDomainMin = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC).Add(evaluationDomainMargin)
	evaluationDomainMax = time.Unix(0, math.MaxInt64).UTC().Add(-evaluationDomainMargin)
)

// evaluationDomainMargin is the headroom [checkEvaluationDomain] keeps
// between the windows it models and the edges of the representable
// span. The analysis models each selector's lookback, each matrix range
// and each subquery range; individual lowerings widen a scan further by
// at most a step or a lookback (grid alignment, staleness carry-in),
// which a day covers many times over.
const evaluationDomainMargin = 24 * time.Hour

// StorageDomainMin and StorageDomainMax are the first and last instants a
// ClickHouse DateTime64(9) column can hold. No stored sample lies outside
// them, so a read clipped to them misses nothing.
var (
	StorageDomainMin = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)
	StorageDomainMax = time.Unix(0, math.MaxInt64).UTC()
)

// maxPinDistanceMillis is the largest distance, in milliseconds, between
// an evaluation instant and an `@` pin that reference Prometheus turns
// into an offset without overflow. Its engine stores the offset as a
// time.Duration — int64 nanoseconds — computed as
// `time.Duration(evalTime-pin) * time.Millisecond`, which wraps beyond
// this distance (about 292 years) and evaluates the pinned subtree at a
// different, wrapped instant.
const maxPinDistanceMillis = math.MaxInt64 / int64(time.Millisecond)

// ErrOutsideEvaluationDomain is wrapped by every lowering refusal of an
// expression whose evaluation leaves the evaluation domain. The HTTP
// layer never lowers such an expression — it asks
// [LeavesEvaluationDomain] first and answers it with the reference
// engine — so the refusal only reaches callers that lower directly.
var ErrOutsideEvaluationDomain = errors.New("outside the evaluation domain")

// LeavesEvaluationDomain reports whether evaluating expr over the
// request instants [start, end] reaches outside the evaluation domain:
// an eval instant, an `@` pin, or a window derived from either lies
// outside [evaluationDomainMin, evaluationDomainMax], or a pin sits far
// enough from an eval instant that reference Prometheus's offset
// arithmetic wraps.
//
// It over-approximates: every window is widened by the lookback and the
// subquery range, and every eval instant of a subquery is taken over its
// whole window. A false positive costs only the slower reference
// evaluation; a false negative would lower a plan that answers wrongly.
//
// start and end are taken literally: 0001-01-01T00:00:00Z, Go's zero
// time.Time, is an instant like any other here, far outside the domain.
func LeavesEvaluationDomain(expr parser.Expr, start, end time.Time) bool {
	d := domainWalk{
		startMs: timeMillis(start),
		endMs:   timeMillis(end),
		minMs:   timeMillis(evaluationDomainMin),
		maxMs:   timeMillis(evaluationDomainMax),
	}
	return d.leaves(expr, d.startMs, d.endMs)
}

// checkEvaluationDomain is the lowering entry points' refusal of an
// expression [LeavesEvaluationDomain] reports. To a lowering, a zero
// start or end means "no range threaded": the plan anchors to the
// server's now, so that is the instant checked.
func checkEvaluationDomain(expr parser.Expr, start, end time.Time) error {
	now := time.Now()
	if start.IsZero() {
		start = now
	}
	if end.IsZero() {
		end = now
	}
	if LeavesEvaluationDomain(expr, start, end) {
		return fmt.Errorf("promql: evaluation reaches outside %s to %s, the span lowering represents exactly: %w",
			evaluationDomainMin.Format(time.RFC3339), evaluationDomainMax.Format(time.RFC3339), ErrOutsideEvaluationDomain)
	}
	return nil
}

// domainWalk carries the request bounds and the domain, all in Unix
// milliseconds — the unit reference Prometheus evaluates in.
type domainWalk struct {
	startMs, endMs int64
	minMs, maxMs   int64
}

// leaves walks expr evaluated at every instant in [lo, hi].
func (d domainWalk) leaves(expr parser.Expr, lo, hi int64) bool {
	if d.outside(lo, hi) {
		return true
	}
	switch e := expr.(type) {
	case *parser.VectorSelector:
		pinLo, pinHi, out := d.pinned(e.Timestamp, e.StartOrEnd, lo, hi)
		off := durationMillis(e.OriginalOffset)
		return out || d.outside(
			satSub(satSub(pinLo, off), durationMillis(instantLookback)),
			satSub(pinHi, off),
		)
	case *parser.MatrixSelector:
		vs, ok := e.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return true
		}
		pinLo, pinHi, out := d.pinned(vs.Timestamp, vs.StartOrEnd, lo, hi)
		off := durationMillis(vs.OriginalOffset)
		return out || d.outside(
			satSub(satSub(pinLo, off), durationMillis(e.Range)),
			satSub(pinHi, off),
		)
	case *parser.SubqueryExpr:
		pinLo, pinHi, out := d.pinned(e.Timestamp, e.StartOrEnd, lo, hi)
		if out {
			return true
		}
		off := durationMillis(e.OriginalOffset)
		step := e.Step
		if step == 0 {
			step = DefaultSubqueryStep
		}
		innerLo := satSub(satSub(satSub(pinLo, off), durationMillis(e.Range)), durationMillis(step))
		return d.leaves(e.Expr, innerLo, satSub(pinHi, off))
	case *parser.StepInvariantExpr:
		return d.leaves(e.Expr, lo, hi)
	case *parser.ParenExpr:
		return d.leaves(e.Expr, lo, hi)
	case *parser.UnaryExpr:
		return d.leaves(e.Expr, lo, hi)
	case *parser.BinaryExpr:
		return d.leaves(e.LHS, lo, hi) || d.leaves(e.RHS, lo, hi)
	case *parser.AggregateExpr:
		return (e.Param != nil && d.leaves(e.Param, lo, hi)) || d.leaves(e.Expr, lo, hi)
	case *parser.Call:
		for _, arg := range e.Args {
			if d.leaves(arg, lo, hi) {
				return true
			}
		}
		return false
	case *parser.NumberLiteral, *parser.StringLiteral:
		return false
	default:
		// A node kind this walk does not model cannot be shown to stay
		// inside the domain.
		return true
	}
}

// pinned resolves the instants a node evaluates at: its `@` pin when it
// has one, else the enclosing eval instants [lo, hi]. out reports a pin
// far enough from some enclosing eval instant that reference
// Prometheus's offset arithmetic wraps ([maxPinDistanceMillis]).
func (d domainWalk) pinned(ts *int64, startOrEnd parser.ItemType, lo, hi int64) (pinLo, pinHi int64, out bool) {
	var pin int64
	switch {
	case ts != nil:
		pin = *ts
	case startOrEnd == parser.START:
		pin = d.startMs
	case startOrEnd == parser.END:
		pin = d.endMs
	default:
		return lo, hi, false
	}
	far := satAbs(satSub(lo, pin)) > maxPinDistanceMillis || satAbs(satSub(hi, pin)) > maxPinDistanceMillis
	return pin, pin, far
}

// outside reports whether [lo, hi] leaves [d.minMs, d.maxMs].
func (d domainWalk) outside(lo, hi int64) bool {
	return lo < d.minMs || hi > d.maxMs
}

// timeMillis is t in Unix milliseconds, saturated at the int64 range.
// time.Time names instants far beyond it, and so does a pin the parser
// accepted ([parser.VectorSelector.Timestamp] is already milliseconds).
func timeMillis(t time.Time) int64 {
	sec := t.Unix()
	const millisPerSecond = int64(time.Second / time.Millisecond)
	if sec > math.MaxInt64/millisPerSecond {
		return math.MaxInt64
	}
	if sec < math.MinInt64/millisPerSecond {
		return math.MinInt64
	}
	return satAdd(sec*millisPerSecond, int64(t.Nanosecond())/int64(time.Millisecond))
}

// durationMillis is d in milliseconds, truncated as reference
// Prometheus's own durationMilliseconds does.
func durationMillis(d time.Duration) int64 {
	return int64(d / time.Millisecond)
}

// satAdd is a+b saturated at the int64 range. The sum overflows exactly
// when a and b share a sign that the wrapped sum does not, and a wrapped
// sum carries the sign opposite to the true one.
func satAdd(a, b int64) int64 {
	sum := a + b
	if (a^sum)&(b^sum) < 0 {
		if sum < 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return sum
}

// satSub is a-b saturated at the int64 range. The difference overflows
// exactly when a and b differ in sign and the wrapped difference does not
// carry a's, and then the true difference has a's sign.
func satSub(a, b int64) int64 {
	diff := a - b
	if (a^b)&(a^diff) < 0 {
		if a >= 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return diff
}

// satAbs is |a| saturated at the int64 range.
func satAbs(a int64) int64 {
	return max(a, satSub(0, a))
}
