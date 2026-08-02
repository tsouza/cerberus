package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// infoMetricNameLabel is the reserved label carrying a series' metric
// name in the Attributes-keyed label space. The info-enrichment join must
// never copy it from the info series onto the base series — the base
// series keeps its own `__name__` (PromQL's info() preserves the base
// sample's identity and only grows its data labels).
const infoMetricNameLabel = "__name__"

// emitInfoJoin renders PromQL's `info(v[, {matchers}])` label-enrichment
// join. The output keeps the base side's MetricName / TimeUnix / Value
// verbatim and grows its Attributes with the info series' data labels.
//
// Shape:
//
//	SELECT
//	    L.MetricName AS MetricName,
//	    mapConcat(<info extras>, L.Attributes) AS Attributes,
//	    L.TimeUnix    AS TimeUnix,
//	    L.Value       AS Value
//	FROM (<base>) AS L
//	LEFT JOIN (<info>) AS R
//	  ON L.Attributes['job'] = R.Attributes['job']
//	 AND L.Attributes['instance'] = R.Attributes['instance']
//
// The join is LEFT so base series with no matching info series pass
// through unchanged — matching the reference engine, whose default
// `target_info` case (no data-label matcher) keeps every unmatched base
// series. When InfoJoin.DropUnmatched is set the join is INNER instead:
// a data-label matcher that cannot match the empty string makes an
// unmatched base sample unrepresentable, and the reference engine drops
// it (see combineWithInfoVector's `allMatchersMatchEmpty` branch).
//
// When InfoJoin.MergeInfoMetrics is set the `__name__` matchers may
// select several info metrics at once, so one base sample can join
// several info rows. The reference engine unions their data labels onto
// a single output sample; the emitter reproduces that by grouping on the
// base sample's four columns and folding the per-row extras maps into
// one (see infoMergedExtrasFrag).
//
// `<info extras>` is the info Attributes with the identity labels
// + `__name__` stripped (and, when DataLabels is set, narrowed to exactly
// those names); `mapConcat(extras, base)` lets the base side win on any
// conflicting key, mirroring the reference rule that skips info labels
// already present on the base.
//
// CH map subscript of a missing key returns the empty string, so the
// per-identity-label equality matches the reference identity semantics:
// a base series carrying only `job` joins an info series carrying only
// `job` (both sides see `”` for the absent `instance`), and a base
// carrying `instance` does NOT match an info series lacking it.
func (e *emitter) emitInfoJoin(j *chplan.InfoJoin) error {
	if err := e.validateInfoJoinCols(j); err != nil {
		return err
	}
	if len(j.IdentityLabels) == 0 {
		return fmt.Errorf("%w: InfoJoin.IdentityLabels empty", ErrUnsupported)
	}

	leftFrag, err := e.subqueryFrag(j.Input)
	if err != nil {
		return err
	}
	rightFrag, err := e.subqueryFrag(j.Info)
	if err != nil {
		return err
	}

	kind := LeftJoin
	if j.DropUnmatched {
		kind = InnerJoin
	}

	sb := NewQuery().
		Select(
			As(qualColFrag("L", j.MetricNameColumn), j.MetricNameColumn),
			As(infoOutputAttributesFrag(j), j.AttributesColumn),
			As(qualColFrag("L", j.TimestampColumn), j.TimestampColumn),
			As(qualColFrag("L", j.ValueColumn), j.ValueColumn),
		).
		From(aliasedFrag(leftFrag, "L")).
		Join(
			kind,
			aliasedFrag(rightFrag, "R"),
			infoJoinPredicateFrag(j),
		)
	if j.MergeInfoMetrics {
		sb.GroupBy(infoBaseSampleKeyFrags(j)...)
	}
	e.emitSelect(sb)
	return nil
}

// infoBaseSampleKeyFrags returns the four base-side columns that identify
// one output sample. Grouping the join result on them collapses the
// per-info-metric fan-out back to one row per base sample; the base side
// is per-series-latest (or per-step) by construction, so the group is
// exactly the join's own fan-out and never merges distinct base samples.
func infoBaseSampleKeyFrags(j *chplan.InfoJoin) []Frag {
	return []Frag{
		qualColFrag("L", j.MetricNameColumn),
		qualColFrag("L", j.AttributesColumn),
		qualColFrag("L", j.TimestampColumn),
		qualColFrag("L", j.ValueColumn),
	}
}

func (e *emitter) validateInfoJoinCols(j *chplan.InfoJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: InfoJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: InfoJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: InfoJoin.TimestampColumn unset", ErrUnsupported)
	case j.ValueColumn == "":
		return fmt.Errorf("%w: InfoJoin.ValueColumn unset", ErrUnsupported)
	}
	return nil
}

// infoJoinPredicateFrag renders the LEFT JOIN ON clause: an AND of
// `L.Attributes[<id>] = R.Attributes[<id>]` over every identity label.
func infoJoinPredicateFrag(j *chplan.InfoJoin) Frag {
	parts := make([]Frag, 0, len(j.IdentityLabels))
	for _, id := range j.IdentityLabels {
		parts = append(parts, Eq(
			Subscript(qualColFrag("L", j.AttributesColumn), Lit(id)),
			Subscript(qualColFrag("R", j.AttributesColumn), Lit(id)),
		))
	}
	return And(parts...)
}

// infoOutputAttributesFrag renders `mapConcat(<info extras>, L.Attributes)`.
// The info-extras map is the info side's Attributes with the identity
// labels + `__name__` stripped (default case) or narrowed to exactly the
// DataLabels names (the second-arg label-matcher case). `mapConcat` is
// later-key-wins, so listing L.Attributes second keeps the base side's
// value on any conflicting key — matching the reference engine's rule of
// skipping info labels already present on the base.
//
// Under MergeInfoMetrics the extras map is first folded across the base
// sample's join rows (one per matched info metric) so the union of every
// matched info metric's data labels lands on one output sample.
func infoOutputAttributesFrag(j *chplan.InfoJoin) Frag {
	extras := infoExtrasFrag(j)
	if j.MergeInfoMetrics {
		extras = infoMergedExtrasFrag(extras)
	}
	return Call(
		"mapConcat",
		extras,
		qualColFrag("L", j.AttributesColumn),
	)
}

// infoMergedExtrasFrag folds a per-join-row extras map into one map per
// group: `mapFromArrays(groupArrayArray(mapKeys(x)), groupArrayArray(mapValues(x)))`
// concatenates every row's key array and value array position-wise, so
// the result carries every matched info metric's data labels. An
// unmatched LEFT JOIN row contributes the default empty map, which
// concatenates as nothing.
func infoMergedExtrasFrag(extras Frag) Frag {
	return Call(
		"mapFromArrays",
		Call("groupArrayArray", Call("mapKeys", extras)),
		Call("groupArrayArray", Call("mapValues", extras)),
	)
}

// infoExtrasFrag renders the info side's contributing labels as a
// `mapFilter((k, v) -> <keep>, R.Attributes)`.
//
//   - Default (DataLabels empty): keep every key that is NOT an identity
//     label and NOT `__name__` → `NOT (k IN ('instance','job','__name__'))`.
//   - DataLabels set: keep only the listed names → `k IN (<names>)`. The
//     identity labels and `__name__` are excluded from DataLabels by the
//     lowering, so no extra NOT-clause is needed here.
func infoExtrasFrag(j *chplan.InfoJoin) Frag {
	attrs := qualColFrag("R", j.AttributesColumn)
	if len(j.DataLabels) == 0 {
		excluded := make([]Frag, 0, len(j.IdentityLabels)+1)
		for _, id := range j.IdentityLabels {
			excluded = append(excluded, Lit(id))
		}
		excluded = append(excluded, Lit(infoMetricNameLabel))
		keep := Not(Paren(In(BareIdent("k"), excluded...)))
		return Call("mapFilter", Lambda2("k", "v", keep), attrs)
	}
	wanted := make([]Frag, 0, len(j.DataLabels))
	for _, name := range j.DataLabels {
		wanted = append(wanted, Lit(name))
	}
	keep := In(BareIdent("k"), wanted...)
	return Call("mapFilter", Lambda2("k", "v", keep), attrs)
}
