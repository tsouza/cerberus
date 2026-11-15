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

// IsClassicBucketBudgetMessage reports whether msg is the message of one of
// the classic-histogram stage's own `throwIf` guards —
// [ClassicBucketMergeBudgetMessage] or
// [ClassicBucketWindowSeriesBudgetMessage]. A plan carrying a `throwIf` with
// either message carries a classic-histogram window or bucket-merge stage,
// which is how internal/engine tells a ClickHouse memory-limit abort of such
// a statement from any other.
func IsClassicBucketBudgetMessage(msg string) bool {
	switch msg {
	case ClassicBucketMergeBudgetMessage, ClassicBucketWindowSeriesBudgetMessage:
		return true
	}
	return false
}

// ClassicHistogramMemoryLimitMessage is the message the Prometheus head
// answers with when ClickHouse aborts a statement carrying a
// classic-histogram window or bucket-merge stage for exceeding its memory
// limit (code 241). The stage has no query-wide check of its own, so the
// server's cap is what refuses such a query; the head names the stage
// instead of reporting a generic memory-limit failure.
const ClassicHistogramMemoryLimitMessage = "classic-histogram quantile exceeds the ClickHouse memory limit"
