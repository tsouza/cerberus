package chplan

// ClassicBucketWindowSeriesBudgetMessage is the `throwIf` message the
// instant classic-histogram window guard raises when one series' window
// collects more bucket entries — every in-window row's finite bounds plus its
// +Inf rung — than the configured ceiling
// (CERBERUS_PROMQL_CLASSIC_BUCKET_WINDOW_SERIES_MAX_ENTRIES).
//
// It lives in chplan for the same reason [HistogramMergeBudgetMessage]
// does: the lowering that builds the guard (internal/promql) may not import
// the SQL-emission layer (.go-arch-lint.yml), and the classifier that
// recognises the abort on the way back out (internal/api/prom) needs the
// identical string.
const ClassicBucketWindowSeriesBudgetMessage = "classic-histogram window exceeds the bucket-entries-per-series resource bound"

// ClassicHistogramMemoryLimitMessage is the message the Prometheus head
// answers with when ClickHouse aborts a statement carrying a
// classic-histogram quantile ([HistogramQuantile]) for exceeding its memory
// limit (code 241). No cerberus check bounds the quantile's stages as a
// whole (the instant window stage is bounded per series only), so the
// server's cap is what refuses such a query; the head names the quantile
// instead of reporting a generic memory-limit failure.
const ClassicHistogramMemoryLimitMessage = "classic-histogram quantile exceeds the ClickHouse memory limit"
