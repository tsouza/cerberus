package chplan

import "sort"

// rangeWindowSurfaces records which query-language surfaces can reach a
// range-window reducer from source. A reducer the emitter supports is not
// automatically nameable in every head: `log_rate` exists only for LogQL's
// `rate()` over a log stream, and admitting it to PromQL would accept a
// function reference no PromQL parser produces.
type rangeWindowSurfaces struct {
	// promQL reports whether a PromQL query can name the reducer at
	// source — i.e. whether the PromQL head may lower a call to it, and
	// so whether it may carry a subquery argument.
	promQL bool
}

// rangeWindowFuncs is the vocabulary of [RangeWindow].Func values: every
// name the SQL emitter dispatches on, with the surfaces that can reach it.
//
// This is the ONE definition of that vocabulary. It was two — the
// emitter's dispatch in internal/chsql and a hand-written `rangeVectorFn`
// set in internal/promql that gated whether a function accepts a subquery
// argument — with a docstring on each pointing at the other and asserting
// they agreed. They describe one fact: which reducer names the windowed-
// array emitters handle in both instant and matrix (OuterRange > 0)
// modes. Two hand-maintained answers to one question drift silently, and
// the drift is invisible in both directions: a name in the emitter but
// not the PromQL set rejects a query the SQL could have served, and a
// name in the PromQL set but not the emitter lowers a plan that fails at
// emit time with ErrUnsupported instead of a parse-time rejection.
//
// The emitter's own name→emit-method dispatch cannot live here — its
// arms bind to unexported chsql methods, so they are not a value any
// package can read — and it stays a switch in internal/chsql. The
// TestRangeWindowVocabularyIsDerived ratchet re-derives that switch's arm
// set from source (internal/dispatchscan) and diffs it against this
// vocabulary, in both directions. The PromQL head has no set of its own
// at all: it derives its gate from [IsPromQLRangeWindowFunc].
//
// Membership notes, carried from the set this replaced:
//
//   - predict_linear / holt_winters / quantile_over_time carry extra
//     scalar arguments beyond the subquery itself; the PromQL head
//     threads them onto the RangeWindow the same way its non-subquery
//     lowerers do (#1456).
//   - `double_exponential_smoothing` is absent deliberately. It is a
//     PromQL SOURCE spelling, not an IR name: the head canonicalises it
//     to `holt_winters` before the name reaches the IR, so a second entry
//     here would be a second name for one reducer rather than a second
//     reducer.
//   - first_over_time sits beside last_over_time because the two are the
//     `__name__`-preserving pair: admitting one over a subquery but not
//     the other would make the name contract unobservable for half of it.
//   - present_over_time and mad_over_time are plain reducers in the
//     emitter's shared `emitRangeWindowOverTime` case, carry no extra
//     scalar, and already lower over a bare matrix selector. Withholding
//     them would reject `mad_over_time(m[5m:1m])` while accepting
//     `mad_over_time(m[5m])` — a split reference Prometheus does not
//     make.
//   - The four ts_of_*_over_time reducers route through
//     `emitRangeWindowTsOfOverTime`, which reduces the per-anchor window
//     array exactly as `emitRangeWindowOverTime` does and is likewise
//     agnostic about whether the rows underneath came from a matrix
//     selector or a subquery's anchor grid.
//
// A reducer that lowers to its OWN node shape rather than to a
// RangeWindow — absent_over_time, which becomes an AbsentOverTime — is
// not in this vocabulary at all, and the heads reject a subquery argument
// to it through their generic "does not accept a subquery argument" path.
var rangeWindowFuncs = map[string]rangeWindowSurfaces{
	"rate":     {promQL: true},
	"irate":    {promQL: true},
	"increase": {promQL: true},
	"delta":    {promQL: true},
	"idelta":   {promQL: true},
	"deriv":    {promQL: true},
	"resets":   {promQL: true},

	"changes":               {promQL: true},
	"sum_over_time":         {promQL: true},
	"avg_over_time":         {promQL: true},
	"min_over_time":         {promQL: true},
	"max_over_time":         {promQL: true},
	"count_over_time":       {promQL: true},
	"first_over_time":       {promQL: true},
	"last_over_time":        {promQL: true},
	"stddev_over_time":      {promQL: true},
	"stdvar_over_time":      {promQL: true},
	"present_over_time":     {promQL: true},
	"mad_over_time":         {promQL: true},
	"ts_of_first_over_time": {promQL: true},
	"ts_of_last_over_time":  {promQL: true},
	"ts_of_max_over_time":   {promQL: true},
	"ts_of_min_over_time":   {promQL: true},
	"predict_linear":        {promQL: true},
	"holt_winters":          {promQL: true},
	"quantile_over_time":    {promQL: true},
	// LogQL's `rate()` over a log stream. No PromQL parser produces the
	// name, so the PromQL head must not accept it.
	"log_rate": {promQL: false},
}

// IsPromQLRangeWindowFunc reports whether name is a [RangeWindow].Func
// the emitter reduces as a range-vector windowed array AND a PromQL query
// can name at source.
//
// The name is the IR name: a caller holding a PromQL source spelling
// canonicalises it first (the `double_exponential_smoothing` →
// `holt_winters` rewrite the upstream parser's spelling forces).
func IsPromQLRangeWindowFunc(name string) bool {
	return rangeWindowFuncs[name].promQL
}

// RangeWindowFuncNames returns every [RangeWindow].Func in the
// vocabulary, sorted. The SQL emitter's dispatch ratchet diffs its own
// table against this, so a reducer added to one side and not the other
// fails rather than silently splitting the vocabulary in two.
func RangeWindowFuncNames() []string {
	out := make([]string, 0, len(rangeWindowFuncs))
	for name := range rangeWindowFuncs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
