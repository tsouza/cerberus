package chplan

import "slices"

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
//     selected by the second arg's `__name__` matcher), also lowered to
//     the per-series-latest Sample shape so each base series matches at
//     most one info series per identity key.
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
