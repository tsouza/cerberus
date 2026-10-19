package promql

import (
	"math/big"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// Prometheus evaluates an `@` pin through int64 arithmetic that can
// overflow. Its engine stores the pin as int64 milliseconds, but
// setOffsetForAtModifier turns the distance from the evaluator's start to
// the pin into a time.Duration, int64 nanoseconds. A distance longer than
// about 292 years wraps, and the evaluator then runs at an instant other
// than the pin: `@ -62135596800` evaluated in 2026 runs in 1754.
//
// The storage read Prometheus issues for a selector uses the pin itself
// (getTimeRangesForSelector), not the wrapped instant. A wrapped
// evaluation therefore finds no sample: the samples it is handed lie
// about 584 years (2^64 ns) away from the window it evaluates. A
// constant expression, which reads no sample, is evaluated at the wrapped
// instant.
//
// The functions below transcribe that arithmetic. Go's signed integer
// arithmetic wraps exactly as Prometheus's does.

// promEval is the part of Prometheus's evaluator state that decides the
// instant an `@` pin evaluates at.
type promEval struct {
	// start is the evaluator's startTimestamp, in milliseconds.
	start int64
	// offEval is the evalTime of the setOffsetForAtModifier pass that last
	// set the offsets of the subtree being walked.
	offEval int64
	// offPath lists the subqueries between that pass's root and the node
	// being walked, outermost first.
	offPath []*parser.SubqueryExpr
	// req holds the request start and end in milliseconds, the instants
	// `@ start()` and `@ end()` name.
	req *[2]int64
}

// promDurationMilliseconds is Prometheus's durationMilliseconds.
func promDurationMilliseconds(d time.Duration) int64 {
	return int64(d / (time.Millisecond / time.Nanosecond))
}

// promSubqueryTimes is the offset part of Prometheus's subqueryTimes: the
// offsets of the subqueries in path, reset at the last one carrying a pin.
func promSubqueryTimes(path []*parser.SubqueryExpr, req *[2]int64) (time.Duration, *int64) {
	var (
		offset time.Duration
		ts     *int64
	)
	for _, n := range path {
		offset += n.OriginalOffset
		if pin, ok := promPinMillis(n.Timestamp, n.StartOrEnd, req); ok {
			offset = n.OriginalOffset
			ts = &pin
		}
	}
	return offset, ts
}

// promPinMillis returns the pin of an `@` modifier in milliseconds: the
// literal timestamp, or the request start or end that `@ start()` and
// `@ end()` name, which Prometheus's PreprocessExpr substitutes. It
// reports false for a node with no pin, and for `@ start()` / `@ end()`
// when req is nil.
func promPinMillis(ts *int64, startOrEnd parser.ItemType, req *[2]int64) (int64, bool) {
	switch {
	case ts != nil:
		return *ts, true
	case req == nil:
		return 0, false
	case startOrEnd == parser.START:
		return req[0], true
	case startOrEnd == parser.END:
		return req[1], true
	}
	return 0, false
}

// promAtOffset is the offset setOffsetForAtModifier assigns to a node
// pinned at ts with the given original offset.
func promAtOffset(ev promEval, ts int64, orig time.Duration) time.Duration {
	subqOffset, subqTs := promSubqueryTimes(ev.offPath, ev.req)
	if subqTs != nil {
		subqOffset += time.Duration(ev.offEval-*subqTs) * time.Millisecond
	}
	offsetForTs := time.Duration(ev.offEval-ts) * time.Millisecond
	return orig + offsetForTs - subqOffset
}

// exactAtOffsetMillis is [promAtOffset] in milliseconds computed without
// overflow, truncated toward zero as durationMilliseconds truncates.
func exactAtOffsetMillis(ev promEval, ts int64, orig time.Duration) *big.Int {
	ns := big.NewInt(int64(orig))
	subqOffset, subqTs := promSubqueryTimes(ev.offPath, ev.req)
	ms := big.NewInt(int64(time.Millisecond))
	if subqTs != nil {
		d := new(big.Int).Sub(big.NewInt(ev.offEval), big.NewInt(*subqTs))
		ns.Sub(ns, d.Mul(d, ms))
	}
	ns.Sub(ns, big.NewInt(int64(subqOffset)))
	d := new(big.Int).Sub(big.NewInt(ev.offEval), big.NewInt(ts))
	ns.Add(ns, d.Mul(d, ms))
	return ns.Quo(ns, ms)
}

// promPinnedInstant returns the instant, in milliseconds, at which
// Prometheus evaluates a node pinned at ts with the given original
// offset, and whether the arithmetic wrapped, so that the instant is not
// the pin minus the offset.
func promPinnedInstant(ev promEval, ts int64, orig time.Duration) (int64, bool) {
	instant := ev.start - promDurationMilliseconds(promAtOffset(ev, ts, orig))
	exact := new(big.Int).Sub(big.NewInt(ev.start), exactAtOffsetMillis(ev, ts, orig))
	return instant, !exact.IsInt64() || exact.Int64() != instant
}

// promSubqueryStart is the startTimestamp of the evaluator Prometheus
// runs a subquery's inner expression with: the first multiple of
// interval after start - offset - range.
func promSubqueryStart(start, offsetMillis, rangeMillis, interval int64) int64 {
	mint := start - offsetMillis - rangeMillis
	first := interval * (mint / interval)
	if first <= mint {
		first += interval
	}
	return first
}

// promSubqueryEval returns the evaluator state for the inner expression
// of subquery n, evaluated by ev with the effective offset offset.
// Prometheus re-runs setOffsetForAtModifier over the inner expression
// when the inner evaluator starts at a different instant.
func promSubqueryEval(ev promEval, n *parser.SubqueryExpr, offset, interval time.Duration) promEval {
	start := promSubqueryStart(ev.start, promDurationMilliseconds(offset), promDurationMilliseconds(n.Range), promDurationMilliseconds(interval))
	if start != ev.start {
		return promEval{start: start, offEval: start, req: ev.req}
	}
	path := make([]*parser.SubqueryExpr, len(ev.offPath), len(ev.offPath)+1)
	copy(path, ev.offPath)
	return promEval{start: ev.start, offEval: ev.offEval, offPath: append(path, n), req: ev.req}
}
