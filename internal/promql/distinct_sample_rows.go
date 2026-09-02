package promql

import "github.com/tsouza/cerberus/internal/chplan"

// This file carries the PromQL head's answer to "how many samples does one
// (series, timestamp) contribute" for the `*_over_time` family — cerberus
// issue #2914.
//
// # The rule
//
// A window's sample multiset is the DISTINCT (timestamp, value) rows it
// contains. Two stored rows carrying the same timestamp AND the same value
// are one sample written twice; two rows sharing a timestamp but carrying
// different values are two observations and both count.
//
// One rule, stated once, from which both answers follow — it is not a branch
// on the values. The sample count is the cardinality of a set, and the two
// outcomes are that one cardinality computed over two different inputs.
//
// # Why the identity is the (timestamp, value) pair
//
// Because that is the identity Prometheus itself uses at ingest.
// tsdb.memSeries.appendable compares the incoming sample against the one
// already stored at that timestamp by VALUE BITS: equal bits are a legal
// no-op (the append succeeds and stores nothing), different bits are
// ErrDuplicateSampleForTimestamp (the append is rejected). So an
// identical-row duplicate is storage duplication that Prometheus absorbs
// and cerberus must absorb too, and a conflicting-value duplicate is a shape
// Prometheus never stores at all — leaving cerberus no reference answer to
// match, which is exactly why those fixtures are parity-exempt and pinned by
// a cerberus-vs-cerberus differential instead (cerberus issue #2905). This
// change does not disturb that: a conflicting-value pair still contributes
// both rows.
//
// # Why it is stamped per node here rather than applied in the emitter
//
// chsql's windowed-array emitters are shared by all three heads, and the
// (timestamp, value) pair is the right row identity for a METRIC SAMPLE
// only. A LogQL `unwrap` sample's identity is the log entry: two different
// entries at one timestamp can unwrap to the same number, and collapsing
// them would drop an event Loki counts. TraceQL spans are the same. Those
// heads therefore leave chplan.RangeWindow.DistinctSampleRows false and
// their windows keep every row; the emitter reads the field rather than the
// function name.
//
// # Why the rate family is left alone
//
// rate / increase / delta / irate / idelta already reduce to one sample per
// distinct TIMESTAMP (chsql's dedupWindowPairsByTsFrag, cerberus issue
// #1092) because their arithmetic divides by a sample interval and cannot
// evaluate a zero-length one. That is a STRICTLY STRONGER collapse which
// subsumes this one: on an identical-row duplicate the two rules produce the
// same multiset, so the families already agree wherever the question has a
// defined answer. They differ only on the conflicting-value shape, where the
// rate family must pick a survivor to stay defined and the `*_over_time`
// family has no such forcing — the settled #1092 / #2905 split, which this
// change deliberately leaves in place.

// distinctSampleRowsFuncs is the set of range functions whose window this
// head declares to be a multiset of distinct (timestamp, value) metric
// samples.
//
// It is the whole `*_over_time` family, including the members the collapse
// provably cannot move (min / max / first / last / present / ts_of_*):
// declaring the rule for the family and letting it be a no-op where the
// reducer is an order statistic or a presence flag is what makes the family
// answer under ONE rule. Naming only the reducers that can see it would
// re-create, at the level of function names, exactly the per-function split
// this issue exists to close.
//
// Excluded, each for a stated reason rather than by omission:
//
//   - rate / increase / delta / irate / idelta — the rate family, already
//     governed by the stronger per-timestamp collapse of cerberus issue
//     #1092 (see this file's doc). Setting the field for them would be
//     inert on the arms that carry that collapse and would introduce a NEW
//     divergence on the arms that do not (the lagInFrame adjacency shape,
//     chplan.RangeWindow.LagAdjacency, reduces rows rather than an array and
//     reads no sample-array assembly at all).
//   - changes / resets — neither family, and inherently immune besides:
//     repeating a value creates neither a value change nor a counter reset.
//   - deriv / predict_linear — neither family, and each has a native
//     timeSeries*ToGrid competitor (timeSeriesDerivToGrid,
//     timeSeriesPredictLinearToGrid, chplan.RangeWindowGridNative) that
//     aggregates server-side and could not pick up a rule applied at the
//     array-assembly gate, so setting the field would answer one way on the
//     fan-out arm and another on the native one.
//   - holt_winters — neither family, and the only one of these with no arm
//     that blocks it; it is left out because its weighting has no pinned
//     contract to change it against, not because it is unaffected.
//
// Those five, plus irate / idelta above, are tracked together in cerberus
// issue #2927, which enumerates them from the emitter's own dispatch so the
// remainder is a closed list rather than a sample. Two further shapes need no
// entry at all: LogQL's log_rate reduces log entries rather than metric
// samples (see this file's doc), and the bare-subquery `identity` shape reads
// the time-latest sample, which no collapse can move.
var distinctSampleRowsFuncs = map[string]bool{
	"sum_over_time":         true,
	"avg_over_time":         true,
	"count_over_time":       true,
	"min_over_time":         true,
	"max_over_time":         true,
	"first_over_time":       true,
	"last_over_time":        true,
	"present_over_time":     true,
	"stddev_over_time":      true,
	"stdvar_over_time":      true,
	"mad_over_time":         true,
	"quantile_over_time":    true,
	"ts_of_first_over_time": true,
	"ts_of_last_over_time":  true,
	"ts_of_max_over_time":   true,
	"ts_of_min_over_time":   true,
}

// stampDistinctSampleRows marks every `*_over_time` chplan.RangeWindow in
// plan as reducing distinct (timestamp, value) metric sample rows, and
// returns plan.
//
// It is a post-lowering sweep rather than an assignment inside each
// RangeWindow constructor on purpose: this head builds a RangeWindow at a
// dozen sites (range_fns.go, subquery.go, the histogram-native shapes), and
// a rule that has to be remembered at each of them is a rule that will be
// missed at the next one. One sweep over the finished plan cannot be.
//
// chplan.WalkDeep, not chplan.Walk: a per-step scalar argument binds its
// vector as a chplan.ScalarSubquery, so a `quantile_over_time(scalar(m), …)`
// or a `topk(scalar(count_over_time(m[5m])), …)` hangs its window off an
// Expr slot that Walk does not follow. Missing one there would leave that
// window counting duplicated rows while its siblings do not — the
// cross-lowering divergence this rule exists to remove.
func stampDistinctSampleRows(plan chplan.Node) chplan.Node {
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && distinctSampleRowsFuncs[rw.Func] {
			rw.DistinctSampleRows = true
		}
		return true
	})
	return plan
}
