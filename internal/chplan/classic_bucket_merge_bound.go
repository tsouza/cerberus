package chplan

// ClassicBucketMergeBudgetMessage is the `throwIf` message the
// classic-histogram cross-series bucket merge guard
// (internal/promql/classic_bucket_merge_bound.go) raises when one query's
// merge would read more bucket entries — every per-series row's finite
// bounds plus its +Inf rung, across every group and step — than the
// configured ceiling
// (CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_ENTRIES).
//
// It lives in chplan for the same reason [HistogramMergeBudgetMessage]
// does: the lowering that builds the guard (internal/promql) may not import
// the SQL-emission layer (.go-arch-lint.yml), and the classifier that
// recognises the abort on the way back out (internal/api/prom) needs the
// identical string.
const ClassicBucketMergeBudgetMessage = "classic-histogram bucket merge exceeds the " +
	"merged-bucket-entry resource bound"
