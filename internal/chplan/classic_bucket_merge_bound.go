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

// ClassicBucketWindowSeriesBudgetMessage is the `throwIf` message the
// instant classic-histogram window guard raises when one series' window
// collects more bucket entries — every in-window row's finite bounds plus its
// +Inf rung — than the configured ceiling
// (CERBERUS_PROMQL_CLASSIC_BUCKET_WINDOW_SERIES_MAX_ENTRIES).
const ClassicBucketWindowSeriesBudgetMessage = "classic-histogram window exceeds the bucket-entries-per-series resource bound"

// ClassicHistogramMemoryLimitMessage is the message the Prometheus head
// answers with when ClickHouse aborts a statement carrying a
// classic-histogram quantile ([HistogramQuantile]) for exceeding its memory
// limit (code 241). The quantile's stages are bounded per series and per
// merge, not as a whole, so the server's cap is what refuses such a query;
// the head names the quantile instead of reporting a generic memory-limit
// failure.
const ClassicHistogramMemoryLimitMessage = "classic-histogram quantile exceeds the ClickHouse memory limit"
