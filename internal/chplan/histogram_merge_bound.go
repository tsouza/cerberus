package chplan

// HistogramMergeBudgetMessage is the `throwIf` message the native-histogram
// across-series merge guard raises when the merge's resource bound is
// crossed: primarily the joint `rows x (merged bucket-range width)^2` cost
// the real cost driver measured (internal/promql/histogram_merge_bound.go's
// header doc has the calibration), plus a series-per-group ceiling that
// exists purely to keep that cost multiply from overflowing Int64 rather
// than as an independently-tuned business threshold (see that package's
// maxHistogramMergeRowCountOverflowGuard). It lives in chplan (not chsql)
// for the same reason [InfoConflictingLabelMessage] and
// [DuplicateLabelsetMessage] do: the lowering that builds the guard
// (internal/promql) may not import the SQL-emission layer (see
// .go-arch-lint.yml — promql may depend on chplan only), and the classifier
// that recognises the abort on the way back out (chsql) needs the identical
// string.
//
// Unlike its two siblings this is not a reference-Prometheus wire message —
// upstream has no server-side SQL engine to overrun this way — so the
// wording is cerberus's own, but the shape of the guard is identical: a
// `throwIf(<condition>, <this message>) = 0` predicate that aborts the query
// before the expensive per-row array evaluation runs, rather than letting
// ClickHouse discover the cost by allocating it (issue #2385: 19 production
// MEMORY_LIMIT_EXCEEDED failures, up to 6.31 GiB, from exactly this shape).
const HistogramMergeBudgetMessage = "native histogram merge exceeds the series-per-group or merged-bucket-width resource bound"

// ExpHistogramWindowSampleBudgetMessage is the abort text for cerberus
// issue #3252's samples-per-series-per-window pre-rejection
// (internal/promql's exp_histogram_window_sample_bound.go). It names the
// AXIS rather than the operator, because that axis — how densely one
// series is sampled inside one window — is the thing an operator can act
// on: shorten the range, coarsen the step of an inner subquery, or raise
// CERBERUS_CH_QUERY_MAX_MEMORY, which the ceiling is derived from.
const ExpHistogramWindowSampleBudgetMessage = "exponential-histogram window exceeds the samples-per-series-per-window resource bound"
