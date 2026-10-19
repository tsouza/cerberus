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
// they never read the window's edges. predict_linear also reads the
// evaluation instant, which a cut leaves where it is. rate, increase and
// delta extrapolate to the edges, so they are absent.
var sampleSetRangeFunctions = map[string]bool{
	"absent_over_time":             true,
	"avg_over_time":                true,
	"changes":                      true,
	"count_over_time":              true,
	"deriv":                        true,
	"double_exponential_smoothing": true,
	"first_over_time":              true,
	fnHoltWinters:                  true,
	"idelta":                       true,
	"irate":                        true,
	"last_over_time":               true,
	"mad_over_time":                true,
	"max_over_time":                true,
	"min_over_time":                true,
	"predict_linear":               true,
	"present_over_time":            true,
	fnQuantileOverTime:             true,
	"resets":                       true,
	"stddev_over_time":             true,
	"stdvar_over_time":             true,
	"sum_over_time":                true,
	"ts_of_first_over_time":        true,
	"ts_of_last_over_time":         true,
	"ts_of_max_over_time":          true,
	"ts_of_min_over_time":          true,
}

// storageCut is a window (end - rng, end] cut back to the stored range.
type storageCut struct {
	// endMillis is the cut window's end in milliseconds and rng its length.
	endMillis int64
	rng       time.Duration
	// startBeyond and endBeyond are how far the window's own start and
	// end lie outside the cut window's, zero for an edge it kept.
	startBeyond, endBeyond time.Duration
}

// storageWindow returns the window (end - rng, end] a selector evaluated
// over env reads, cut back to the stored range. It reports false unless
// the selector is evaluated at a single instant Prometheus reads at, its
// window reaches past an edge, and the window still overlaps the stored
// range (one that does not is empty, which the fit answers on its own).
func storageWindow(env domainEnv, rng time.Duration) (storageCut, bool) {
	if !env.known || env.wrapped || !env.lo.Equal(env.hi) {
		return storageCut{}, false
	}
	end, start := env.hi, env.hi.Add(-rng)
	if checkInstantDomain(end, end, rng, 0) == nil || outsideStorage(start, end) {
		return storageCut{}, false
	}
	cutEnd, cutStart := end, start
	if cutEnd.After(time.UnixMilli(storageEndMillis)) {
		cutEnd = time.UnixMilli(storageEndMillis).UTC()
	}
	if cutStart.Before(storageStart) {
		cutStart = storageStart
	}
	return storageCut{
		endMillis:   cutEnd.UnixMilli(),
		rng:         cutEnd.Sub(cutStart),
		startBeyond: cutStart.Sub(start),
		endBeyond:   end.Sub(cutEnd),
	}, true
}

// cutMatrix returns the matrix selector arg, evaluated over env, pinned at
// the end of its cut window with the cut window's range, when its window
// reaches past the stored range, and the cut.
func cutMatrix(arg parser.Expr, env domainEnv) (*parser.MatrixSelector, storageCut, bool) {
	ms, ok := unwrapParens(arg).(*parser.MatrixSelector)
	if !ok {
		return nil, storageCut{}, false
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, storageCut{}, false
	}
	at, _ := pinnedEnv(vs.Timestamp, vs.StartOrEnd, vs.OriginalOffset, env)
	cut, ok := storageWindow(at, ms.Range)
	if !ok {
		return nil, storageCut{}, false
	}
	return &parser.MatrixSelector{VectorSelector: pinSelector(vs, cut.endMillis), Range: cut.rng, EndPos: ms.EndPos}, cut, true
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
	cut, ok := storageWindow(at, instantLookback)
	start := at.hi.Add(-instantLookback)
	if !ok || cut.endBeyond == 0 || start.Before(instantDomainMin) {
		return nil, false
	}
	end := cut.endMillis
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

// extrapolatedRangeFunctions are the range functions that extrapolate
// their samples to the window's edges. Over a cut window the lowering
// carries how far the edges lie beyond it ([chplan.RangeWindow.StartBeyond],
// [chplan.RangeWindow.EndBeyond]).
var extrapolatedRangeFunctions = map[string]bool{
	"delta":    true,
	"increase": true,
	"rate":     true,
}

// cutWindowArg returns the call n with its range-vector argument cut back
// to the stored range when its window reaches past it and the cut changes
// nothing n answers. A function of sampleSetRangeFunctions reads the cut
// window as it is. One of extrapolatedRangeFunctions also reads how far
// the window's edges lie beyond the cut, which the fit records for the
// lowering ([domainFit.windowBeyond]).
func (f *domainFit) cutWindowArg(n *parser.Call, env domainEnv) (*parser.Call, bool) {
	name := n.Func.Name
	pos := 0
	if name == fnQuantileOverTime {
		pos = 1
	}
	if len(n.Args) <= pos {
		return nil, false
	}
	if !sampleSetRangeFunctions[name] && !extrapolatedRangeFunctions[name] || !f.cutsStorage() {
		return nil, false
	}
	ms, cut, ok := cutMatrix(n.Args[pos], env)
	if !ok {
		return nil, false
	}
	cp := *n
	cp.Args = append(parser.Expressions(nil), n.Args...)
	cp.Args[pos] = ms
	if extrapolatedRangeFunctions[name] {
		f.beyond = append(f.beyond, &beyondCut{ms: ms, cut: cut})
	}
	return &cp, true
}

// beyondCut is a matrix selector cut for a function that extrapolates
// to the window's edges, and whether its lowering read the cut.
type beyondCut struct {
	ms   *parser.MatrixSelector
	cut  storageCut
	read bool
}

// windowBeyond returns how far the edges of the window Prometheus reads
// for ms lie beyond the cut window ms reads, and records that the
// lowering read them.
func (f *domainFit) windowBeyond(ms *parser.MatrixSelector) (start, end time.Duration) {
	if f == nil {
		return 0, 0
	}
	for _, b := range f.beyond {
		if b.ms == ms {
			b.read = true
			return b.cut.startBeyond, b.cut.endBeyond
		}
	}
	return 0, 0
}

// checkBeyondRead rejects a query holding a cut window whose edges no
// lowering read: one lowered by a path that extrapolates to the cut
// window's own edges, which are not Prometheus's. It names the first such
// window's instant outside the stored range.
func (f *domainFit) checkBeyondRead() error {
	if f == nil {
		return nil
	}
	for _, b := range f.beyond {
		if b.read {
			continue
		}
		end := time.UnixMilli(b.cut.endMillis).UTC()
		if b.cut.endBeyond != 0 {
			return instantDomainError(end.Add(b.cut.endBeyond))
		}
		return instantDomainError(end.Add(-b.cut.rng - b.cut.startBeyond))
	}
	return nil
}
