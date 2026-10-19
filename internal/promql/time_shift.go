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
// DateTime64(9) expression instant: the instant minus the region's shift,
// when the region is shifted. Both operands are Int64, so the subtraction
// is exact; an instant Prometheus evaluates at past 2262 has no
// DateTime64(9) form, but its milliseconds do fit Int64.
func (c lowerCtx) evalMillis(instant chplan.Expr) chplan.Expr {
	millis := &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{instant}}
	if !c.shifted() {
		return millis
	}
	return &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  millis,
		Right: &chplan.LitInt{V: c.shiftMillis},
	}
}

// evalSecondsValue renders the value `time()` and `timestamp(v)` report
// for the evaluation instant instant: Prometheus's `float64(ts) / 1000`
// over the milliseconds [lowerCtx.evalMillis] renders.
func (c lowerCtx) evalSecondsValue(instant chplan.Expr) chplan.Expr {
	return millisSecondsValue(c.evalMillis(instant))
}

// unixSecondsValue renders the value `timestamp(v)` reports for the stored
// sample whose DateTime64(9) timestamp is instant: Prometheus's
// `float64(ts) / 1000` over its Unix milliseconds,
// `toFloat64(toUnixTimestamp64Milli(<instant>)) / 1000`.
//
// The milliseconds are the only spelling that computes Prometheus's own
// float64. Dividing the instant's Unix nanoseconds by 1e9 rounds twice,
// once converting the nanoseconds to float64 and once dividing, and lands
// one ULP away from Prometheus's value for many instants more than 2^62
// nanoseconds before the epoch (before about 1824) and many carrying a
// millisecond fraction. Truncating to milliseconds first also
// drops any sub-millisecond fraction a DateTime64(9) column stores, which
// Prometheus's own int64-millisecond sample timestamps never carry.
func unixSecondsValue(instant chplan.Expr) chplan.Expr {
	return millisSecondsValue(&chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{instant}})
}

// millisSecondsValue renders Prometheus's `float64(ts) / 1000` over the
// Int64 Unix milliseconds millis: `toFloat64(<millis>) / 1000`.
func millisSecondsValue(millis chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{millis}},
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
