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
// hard-codes `{instance, job}`. The emitter renders a LEFT JOIN so base
// series with no matching info series pass through unchanged (the default
// `target_info` case has no data-label matcher, so unmatched base series
// are always kept — see combineWithInfoVector's `allMatchersMatchEmpty`
// branch).
//
// DataLabels, when non-empty, restricts the set of info labels copied onto
// the output to exactly those names — the second-arg label-matcher case
// (`info(v, {k=~"…"})`). Empty DataLabels means "copy every info label not
// already present on the base" (the default case). The identity labels are
// never copied (they're already on the base by construction), and the info
// metric's `__name__` is never copied.
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
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn ||
		j.ValueColumn != o.ValueColumn {
		return false
	}
	return j.Input.Equal(o.Input) && j.Info.Equal(o.Info)
}
