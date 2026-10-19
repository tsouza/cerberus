package promql

import (
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// A stored sample's timestamp is a DateTime64(9), so no sample lies outside
// [instantDomainMin, instantDomainMax]. A window that reaches past those
// edges therefore holds the same samples as the window cut back to them,
// and a function whose value depends on nothing but those samples answers
// the same over both. The fit rewrites such a window into the cut one,
// whose instants ClickHouse represents exactly.

// storageEndMillis is the latest whole millisecond, the precision of a
// PromQL `@` pin, that a stored timestamp can reach.
var storageEndMillis = instantDomainMax.UnixMilli()

// storageStart is the left edge of a cut window: one nanosecond before the
// earliest whole millisecond a stored timestamp can reach, so that a
// sample at that millisecond stays inside the left-open window.
var storageStart = time.UnixMilli(instantDomainMin.UnixMilli() + 1).Add(-time.Nanosecond)

// sampleSetRangeFunctions are the range functions whose value is fixed by
// the samples the window holds, not by where the window starts or ends:
// they never read the window's edges or the evaluation instant. rate,
// increase and delta extrapolate to the edges, and predict_linear
// predicts from the evaluation instant, so they are absent.
var sampleSetRangeFunctions = map[string]bool{
	"absent_over_time":             true,
	"avg_over_time":                true,
	"changes":                      true,
	"count_over_time":              true,
	"deriv":                        true,
	"double_exponential_smoothing": true,
	"first_over_time":              true,
	"holt_winters":                 true,
	"idelta":                       true,
	"irate":                        true,
	"last_over_time":               true,
	"mad_over_time":                true,
	"max_over_time":                true,
	"min_over_time":                true,
	"present_over_time":            true,
	"quantile_over_time":           true,
	"resets":                       true,
	"stddev_over_time":             true,
	"stdvar_over_time":             true,
	"sum_over_time":                true,
	"ts_of_first_over_time":        true,
	"ts_of_last_over_time":         true,
	"ts_of_max_over_time":          true,
	"ts_of_min_over_time":          true,
}

// storageWindow returns the window (end - rng, end] a selector evaluated
// over env reads, cut back to the stored range: its end in milliseconds
// and its length. It reports false unless the selector is evaluated at a
// single instant Prometheus reads at, its window reaches past an edge,
// and the window still overlaps the stored range (one that does not is
// empty, which the fit answers on its own).
func storageWindow(env domainEnv, rng time.Duration) (int64, time.Duration, bool) {
	if !env.known || env.wrapped || !env.lo.Equal(env.hi) {
		return 0, 0, false
	}
	end, start := env.hi, env.hi.Add(-rng)
	if checkInstantDomain(end, end, rng, 0) == nil || outsideStorage(start, end) {
		return 0, 0, false
	}
	if end.After(time.UnixMilli(storageEndMillis)) {
		end = time.UnixMilli(storageEndMillis).UTC()
	}
	if start.Before(storageStart) {
		start = storageStart
	}
	return end.UnixMilli(), end.Sub(start), true
}

// cutMatrix returns the matrix selector arg, read by a function of
// sampleSetRangeFunctions over env, pinned at the end of its cut window
// with the cut window's range, when its window reaches past the stored
// range.
func cutMatrix(arg parser.Expr, env domainEnv) (parser.Expr, bool) {
	ms, ok := unwrapParens(arg).(*parser.MatrixSelector)
	if !ok {
		return nil, false
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, false
	}
	at, _ := pinnedEnv(vs.Timestamp, vs.StartOrEnd, vs.OriginalOffset, env)
	end, rng, ok := storageWindow(at, ms.Range)
	if !ok {
		return nil, false
	}
	return &parser.MatrixSelector{VectorSelector: pinSelector(vs, end), Range: rng, EndPos: ms.EndPos}, true
}

// cutSelector returns the instant selector vs, evaluated over env, as an
// expression whose instants ClickHouse represents, when its lookback ends
// past the last stored instant: `vs @ E and timestamp(vs @ E) > M`, with E
// the last stored millisecond. Prometheus answers vs with the newest
// sample of its lookback (S, pin]; no sample lies after E, so that is the
// newest sample at or before E, kept when its millisecond lies after S.
// M is S plus half a millisecond: a sample's millisecond lies after S
// exactly when it lies after M, and the half millisecond absorbs the
// rounding of a timestamp near 2262 to Float64 seconds. A lookback that
// starts before the first stored instant is not cut: the lookback of
// `vs @ E` would start there too.
func cutSelector(vs *parser.VectorSelector, env domainEnv) (parser.Expr, bool) {
	at, _ := pinnedEnv(vs.Timestamp, vs.StartOrEnd, vs.OriginalOffset, env)
	end, _, ok := storageWindow(at, instantLookback)
	start := at.hi.Add(-instantLookback)
	if !ok || !at.hi.After(time.UnixMilli(storageEndMillis)) || start.Before(instantDomainMin) {
		return nil, false
	}
	pinnedAt := pinSelector(vs, end)
	return &parser.BinaryExpr{
		Op:  parser.LAND,
		LHS: pinnedAt,
		RHS: &parser.BinaryExpr{
			Op: parser.GTR,
			LHS: &parser.Call{
				Func: parser.Functions["timestamp"],
				Args: parser.Expressions{pinSelector(vs, end)},
			},
			RHS: &parser.NumberLiteral{Val: (float64(start.UnixMilli()) + halfMillisecond) / millisPerSecond},
		},
		VectorMatching: &parser.VectorMatching{Card: parser.CardManyToMany},
	}, true
}

// halfMillisecond is half a millisecond, in milliseconds.
const halfMillisecond = 0.5

// pinSelector returns a copy of vs pinned at the instant ms with no
// offset.
func pinSelector(vs *parser.VectorSelector, ms int64) *parser.VectorSelector {
	cp := *vs
	cp.Timestamp = &ms
	cp.StartOrEnd = 0
	cp.OriginalOffset, cp.Offset = 0, 0
	return &cp
}
