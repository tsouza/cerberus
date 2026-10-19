package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// millisPerSecond converts the Unix milliseconds Prometheus evaluates in
// to the Unix seconds a PromQL value carries.
const millisPerSecond = 1000

// atNode returns c for lowering node: with the time shift the domain fit
// recorded for node, when it recorded one. Every lowering dispatcher calls
// it on entry, so a region is lowered with its shift whichever of its
// nodes a lowering path enters it through.
func (c lowerCtx) atNode(node parser.Node) lowerCtx {
	if shift, ok := c.domain.shiftOf(node); ok {
		c.shiftMillis = shift
	}
	return c
}

// shifted reports whether the region being lowered is evaluated at
// instants shifted away from Prometheus's own (see [fitInstantDomain]).
func (c lowerCtx) shifted() bool { return c.shiftMillis != 0 }

// evalMillis renders the instant, in Unix milliseconds, at which
// Prometheus evaluates the step whose evaluation instant is the
// DateTime64(9) expression instant: the instant minus the region's shift.
// Both operands are Int64, so the subtraction is exact; an instant
// Prometheus evaluates at past 2262 has no DateTime64(9) form, but its
// milliseconds do fit Int64.
func (c lowerCtx) evalMillis(instant chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{instant}},
		Right: &chplan.LitInt{V: c.shiftMillis},
	}
}

// evalSecondsValue renders the value `time()` and `timestamp(v)` report
// for the evaluation instant instant of a shifted region: Prometheus's
// `float64(ts) / 1000` over the instant's milliseconds.
func (c lowerCtx) evalSecondsValue(instant chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{c.evalMillis(instant)}},
		Right: &chplan.LitFloat{V: millisPerSecond},
	}
}

// evalWholeSeconds renders the whole Unix seconds of the evaluation
// instant of a shifted region: Prometheus's `ts / 1000`, truncated toward
// zero as ClickHouse's intDiv truncates.
func (c lowerCtx) evalWholeSeconds(instant chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnIntDiv, Args: []chplan.Expr{
		c.evalMillis(instant),
		&chplan.LitInt{V: millisPerSecond},
	}}
}
