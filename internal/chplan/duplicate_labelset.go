package chplan

// DuplicateLabelsetMessage is the `throwIf` message a name-dropping PromQL
// construct raises when two input series that differed ONLY by `__name__`
// collapse onto one label set. It is the verbatim reference-engine text:
//
//	prometheus/prometheus@cerberus-parser/promql/engine.go:2295
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
const DuplicateLabelsetMessage = "vector cannot contain metrics with the same labelset"
