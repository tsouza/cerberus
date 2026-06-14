package chplan

import "slices"

// InfoJoin enriches each Base series with the data labels of a matching
// Info series, joining on the identifying labels (PromQL `info()`'s
// hard-coded `instance` + `job`). It is the plan-IR form of the
// experimental PromQL `info(v instant-vector, [label-selector])`
// function.
//
// Semantics (mirroring prometheus/promql/info.go::evalInfo):
//
//   - The join key is the set of IdentifyingLabels present and non-empty
//     on the Base series. The emitter builds the per-series signature as
//     `mapFilter((k, v) -> k IN (identifying…), Attributes)` on both
//     sides and joins on signature equality. Identifying labels absent
//     from a Base series simply don't participate in that series' key
//     (matching the reference, which skips empty identifying values).
//
//   - Output Value / TimeUnix / MetricName come from the Base series,
//     UNCHANGED — info() only enriches labels.
//
//   - Output Attributes = the Info series' data labels (every Info label
//     that is NOT an identifying label and NOT `__name__`) overlaid
//     UNDER the Base labels: base labels win on conflict. The emitter
//     renders this as `mapConcat(InfoDataLabels, BaseAttributes)` so the
//     later (Base) map keys overwrite the earlier (Info) ones — CH's
//     later-key-wins merge. When no Info series matches a Base series the
//     LEFT JOIN yields an empty Info map, so the output is just the Base
//     Attributes (the "return base series unenriched" branch).
//
//   - DataLabelFilter, when non-empty, restricts WHICH info data labels
//     are copied onto the output — only labels named in DataLabelFilter
//     survive the `mapFilter` on the info side. It is the lowered form of
//     the non-`__name__` matchers in the optional `{…}` selector. Empty
//     means "copy every data label" (the bare `info(v)` shape, or a
//     selector with only a `__name__` matcher).
//
// The Info side is an ordinary Sample-shaped subtree (a Scan+Filter+LWR
// over the info metric), so PREWHERE promotion and the LWR collapse apply
// to it exactly as they do to a normal selector.
type InfoJoin struct {
	Base Node
	Info Node

	// IdentifyingLabels is the ordered set of labels used as the join
	// key — PromQL hard-codes {"instance", "job"}.
	IdentifyingLabels []string

	// DataLabelFilter restricts which info data labels are copied onto
	// the output. Empty = copy every (non-identifying, non-__name__)
	// info label.
	DataLabelFilter []string

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*InfoJoin) planNode() {}

func (j *InfoJoin) Children() []Node { return []Node{j.Base, j.Info} }

func (j *InfoJoin) Equal(other Node) bool {
	o, ok := other.(*InfoJoin)
	if !ok {
		return false
	}
	if !slices.Equal(j.IdentifyingLabels, o.IdentifyingLabels) {
		return false
	}
	if !slices.Equal(j.DataLabelFilter, o.DataLabelFilter) {
		return false
	}
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn ||
		j.ValueColumn != o.ValueColumn {
		return false
	}
	return j.Base.Equal(o.Base) && j.Info.Equal(o.Info)
}
