package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_datefn.go lowers the eight VALUE-READING
// date-component functions (`year`/`month`/`day_of_month`/`day_of_week`/
// `day_of_year`/`days_in_month`/`hour`/`minute` — NOT `timestamp`, which
// reads a sample's own TIME column rather than its Value and already has
// its own histogram-valued lowering in histogram_native_timestamp.go)
// wrapping a mixed float/histogram `or` — cerberus issue #2609, the
// date-function-family sibling of sort()/sort_desc()'s own composition
// (histogram_native_mixed_or_sort.go, cerberus issue #2605).
//
// Reference Prometheus (promql/functions.go's dateWrapper, the shared
// helper year/month/day_of_month/day_of_week/day_of_year/days_in_month/
// hour/minute all route through) skips every sample whose `.H` field is
// set before computing the date component from `.V` — the identical
// "process only float samples, drop the rest" rule sort()/sort_desc()
// and the float-only aggregate family already mirror against a mixed
// `or`. date_fns.go's own package doc already cites this exact rule for
// the non-mixed (all-histogram) argument case (cerberus issue #2498).
//
// Like sort()/sort_desc() and unlike the REDUCE-family aggregates, a
// date function is a per-row transform: it keeps every surviving row (no
// grouping, no K-selection), so composing it over a mixed `or` needs
// nothing beyond the shadow-resolved FLOAT arm
// [shadowResolveMixedExpHistogramOperands] already builds for every
// sibling composition in this file group, fed through the SAME
// [guardedValueProjection] / [dateFnExpr] pipeline [lowerDateFn]'s
// non-mixed path already builds for an ordinary (non-histogram)
// argument. The histogram-valued return is unused for the identical
// reason topk/bottomk/sort's own headers give: dateWrapper drops every
// `.H`-set sample unconditionally, so a histogram row can never survive
// into the output regardless of which side of the `or` it
// shadow-resolved to keep.
//
// Checked directly inside [lowerDateFn]
// ([dateFnOverMixedExpHistogramSetOp] below) rather than as a root-only
// recognizer registered in [lowerMixedExpHistogramFamily]: date
// functions are ordinary [lowerCall] dispatch targets reached from EVERY
// nesting depth, exactly like sort()/sort_desc() — see
// histogram_native_mixed_or_sort.go's header for the full argument,
// which applies here unchanged.
//
// `timestamp` is excluded by name: it reads tsRef (the argument's own
// TIME, not its Value) and reference's `funcTimestamp`/
// `rangeEvalTimestampFunctionOverVectorSelector` never inspect `.H` at
// all, so it needs no drop rule — and it is not one of the eight
// functions cerberus issue #2609 scopes.
func dateFnOverMixedExpHistogramSetOp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	if len(c.Args) != 1 || c.Func.Name == "timestamp" {
		return nil, false
	}
	return mixedExpHistogramSetOp(c.Args[0], s, ctx)
}

// lowerDateFnOverMixedExpHistogramSetOp lowers the shape
// [dateFnOverMixedExpHistogramSetOp] recognised. See this file's header
// for why the shadow-resolved float arm alone, fed through the ordinary
// date-component value projection, already answers reference's
// semantics.
func lowerDateFnOverMixedExpHistogramSetOp(c *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatForAgg, err := shadowResolveFloatArmChecked(b, s, ctx)
	if err != nil {
		return nil, err
	}

	// tsRef is nil: [dateFnExpr] only consults it for "timestamp", which
	// [dateFnOverMixedExpHistogramSetOp] never recognises.
	newValue := dateFnExpr(c.Func.Name, valueAsDateTime(s), nil)
	if newValue == nil {
		return nil, fmt.Errorf("promql: unknown date function %s", c.Func.Name)
	}
	return guardedValueProjection(floatForAgg, c.Args[0], s, ctx, asFloat64(newValue)), nil
}
