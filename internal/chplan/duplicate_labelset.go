package chplan

// DuplicateLabelsetMessage is the `throwIf` message a name-dropping PromQL
// construct raises when two input series that differed ONLY by `__name__`
// collapse onto one label set. It is the verbatim reference-engine text:
//
//	prometheus/prometheus@cerberus-parser/promql/engine.go
//	`ev.errorf("vector cannot contain metrics with the same labelset")`
//
// Upstream reaches it from `Matrix.ContainsSameLabelset()` right after a
// range-function call has stripped the name; cerberus reaches it from the
// HAVING guard `internal/promql`'s lowering attaches at the same boundary,
// so a query upstream rejects is rejected here with the same words rather
// than answered with a silently merged series.
//
// It lives in chplan (not chsql) for the same reason
// [InfoConflictingLabelMessage] does: the lowering that builds the guard
// may not import the SQL-emission layer (see .go-arch-lint.yml — promql
// may depend on chplan only).
//
// This is the message every duplicate-labelset guard call site still
// raises when chopt.FeatureTSThrowDuplicateSeriesIf (cerberus issue #3038)
// is off. When it is on, the guard's HAVING calls
// timeSeriesThrowDuplicateSeriesIf instead, and ClickHouse raises
// [DuplicateSeriesTagsMessagePrefix]'s message — naming the actual
// colliding tags — rather than this static text.
const DuplicateLabelsetMessage = "vector cannot contain metrics with the same labelset"

// DuplicateSeriesTagsMessagePrefix is the STABLE prefix of the message
// ClickHouse's `timeSeriesThrowDuplicateSeriesIf` raises
// (chopt.FeatureTSThrowDuplicateSeriesIf, cerberus issue #3038) — verified
// directly against a real ClickHouse 26.2+ server:
//
//	Multiple series have the same tags <tags>, duplicate series in the
//	same result set are not allowed
//
// Unlike [DuplicateLabelsetMessage], this is not a full literal cerberus
// supplies as a throwIf argument — ClickHouse itself renders the <tags>
// portion from its own per-query tag-group collector, so only the prefix
// is a compile-time constant a caller can match on. A caller that wants
// the actual colliding tags extracts the full sentence from the decoded
// exception (internal/api/prom's classifyThrowIfGuardError does this)
// rather than restating a known literal the way every other
// classifyThrowIfGuardError case does.
const DuplicateSeriesTagsMessagePrefix = "Multiple series have the same tags "
