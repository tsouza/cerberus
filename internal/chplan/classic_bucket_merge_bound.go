package chplan

// ClassicBucketMergeBudgetMessage is the `throwIf` message the
// classic-histogram cross-series bucket merge guard
// (internal/promql/classic_bucket_merge_bound.go) raises when its resource
// bound is crossed: the `(total bucket-element volume in the group) x
// (widest single row's own bucket count)` cost that package's header doc
// calibrates against a real ClickHouse server.
//
// It lives in chplan (not promql or chsql) for the same reason
// [HistogramMergeBudgetMessage] does: the lowering that builds the guard
// (internal/promql) may not import the SQL-emission layer (see
// .go-arch-lint.yml — promql may depend on chplan only), and the classifier
// that recognises the abort on the way back out (internal/api/prom) needs
// the identical string.
//
// Cerberus's own wording, mirroring [HistogramMergeBudgetMessage] exactly:
// a `throwIf(<condition>, <this message>) = 0` predicate that aborts the
// query before the expensive cross-series array evaluation runs, rather
// than letting ClickHouse discover the cost by allocating it (issue #2408:
// a real audited benchmark found this stage — classicBucketMergeShaping,
// inside lowerHistogramQuantileClassicAggRange / lowerHistogramQuantileAgg —
// dominates memory at realistic query width regardless of which per-series
// lowering mechanism feeds it, and had no resource bound of its own).
const ClassicBucketMergeBudgetMessage = "classic-histogram bucket merge exceeds the " +
	"bucket-volume-times-widest-row resource bound"
