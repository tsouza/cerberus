package chplan

import "slices"

// InfoConflictingLabelMessage is the `throwIf` message a query-time info()
// abort carries, shared between the lowering layer that plants the guard
// (internal/promql's collapseInfoSeriesBySignature, internal/chsql's
// infoConflictGuardFrag) and the classifier that recognises it on the way
// back out (chsql.IsInfoConflictingLabelError). It lives in chplan — the
// one layer both promql and chsql already depend on — rather than in
// chsql, so promql can embed it in a plan expression without importing the
// SQL-emission layer (see .go-arch-lint.yml: promql may depend on chplan
// only).
//
// Two distinct scenarios raise it, both mirroring the reference engine's
// refusal to silently pick a winner:
//
//   - collapseInfoSeriesBySignature: two ROWS on the info side share one
//     (metric name, identity labels) signature AND tie exactly on the
//     greatest timestamp (promql/info.go's "found duplicate series for
//     info metric").
//   - infoConflictGuardFrag: two matched info METRICS (MergeInfoMetrics)
//     disagree on the value of the same data label (promql/info.go's
//     "conflicting label: %s").
//
// Upstream distinguishes the two with different, more specific messages
// that name the offending series/label; ClickHouse requires `throwIf`'s
// message to be a compile-time constant, so cerberus can't interpolate
// per-row detail into it. The observable behaviour both scenarios need —
// the query aborts instead of returning an arbitrary pick — is what the
// divergence in wording is about.
const InfoConflictingLabelMessage = "conflicting label"

// InfoJoin models PromQL's `info(v[, {label matchers}])` label-enrichment
// join. PromQL's reference engine (promql/info.go::evalInfo) enriches each
// base series in `v` with the data labels carried by a companion info
// metric (default `target_info`) that shares the same identifying labels
// (`instance` / `job`). Sample values + timestamps pass through unchanged;
// only the label set grows.
//
// Lowering shape:
//
//   - Input  — the base vector (arg[0] of `info`), already lowered to the
//     canonical per-series-latest Sample shape (MetricName, Attributes,
//     TimeUnix, Value).
//   - Info   — the info-metric scan (default `target_info`, or the name
//     selected by the second arg's `__name__` matcher), lowered through
//     the same VectorSelector path and then COLLAPSED to one row per
//     (metric name, identity-label values) signature — see
//     internal/promql's collapseInfoSeriesBySignature. Two info series
//     sharing a signature (e.g. two target_info scrapes with the same
//     job/instance but different build versions) keep only the sample
//     with the greatest timestamp; an exact tie on that timestamp aborts
//     the query rather than picking one arbitrarily (mirrors upstream's
//     combineWithInfoSeries/sigFunction).
//
// IdentityLabels are the labels the join keys on — the reference engine
// hard-codes `{instance, job}`.
//
// DropUnmatched selects the join flavour. The reference engine keeps a
// base sample that matched no info series only when every data-label
// matcher also matches the empty string (combineWithInfoVector's
// `allMatchersMatchEmpty` branch): `info(v)` and `info(v, {k=~".*"})`
// keep it, `info(v, {k="x"})` and `info(v, {k=~".+"})` do not. False →
// LEFT JOIN (pass unmatched base samples through unenriched); true →
// INNER JOIN (drop them).
//
// DataLabels, when non-empty, restricts the set of info labels copied onto
// the output to exactly those names — the second-arg label-matcher case
// (`info(v, {k=~"…"})`). Empty DataLabels means "copy every info label not
// already present on the base" (the default case). The identity labels are
// never copied (they're already on the base by construction), and the info
// metric's `__name__` is never copied.
//
// MergeInfoMetrics is set when the second argument's `__name__` matchers
// can select more than one info metric (a regex, a negation, or several
// matchers). A base series then joins one row per matched info metric,
// while the reference engine unions all of their data labels onto a
// single output sample. The emitter folds the extra rows back together
// by grouping on the base sample's identity. A lone `__name__` equality
// (including the default `target_info`) matches at most one info metric
// per identity key, so the fold is skipped.
//
// The output Attributes is `mapConcat(infoExtras, base.Attributes)` so the
// base side wins on any conflicting key — matching the reference rule that
// skips info labels already present on the base series.
type InfoJoin struct {
	Input Node
	Info  Node

	// IdentityLabels are the labels the join matches on (instance/job).
	IdentityLabels []string
	// DataLabels restricts which info labels are copied onto the output.
	// Empty → copy every non-identity, non-__name__ info label.
	DataLabels []string
	// DropUnmatched drops base samples that matched no info series
	// (INNER JOIN) instead of passing them through (LEFT JOIN).
	DropUnmatched bool
	// MergeInfoMetrics folds the per-info-metric join rows of one base
	// sample back into a single output row with the union of their data
	// labels.
	MergeInfoMetrics bool

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*InfoJoin) planNode() {}

func (j *InfoJoin) Children() []Node { return []Node{j.Input, j.Info} }

func (j *InfoJoin) Equal(other Node) bool {
	o, ok := other.(*InfoJoin)
	if !ok {
		return false
	}
	if !slices.Equal(j.IdentityLabels, o.IdentityLabels) ||
		!slices.Equal(j.DataLabels, o.DataLabels) {
		return false
	}
	if j.DropUnmatched != o.DropUnmatched || j.MergeInfoMetrics != o.MergeInfoMetrics {
		return false
	}
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn ||
		j.ValueColumn != o.ValueColumn {
		return false
	}
	return j.Input.Equal(o.Input) && j.Info.Equal(o.Info)
}
