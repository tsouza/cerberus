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
// series. `<info extras>` is the info Attributes with the identity labels
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

	sb := NewQuery().
		Select(
			As(qualColFrag("L", j.MetricNameColumn), j.MetricNameColumn),
			As(infoOutputAttributesFrag(j), j.AttributesColumn),
			As(qualColFrag("L", j.TimestampColumn), j.TimestampColumn),
			As(qualColFrag("L", j.ValueColumn), j.ValueColumn),
		).
		From(aliasedFrag(leftFrag, "L")).
		Join(
			LeftJoin,
			aliasedFrag(rightFrag, "R"),
			infoJoinPredicateFrag(j),
		)
	e.emitSelect(sb)
	return nil
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
func infoOutputAttributesFrag(j *chplan.InfoJoin) Frag {
	return Call(
		"mapConcat",
		infoExtrasFrag(j),
		qualColFrag("L", j.AttributesColumn),
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
