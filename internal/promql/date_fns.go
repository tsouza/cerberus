package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

const timestampFunctionName = "timestamp"

// lowerDateFn maps PromQL date-component functions to their ClickHouse
// equivalents. Each function takes one instant-vector argument whose
// `Value` column is interpreted as a Unix timestamp in seconds — except
// `timestamp(v)`, which ignores `Value` and reports a timestamp chosen by
// the argument's parser shape (see [timestampResultExpr]).
//
// When called without an argument the PromQL spec defaults the input to
// `vector(time())` — a single instant-vector entry whose value is the
// current evaluation timestamp in seconds. cerberus lowers that to a
// degenerate `OneRow` source (a `StepGrid` in range mode) projecting the
// date component of the evaluation instant ([lowerDateFnNoArg]).
// The `timestamp` function does NOT have a zero-arg form in upstream
// PromQL — Prometheus rejects it during parsing — so the no-arg branch
// is unreachable for that name; we keep the same shape anyway for
// uniformity in case upstream relaxes the rule.
//
// Semantic notes:
//
//   - Every date component is computed from a whole count of Unix seconds
//     by [goCivilTime], an integer-arithmetic transcription of Go's own
//     `time` package calendar. ClickHouse's DateTime and DateTime64 types
//     are never involved, so no input is clamped to their ranges: the
//     answer is Prometheus's for every int64 second count.
//
//   - The seconds are what Prometheus feeds `time.Unix`: `int64(v)` of the
//     sample value for the one-argument form ([valueAsUnixSeconds]), the
//     evaluation instant's milliseconds divided by 1000 for the no-argument
//     form ([evalUnixSeconds]).
//
//   - `timestamp(v)` ignores `Value`. For a VECTOR SELECTOR argument it
//     reads the sample's own TimeUnix column; for every other argument
//     shape it reads the evaluation instant instead, matching the two
//     reference implementations ([timestampResultExpr]). Either way the
//     chosen DateTime64(9) becomes fractional Unix seconds via
//     `toUnixTimestamp64Nano(<ts>) / 1e9`.
//
// Type-coercion note: the calendar arithmetic yields integers of assorted
// widths, and clickhouse-go/v2 refuses to scan an integer column into the
// cerberus Sample row's `*float64` Value. Every emit is wrapped in
// `toFloat64(...)` so the Value column lands as Float64 on the wire.
// `timestamp(v)` is already Float64 (it divides by `1e9`) but wrapping it
// is harmless.
func lowerDateFn(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) > 1 {
		return nil, fmt.Errorf("promql: %s expects 0 or 1 argument, got %d", c.Func.Name, len(c.Args))
	}

	if len(c.Args) == 0 {
		return lowerDateFnNoArg(c.Func.Name, s, ctx)
	}

	// `timestamp(v)` is defined for a histogram-VALUED argument exactly as
	// it is for a float-valued one — reference reads only Point.T, never
	// Point.H/Point.F (see this file's package doc). Every other
	// date-component function's argument is genuinely FLOAT-only (its
	// value math divides/casts the sample's own Value), so this check is
	// scoped to `timestamp` alone. See histogram_native_timestamp.go for
	// why the generic `lower()` call below cannot answer it: an
	// exp-histogram-valued argument falls through to
	// expHistogramSelectorRouting's catch-all rejection without this.
	if c.Func.Name == timestampFunctionName {
		if node, ok, err := lowerTimestampOverExpHistogram(c.Args[0], s, ctx); ok {
			return node, err
		}
	} else {
		// Reference Prometheus routes year/month/day_of_month/day_of_year/
		// day_of_week/days_in_month/hour/minute through a shared dateWrapper
		// helper (promql/functions.go) that explicitly skips every sample
		// whose H field is set before computing the date component from F —
		// the same "process only float samples, drop the rest" rule
		// simpleFloatFunc applies for abs/ceil/floor/round/etc (issue
		// #2221) and clamp/sort already mirror (issues #2345, #2456).
		// These eight never routed their argument through
		// lowerExpHistogramValuedShape, so a histogram-valued argument fell
		// through to the generic lower() dispatch below and hit
		// expHistogramSelectorRouting's catch-all rejection instead of
		// Prom's drop-and-answer-empty semantics (cerberus issue #2498).
		// lowerExpHistogramArgAsCanonicalFloat also recognises a NESTED
		// drop-family argument (cerberus issue #2528) — e.g.
		// `day_of_month(demo_latency_exp_hist + 0)` — which already
		// evaluates to empty, so this family's own float-only drop is a
		// no-op on top of it.
		if node, ok, err := lowerExpHistogramArgAsCanonicalFloat(c.Args[0], s, ctx); ok {
			return node, err
		}
	}

	// `or` between a float-valued operand and a histogram-valued operand
	// (cerberus issue #2330) directly wrapped by one of the eight
	// value-reading date functions, reached from ANY nesting depth since
	// [lowerCall] is the single generic dispatch point for this function
	// regardless of whether it sits at the query root or nested under
	// another wrapper (cerberus issue #2609). See
	// histogram_native_mixed_or_datefn.go's own doc comment for why
	// reference's dateWrapper drop rule composes with no reduction
	// machinery at all — just the shadow-resolved float arm fed into the
	// same value projection this function's own non-mixed path builds
	// below.
	if b, ok := dateFnOverMixedExpHistogramSetOp(c, s, ctx); ok {
		prepare, err := lowerWithMixedOperandPolicy(mixedDateFamily, mixedOperandAdmission, mixedFloatOnly)
		if err != nil {
			return nil, err
		}
		inner, err := shadowResolveFloatArmChecked(b, s, ctx)
		if err != nil {
			return nil, err
		}
		return projectDateFnOverInner(c, prepare(inner), s, ctx)
	}

	// The argument is lowered under an ARGUMENT ctx rather than the caller's
	// own, because `timestamp(<vector-selector>)` needs the selector seam to
	// publish a column no other consumer asks for. Every other function/shape
	// pair leaves the ctx untouched.
	argCtx := dateFnArgCtx(c.Func.Name, c.Args[0], ctx)
	inner, err := lower(c.Args[0], s, argCtx)
	if err != nil {
		return nil, err
	}
	return projectDateFnOverInner(c, inner, s, ctx)
}

// projectDateFnOverInner is the shared post-load date kernel. Operand loading
// precedes function validation on both entry paths. Direct mixed operands have
// already been shadow-resolved; nested mixed operands require their own plan
// admission before narrowing. Timestamp retains its separate payload contract.
func projectDateFnOverInner(c *parser.Call, inner chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if c.Func.Name != timestampFunctionName && dateFnExpr(c.Func.Name, nil, nil) == nil {
		return nil, fmt.Errorf("promql: unknown date function %s", c.Func.Name)
	}
	family := mixedDateFamily
	if c.Func.Name == timestampFunctionName {
		family = mixedTimestampFamily
		if mixedRowsNeedPreparation(inner) {
			return lowerTimestampOverMixedPlan(inner, c.Args[0], s, ctx)
		}
	} else if mixedRowsNeedPreparation(inner) {
		prepare, err := lowerWithMixedOperandPolicy(mixedDateFamily, mixedPlanAdmission, mixedFloatOnly)
		if err != nil {
			return nil, err
		}
		inner = prepare(inner)
	}
	return guardedValueProjection(inner, c.Args[0], s, ctx, family, func(refs sampleRoleRefs) chplan.Expr {
		inputSchema := refs.sourceMetrics(s)
		return asFloat64(dateFnExpr(c.Func.Name, valueAsUnixSeconds(inputSchema), timestampResultExpr(c.Args[0], inputSchema, ctx)))
	}, carriedSampleTimestampColumns(c.Func.Name, c.Args[0], ctx)...)
}

// dateFnArgCtx returns the ctx the date function's argument is lowered under.
//
// It differs from the caller's ctx in exactly one case: a range-mode
// `timestamp(<vector-selector>)`, where the result is the SELECTED SAMPLE's
// own timestamp and the range-mode selector seam publishes only the step
// anchor by default. [lowerCtx.withSampleTimestamp] asks it for the sample's
// timestamp as well; [timestampResultExpr] reads the column it publishes.
//
// Instant mode needs nothing: the instant seam already emits
// `max(TimeUnix) AS lwr_ts` and re-aliases it back over the schema timestamp
// column, so the sample's own time IS the timestamp column there.
func dateFnArgCtx(name string, arg parser.Expr, ctx lowerCtx) lowerCtx {
	if !readsRangeSampleTimestamp(name, arg, ctx) {
		return ctx
	}
	return ctx.withSampleTimestamp()
}

// carriedSampleTimestampColumns names the columns a duplicate-labelset guard
// between the selector seam and the value projection must carry through, so
// the projection can still read them.
//
// Only the range-mode `timestamp(<vector-selector>)` shape has one: its value
// expression references [chplan.RangeLWRSampleTimestampColumn], which is not
// one of the four canonical Sample columns the guard carries by default. A
// selector that spans several metric names (`timestamp({job="api"})`) puts a
// guard there, and without this the projection above it would reference a
// column the guard's GROUP BY had dropped.
func carriedSampleTimestampColumns(name string, arg parser.Expr, ctx lowerCtx) []string {
	if !readsRangeSampleTimestamp(name, arg, ctx) {
		return nil
	}
	return []string{chplan.RangeLWRSampleTimestampColumn}
}

// readsRangeSampleTimestamp reports whether this date-function call reports
// the selected sample's OWN timestamp out of a range-mode selector seam —
// i.e. it is `timestamp`, its argument is a vector selector, and the
// argument lowers through the RangeLWR AGGREGATED seam that collapses each
// (series, anchor) group and stamps `TimeUnix` with the step anchor. It is
// the single predicate [dateFnArgCtx], [carriedSampleTimestampColumns] and
// [timestampResultExpr] all answer from, so the column cannot be requested
// without being read or read without being requested.
//
// `ctx.step > 0` alone is NOT that seam: it is also true wherever a
// subquery-inner or range-vector-reducer argument lowers with
// `ctx.inRangeVector` set, and that path takes the OPPOSITE branch of
// [lowerVectorSelector] — it suppresses the RangeLWR wrap so every
// in-window sample survives as its own row, with `TimeUnix` already the
// raw sample time and no [chplan.RangeLWRSampleTimestampColumn] ever
// published. `ctx.step > 0` was exactly that kind of proxy: true for the
// aggregated seam this predicate means to name, but also true — for an
// unrelated reason, the OUTER query's own step — for the raw-row seam a
// subquery inner selector builds. Requesting the column there asks for
// one that does not exist in the SQL this branch actually emits
// (`max_over_time(timestamp(<selector>)[5m:1m])`, a live 502: `Unknown
// expression identifier lwr_sample_ts`). Deciding from `inRangeVector`
// instead answers from which seam the argument's OWN lowering takes, not
// from a step value that means something else on that path.
func readsRangeSampleTimestamp(name string, arg parser.Expr, ctx lowerCtx) bool {
	if name != timestampFunctionName || ctx.step <= 0 || ctx.inRangeVector {
		return false
	}
	_, isSelector := unwrapVectorSelector(arg)
	return isSelector
}

// lowerDateFnNoArg synthesises a single-row constant instant vector for
// the no-arg form of a date function. PromQL spec: `year()` ≡
// `year(vector(time()))`. The result is a one-row vector with the date
// component of the evaluation instant as its sample value
// ([syntheticScalarVector] supplies the canonical Sample shape and, in
// range mode, the per-step `StepGrid` source).
func lowerDateFnNoArg(name string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	expr := dateFnExpr(name, evalUnixSeconds(ctx), nil)
	if expr == nil {
		return nil, fmt.Errorf("promql: unknown date function %s", name)
	}
	return syntheticScalarVector(asFloat64(expr), nil, s, ctx), nil
}

// evalUnixSeconds renders the whole Unix seconds Prometheus's no-argument
// date functions read: `time.Unix(enh.Ts/1000, 0)`, the evaluation
// instant's milliseconds divided by 1000 and truncated toward zero.
// Truncating the instant's nanoseconds by 1e9 in one step is the same
// number, and ClickHouse's `intDiv` truncates toward zero as Go's `/` does.
//
// In instant mode the instant is the request anchor ([anchorBaseExpr]:
// the literal evaluation time, or `now64(9)` when no anchor was
// threaded). In range mode it is `now64(9)`, which
// [syntheticScalarVector] rewrites to each step's `anchor_ts`.
func evalUnixSeconds(ctx lowerCtx) chplan.Expr {
	instant := chplan.NowNano()
	if ctx.step <= 0 {
		instant = anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
	}
	return &chplan.FuncCall{Fn: chplan.FnIntDiv, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{instant}},
		&chplan.LitInt{V: chplan.NanoToSecondDivisor},
	}}
}

// timestampResultExpr returns the expression `timestamp(v)` reports as
// its result, chosen by the PARSER SHAPE of the argument. Reference
// Prometheus implements `timestamp` twice and the argument's shape —
// not its value — picks between them:
//
//   - `timestamp(<VectorSelector>)` is special-cased by the reference
//     engine (promql/engine.go, rangeEvalTimestampFunctionOverVectorSelector),
//     which stamps each output sample with the RAW sample timestamp
//     (`F: float64(s.T) / 1000`). That is the sample's own `TimeUnix`.
//   - EVERY other argument shape falls through to `funcTimestamp`, which
//     returns `float64(enh.Ts) / 1000` — the EVALUATION instant of the
//     step being computed, with the input sample's own timestamp
//     discarded.
//
// The two answers coincide only when the selected sample sits exactly on
// the eval instant; under the default 5m lookback they differ by up to
// the lookback delta, so `timestamp(abs(m))` and `timestamp(m)` are
// genuinely different queries.
//
// The unwrap is load-bearing and mirrors the reference engine's own:
// upstream peels ParenExpr and StepInvariantExpr before its type test, so
// `timestamp((m))` and an `@`-pinned `timestamp(m @ 100)` still take the
// selector branch. [unwrapVectorSelector] peels exactly that pair.
//
// The tsRef slot is only consulted by the `timestamp` arm of
// [dateFnExpr]; every other date function reads `Value`, so the choice
// made here is inert for them.
//
// WHICH COLUMN holds "the sample's own timestamp" is mode-dependent, and
// getting that wrong is what made the selector branch report the step
// anchor in range mode. In INSTANT mode the selector seam's `TimeUnix` IS
// the raw sample time (`max(TimeUnix) AS lwr_ts`). In RANGE mode it is
// not: every range-mode AGGREGATED seam stamps `TimeUnix` with the STEP
// ANCHOR, and the per-(series, anchor) `argMax(Value, TimeUnix)` collapse
// throws the selecting sample's own time away. That is what
// [chplan.RangeLWRSampleTimestampColumn] exists to publish, requested by
// [readsRangeSampleTimestamp] on the way down, and it is the column the
// selector branch has to read here — otherwise `time() - timestamp(up)`
// is a constant near zero instead of the sample's age.
//
// This mirrors [readsRangeSampleTimestamp] exactly, and for the same
// reason: `ctx.inRangeVector` excludes the RAW-row seam a subquery-inner
// or range-vector-reducer argument lowers through, where `TimeUnix` is
// already the sample's own time and [chplan.RangeLWRSampleTimestampColumn]
// was never requested — so it was never published, and reading it here
// would reference a column the emitted SQL does not have.
func timestampResultExpr(arg parser.Expr, s schema.Metrics, ctx lowerCtx) chplan.Expr {
	if _, isSelector := unwrapVectorSelector(arg); isSelector {
		if ctx.step > 0 && !ctx.inRangeVector {
			return &chplan.ColumnRef{Name: chplan.RangeLWRSampleTimestampColumn}
		}
		return &chplan.ColumnRef{Name: s.TimestampColumn}
	}
	return evalInstantExpr(s, ctx)
}

// evalInstantExpr renders the evaluation instant of the step whose row is
// being projected — Prometheus's `enh.Ts` — for a projection sitting
// directly above an already-lowered instant vector.
//
// In RANGE mode the answer is the sample timestamp column, and that is a
// structural property of cerberus's range lowering rather than a
// coincidence: every range-mode source stamps `TimeUnix` with the STEP
// ANCHOR, not with the underlying sample's own time. [wrapRangeLatestPerSeries]
// collapses each (series, anchor) bucket and emits the canonical Sample
// contract with `TimeUnix = anchor_ts`; [wrapRangeAbsoluteAtBroadcast]
// projects the StepGrid's `anchor_ts` as `TimeUnix`; a RangeWindow does
// the same for matrix functions. So above any range lowering the
// timestamp column already IS the eval instant.
//
// In INSTANT mode it is not: the instant LWR emits `max(TimeUnix) AS
// lwr_ts`, i.e. the newest in-window SAMPLE's own timestamp, which is
// what makes the selector and non-selector forms differ. There the eval
// instant is the request anchor, rendered as the same literal
// [chplan.TimeLiteral] [anchorBaseExpr] gives every other anchor-reading
// lowering, falling back to `now64(9)` when no anchor was threaded (a
// plain [Lower] without range threading) — exactly the fallback `time()`
// uses in the same situation.
func evalInstantExpr(s schema.Metrics, ctx lowerCtx) chplan.Expr {
	if ctx.step > 0 {
		return &chplan.ColumnRef{Name: s.TimestampColumn}
	}
	if !ctx.end.IsZero() {
		return anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
	}
	return anchorBaseExpr(evalAnchor{})
}

// asFloat64 wraps e in `toFloat64(...)`. Used by the date-function
// lowerings to coerce the date kernel's integer results into Float64,
// matching the Sample.Value
// column type the cursor decodes into. Idempotent for Float64 inputs
// (CH's `toFloat64` is a no-op identity on Float64) so the wrap is
// safe even on the `timestamp(v)` path that already yields Float64.
func asFloat64(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{e}}
}

// dateFnExpr returns the CH expression that computes the date component
// for the given PromQL function name. unixSeconds is the Int64 count of
// Unix seconds the component is taken from ([valueAsUnixSeconds],
// [evalUnixSeconds]); tsRef is the timestamp expression `timestamp(v)`
// reports, chosen by the caller from the argument's shape (see
// [timestampResultExpr]) — the raw `TimeUnix` column for a vector
// selector, the evaluation instant otherwise. No other date function
// reads it.
//
// Returns nil when name is not a recognised date function — caller
// translates that into an "unsupported" error.
func dateFnExpr(name string, unixSeconds, tsRef chplan.Expr) chplan.Expr {
	switch name {
	case "year":
		return goCivil(unixSeconds).year()
	case "month":
		return goCivil(unixSeconds).month()
	case "day_of_month":
		return goCivil(unixSeconds).dayOfMonth()
	case "day_of_year":
		return goCivil(unixSeconds).dayOfYear()
	case "day_of_week":
		return goCivil(unixSeconds).dayOfWeek()
	case "days_in_month":
		return goCivil(unixSeconds).daysInMonth()
	case "hour":
		return goCivil(unixSeconds).hour()
	case "minute":
		return goCivil(unixSeconds).minute()
	case timestampFunctionName:
		// `timestamp(v)` returns tsRef as float seconds — NOT a
		// function of Value. Convert the DateTime64(9) expression to
		// nanoseconds (Int64) and divide by 1e9 to get fractional
		// seconds.
		return &chplan.Binary{
			Op:    chplan.OpDiv,
			Left:  &chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{tsRef}},
			Right: &chplan.LitFloat{V: float64(chplan.NanoToSecondDivisor)},
		}
	}
	return nil
}

// goInt64ConversionMin and goInt64ConversionEnd bound the float64 values
// Go's `int64(v)` converts by truncation: [-2^63, 2^63). Every other
// value — NaN, ±Inf, and any finite value outside that span — converts to
// math.MinInt64 on amd64, the architecture Prometheus releases are
// measured on. Both bounds are exact float64 values.
const (
	goInt64ConversionMin = float64(math.MinInt64)
	goInt64ConversionEnd = -goInt64ConversionMin
)

// valueAsUnixSeconds renders Go's `int64(Value)` as an Int64 — the
// seconds Prometheus's date functions pass to `time.Unix`. A value inside
// [goInt64ConversionMin, goInt64ConversionEnd) truncates toward zero, as
// ClickHouse's `toInt64` does; every other value, NaN included, becomes
// math.MinInt64. The out-of-range value is replaced BEFORE the cast
// rather than after it, because ClickHouse's `toInt64` throws on NaN and
// ±Inf and `if` evaluates both of its branches.
func valueAsUnixSeconds(s schema.Metrics) chplan.Expr {
	value := &chplan.ColumnRef{Name: s.ValueColumn}
	convertible := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.Binary{Op: chplan.OpGe, Left: value, Right: &chplan.LitFloat{V: goInt64ConversionMin}},
		Right: &chplan.Binary{Op: chplan.OpLt, Left: value, Right: &chplan.LitFloat{V: goInt64ConversionEnd}},
	}
	return &chplan.FuncCall{Fn: chplan.FnToInt64, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{convertible, value, &chplan.LitFloat{V: goInt64ConversionMin}}},
	}}
}
