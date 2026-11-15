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

// ClassicBucketWindowBudgetMessage is the `throwIf` message the instant
// classic-histogram window guard (internal/promql/classic_bucket_window_bound.go)
// raises when its pre-count estimates that the per-series window stage — the
// series it groups, the bucket entries it collects and the fold that reduces
// them — would use more memory than the configured ceiling
// (CERBERUS_PROMQL_CLASSIC_BUCKET_WINDOW_MAX_COST_UNITS). It lives here beside
// [ClassicBucketMergeBudgetMessage] for the same reason: the lowering that
// builds the guard may not import the SQL-emission layer, and the classifier
// in internal/api/prom needs the identical string.
const ClassicBucketWindowBudgetMessage = "classic-histogram window exceeds the estimated-memory resource bound"

// ClassicBucketWindowSeriesBudgetMessage is the `throwIf` message the
// instant classic-histogram window guard raises when one series' window
// collects more bucket entries — every in-window row's finite bounds plus its
// +Inf rung — than the configured ceiling
// (CERBERUS_PROMQL_CLASSIC_BUCKET_WINDOW_SERIES_MAX_ENTRIES).
const ClassicBucketWindowSeriesBudgetMessage = "classic-histogram window exceeds the bucket-entries-per-series resource bound"

// ClassicBucketGuardMaxThreads is the ClickHouse thread count the
// classic-histogram memory ceilings are calibrated at: the bucket-merge
// ceiling and the two window ceilings, each measured on a server
// running the statement on two threads. A statement carrying one of their
// checks ([IsClassicBucketBudgetMessage]) runs on at most this many threads,
// because the engine stamps `max_threads` to it (internal/engine,
// applyClassicBucketThreadBound), so a ceiling sized for this count holds
// whatever the server's own `max_threads` is.
const ClassicBucketGuardMaxThreads = 2

// IsClassicBucketBudgetMessage reports whether msg is the `throwIf` message
// of one of the classic-histogram memory checks:
// [ClassicBucketMergeBudgetMessage], [ClassicBucketWindowBudgetMessage] or
// [ClassicBucketWindowSeriesBudgetMessage].
func IsClassicBucketBudgetMessage(msg string) bool {
	switch msg {
	case ClassicBucketMergeBudgetMessage, ClassicBucketWindowBudgetMessage, ClassicBucketWindowSeriesBudgetMessage:
		return true
	}
	return false
}
