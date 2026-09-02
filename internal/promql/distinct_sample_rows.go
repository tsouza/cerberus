package promql

import "github.com/tsouza/cerberus/internal/chplan"

// This file carries the PromQL head's answer to "how many samples does one
// (series, timestamp) contribute" — cerberus issues #2914 and #2927.
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
// # Why the extrapolating rate family is left alone
//
// rate / increase / delta reduce to one sample per distinct TIMESTAMP
// (chsql's dedupWindowPairsByTsFrag, cerberus issue #1092) because their
// arithmetic divides by a sample interval and cannot evaluate a zero-length
// one. That is a STRICTLY STRONGER collapse which subsumes this one: on an
// identical-row duplicate the two rules produce the same multiset, so the
// families already agree wherever the question has a defined answer. They
// differ only on the conflicting-value shape, where the extrapolating family
// must pick a survivor to stay defined and the rest has no such forcing —
// the settled #1092 / #2905 split, which this file deliberately leaves in
// place.
//
// irate and idelta are NOT part of that: they read the trailing pair rather
// than extrapolate across the window, they never route through
// dedupWindowPairsByTsFrag, and until cerberus issue #2927 they were the two
// functions whose window counted a duplicated row twice with the sharpest
// consequence — irate's trailing pair became the duplicate against ITSELF,
// spanning zero time, so the answer was NaN and the series vanished from the
// result entirely. They are declared below like every other reducer over
// metric samples.

// distinctSampleRowsFuncs is the set of range functions whose window this
// head declares to be a multiset of distinct (timestamp, value) metric
// samples.
//
// It is every PromQL range function over metric samples except the
// extrapolating rate family, INCLUDING the members the collapse provably
// cannot move (min / max / first / last / present / ts_of_*, changes and
// resets): declaring the rule for the whole family and letting it be a no-op
// where the reducer is an order statistic, a presence flag or an
// adjacent-pair predicate is what makes the family answer under ONE rule.
// Naming only the reducers that can see it would re-create, at the level of
// function names, exactly the per-function split cerberus issues #2914 and
// #2927 exist to close.
//
// Excluded, each for a stated reason rather than by omission:
//
//   - rate / increase / delta — the extrapolating rate family, already
//     governed by the stronger per-timestamp collapse of cerberus issue
//     #1092 (see this file's doc). Setting the field for them would be inert
//     on every arm that carries that collapse and would state a second,
//     weaker rule for the one shape the two rules disagree about.
//   - log_rate — LogQL only, where a row's identity is the log entry rather
//     than its (timestamp, value) projection (see this file's doc).
//   - the bare-subquery `identity` shape (chplan.RangeWindow.Identity, Func
//     empty), whose reducer is the time-latest sample of its window — a value
//     no collapse can move, since dropping a row identical to that sample
//     leaves the sample standing.
//
// absent_over_time needs no entry rather than an exclusion: it lowers to its
// own chplan.AbsentOverTime node instead of a RangeWindow (lowerAbsentOverTime,
// internal/promql/absent.go), and asks only whether an anchor's window holds
// any matching sample at all — which repeating a row cannot change.
//
// # Which arms the declaration actually reaches, and which are immune
//
// The field is read at chsql's ONE array-assembly gate,
// windowSamplePairsFrag, plus the lagInFrame adjacency shape's own
// distinct-rows layer (chsql's range_window_lag_adjacency.go). Two families
// of arm are reached by neither and are safe by IMMUNITY, measured rather
// than assumed — see internal/promql's
// duplicate_row_range_family_chdb_test.go and
// duplicate_timestamp_seed_chdb_test.go, which execute them:
//
//   - the native timeSeries*ToGrid arms (chplan.RangeWindowGridNative, and
//     the chplan.RangeWindowStaleResample arm of last_over_time). Every
//     member of that ClickHouse family collapses a duplicate (series,
//     timestamp) inside the builtin, which is the STRONGER per-timestamp
//     rule and therefore agrees with this one on an identical-row duplicate.
//     The two rules part company only on a NaN-bearing duplicate, where the
//     builtin's survivor follows scan order — the family-wide gap cerberus
//     tracks at https://github.com/tsouza/cerberus/issues/2798, unchanged by
//     this file.
//   - the downsample tier of cerberus issue #2751 (irate / idelta /
//     last_over_time), which reads pre-aggregated
//     timeSeriesLastTwoSamples rollup state rather than raw sample rows.
//     That aggregate collapses a duplicate (series, timestamp) as it folds,
//     so the tier's trailing pair is already the contract's.
//
// That distinction — reached by the gate versus immune to it — is exactly
// what to re-check before adding any function whose reducer is not an order
// statistic.
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
	"irate":                 true,
	"idelta":                true,
	"deriv":                 true,
	"predict_linear":        true,
	"holt_winters":          true,
	"changes":               true,
	"resets":                true,
}

// stampDistinctSampleRows marks every declared chplan.RangeWindow in plan as
// reducing distinct (timestamp, value) metric sample rows, and returns plan.
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
