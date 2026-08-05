package chsql

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// infoMetricNameLabel is the reserved label carrying a series' metric
// name in the Attributes-keyed label space. The info-enrichment join must
// never copy it from the info series onto the base series — the base
// series keeps its own `__name__` (PromQL's info() preserves the base
// sample's identity and only grows its data labels).
const infoMetricNameLabel = "__name__"

// chExceptionPrefix is what ClickHouse puts in front of a `throwIf`
// message when it surfaces the abort to the client:
// `Code: 395. DB::Exception: <message>: while executing …`.
const chExceptionPrefix = "DB::Exception: "

// IsInfoConflictingLabelError reports whether err is the deliberate query
// abort infoConflictGuardFrag plants, rather than a backend failure.
//
// It exists so the HTTP layer can classify the abort the way the
// reference engine classifies its own: a 422 execution error naming the
// query as at fault, not a 5xx blaming the backend. The prefix is matched
// alongside the message so a query that merely mentions the phrase in a
// label value cannot be mistaken for one.
func IsInfoConflictingLabelError(err error) bool {
	return err != nil && strings.Contains(err.Error(), chExceptionPrefix+chplan.InfoConflictingLabelMessage)
}

// infoExtrasKeyElement / infoExtrasValueElement are the 1-based tuple
// positions of the key and the value in the (key, value) pairs the extras
// fold carries.
const (
	infoExtrasKeyElement   = int64(1)
	infoExtrasValueElement = int64(2)
)

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
// one (see infoMergedExtrasFrag). Two matched info metrics disagreeing on
// a data label fail the query there, as they do upstream — see
// infoConflictGuardFrag.
//
// `<info extras>` is the info Attributes with the identity labels
// + `__name__` stripped (and, when DataLabels is set, narrowed to exactly
// those names), minus the labels the base series already carries and any
// empty-valued ones — see infoExtrasFrag.
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
		sb.Having(infoConflictGuardFrag(j))
	}
	e.emitSelect(sb)
	return nil
}

// infoConflictGuardFrag renders the HAVING term that fails the query when
// one base sample's matched info metrics disagree on a data label.
//
// The test is a cardinality comparison over the folded (key, value)
// pairs: every distinct pair against every distinct KEY. They are equal
// exactly when each key carries one value; a key contributed twice with
// different values adds a pair without adding a key, so the pair count
// runs ahead. Two info metrics contributing the same key with the same
// value collapse in both counts and are not a conflict — matching the
// reference engine, which only fails on a differing value.
//
// It rides in HAVING rather than the SELECT list because the emitter's
// output shape is exactly four columns; `throwIf` returns 0 when it does
// not fire, so the `= 0` keeps every surviving group.
func infoConflictGuardFrag(j *chplan.InfoJoin) Frag {
	pairs := infoExtrasPairsFrag(j)
	distinctPairs := Call("length", Call("arrayDistinct", pairs))
	distinctKeys := Call("length", Call("arrayDistinct", infoPairElementFrag(pairs, infoExtrasKeyElement)))
	return Eq(
		Call("throwIf", Neq(distinctPairs, distinctKeys), InlineLit(chplan.InfoConflictingLabelMessage)),
		InlineLit(int64(0)),
	)
}

// infoExtrasPairsFrag folds one base sample's per-info-metric extras maps
// into a single array of (key, value) tuples.
//
// The zip happens BEFORE the aggregation so a key can never be paired
// with another metric's value: folding `mapKeys` and `mapValues` as two
// independent aggregates would leave the pairing resting on both seeing
// the same row order, which ClickHouse does not promise.
//
// An unmatched LEFT JOIN row contributes the default empty map, which
// zips to nothing.
func infoExtrasPairsFrag(j *chplan.InfoJoin) Frag {
	extras := infoExtrasFrag(j)
	return Call("groupArrayArray", Call("arrayZip", Call("mapKeys", extras), Call("mapValues", extras)))
}

// infoPairElementFrag projects one element out of every (key, value)
// tuple in pairs.
func infoPairElementFrag(pairs Frag, element int64) Frag {
	return Call(
		"arrayMap",
		Lambda1("t", Call("tupleElement", BareIdent("t"), InlineLit(element))),
		pairs,
	)
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
		extras = infoMergedExtrasFrag(j)
	}
	return Call(
		"mapConcat",
		extras,
		qualColFrag("L", j.AttributesColumn),
	)
}

// infoMergedExtrasFrag folds a base sample's per-info-metric extras maps
// into one map carrying the union of their data labels.
//
// The pairs are de-duplicated before the map is rebuilt so two info
// metrics contributing the same label with the same value yield one map
// entry rather than a Map with a repeated key. Whatever pairs survive
// de-duplication have distinct keys, because infoConflictGuardFrag has
// already failed the query otherwise.
func infoMergedExtrasFrag(j *chplan.InfoJoin) Frag {
	pairs := Call("arrayDistinct", infoExtrasPairsFrag(j))
	return Call(
		"mapFromArrays",
		infoPairElementFrag(pairs, infoExtrasKeyElement),
		infoPairElementFrag(pairs, infoExtrasValueElement),
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
//
// Two further exclusions apply to both cases, and mirror the reference
// engine's per-label loop (promql/info.go::combineWithInfoVector):
//
//   - A label the BASE series already carries is skipped. Upstream skips
//     it before recording it, which is why a base-carried label never
//     takes part in the conflict test either. Dropping it here also makes
//     the base win structurally rather than by mapConcat's later-key-wins
//     ordering.
//   - An empty-valued label is skipped: an empty label value is how a
//     ClickHouse Map spells "absent", and upstream's conflict test
//     likewise treats an empty accumulated value as nothing recorded.
func infoExtrasFrag(j *chplan.InfoJoin) Frag {
	attrs := qualColFrag("R", j.AttributesColumn)
	var selected Frag
	if len(j.DataLabels) == 0 {
		excluded := make([]Frag, 0, len(j.IdentityLabels)+1)
		for _, id := range j.IdentityLabels {
			excluded = append(excluded, Lit(id))
		}
		excluded = append(excluded, Lit(infoMetricNameLabel))
		selected = Not(Paren(In(BareIdent("k"), excluded...)))
	} else {
		wanted := make([]Frag, 0, len(j.DataLabels))
		for _, name := range j.DataLabels {
			wanted = append(wanted, Lit(name))
		}
		selected = In(BareIdent("k"), wanted...)
	}
	keep := And(
		selected,
		Neq(BareIdent("v"), Lit("")),
		Not(Call("mapContains", qualColFrag("L", j.AttributesColumn), BareIdent("k"))),
	)
	return Call("mapFilter", Lambda2("k", "v", keep), attrs)
}
