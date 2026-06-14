package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitInfoJoin renders the PromQL experimental `info()` function: each
// Base series is enriched with the data labels of the matching Info
// series (`target_info` by default), joined on the identifying labels
// (`instance` + `job`). The Base sample's Value / TimeUnix / MetricName
// flow through unchanged — info() only adds labels.
//
// Shape:
//
//	SELECT B.MetricName, mapConcat(R._info_data, B.Attributes) AS Attributes,
//	       B.TimeUnix, B.Value
//	FROM (<base subtree>) AS B
//	LEFT JOIN (
//	  SELECT mapFilter((k,v)->k IN (identifying…), Attributes) AS _info_sig,
//	         argMax(<data-label map>, TimeUnix)               AS _info_data
//	  FROM (<info subtree>)
//	  GROUP BY _info_sig
//	) AS R
//	ON mapFilter((k,v)->k IN (identifying…), B.Attributes) = R._info_sig
//
// Why a LEFT JOIN: the reference returns the Base series UNENRICHED when
// no info series matches it (for the bare `info(v)` / empty-data-label-
// filter shape). A LEFT JOIN keeps every Base row; the unmatched
// `R._info_data` defaults to an empty Map(String,String) (CH fills a
// non-Nullable join column with its type default), so
// `mapConcat(emptyMap, B.Attributes)` collapses to the Base attributes.
//
// Conflict resolution (base wins): `mapConcat` is later-key-wins in CH,
// so placing the Base attributes LAST overwrites any info data label that
// collides with a base label — matching evalInfo's "skip labels already
// on the base metric" rule.
//
// The per-signature info aggregation picks the newest info row via
// argMax(..., TimeUnix), mirroring evalInfo's "keep the newer info
// sample" tie-break for multiple info series sharing one signature.
func (e *emitter) emitInfoJoin(j *chplan.InfoJoin) error {
	if err := e.validateInfoJoinCols(j); err != nil {
		return err
	}

	baseFrag, err := e.subqueryFrag(j.Base)
	if err != nil {
		return err
	}
	infoSideFrag, err := e.infoJoinInfoSideFrag(j)
	if err != nil {
		return err
	}

	sig := infoSignatureFrag(j.IdentifyingLabels, qualColFrag("B", j.AttributesColumn))
	enriched := Call(
		"mapConcat",
		qualColFrag("R", infoDataAlias),
		qualColFrag("B", j.AttributesColumn),
	)

	sb := NewQuery().
		Select(
			As(qualColFrag("B", j.MetricNameColumn), j.MetricNameColumn),
			As(enriched, j.AttributesColumn),
			As(qualColFrag("B", j.TimestampColumn), j.TimestampColumn),
			As(qualColFrag("B", j.ValueColumn), j.ValueColumn),
		).
		From(aliasedFrag(baseFrag, "B")).
		Join(
			LeftJoin,
			aliasedFrag(infoSideFrag, "R"),
			Eq(sig, qualColFrag("R", infoSigAlias)),
		)
	e.emitSelect(sb)
	return nil
}

// infoSigAlias / infoDataAlias are the emitter-internal column names of
// the per-signature info-side aggregation. They use a `_info_` prefix so
// they never collide with a real Attributes label name.
const (
	infoSigAlias  = "_info_sig"
	infoDataAlias = "_info_data"
)

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
	case len(j.IdentifyingLabels) == 0:
		return fmt.Errorf("%w: InfoJoin.IdentifyingLabels empty", ErrUnsupported)
	}
	return nil
}

// infoJoinInfoSideFrag renders the info side as a parenthesised SELECT
// that collapses every info series to one row per identifying-label
// signature, carrying the data-label map (newest-wins via argMax).
func (e *emitter) infoJoinInfoSideFrag(j *chplan.InfoJoin) (Frag, error) {
	sub, err := e.subqueryFrag(j.Info)
	if err != nil {
		return nil, err
	}
	sig := infoSignatureFrag(j.IdentifyingLabels, Col(j.AttributesColumn))
	dataMap := infoDataLabelsFrag(j, Col(j.AttributesColumn))

	inner := NewQuery().
		Select(
			As(sig, infoSigAlias),
			As(Call("argMax", dataMap, Col(j.TimestampColumn)), infoDataAlias),
		).
		From(sub).
		GroupBy(BareIdent(infoSigAlias))
	return inner.Frag(), nil
}

// infoSignatureFrag builds the join-key signature:
//
//	mapFilter((k, v) -> k IN (id1, id2, …), <attrs>)
//
// keeping only the identifying labels. Identifying labels absent from a
// series simply don't appear in its filtered map, so a base series
// missing `instance` joins on `job` alone — matching evalInfo, which
// builds the signature only from non-empty identifying values.
func infoSignatureFrag(identifying []string, attrs Frag) Frag {
	keys := make([]Frag, len(identifying))
	for i, l := range identifying {
		keys[i] = Lit(l)
	}
	return Call(
		"mapFilter",
		Lambda2("k", "v", In(BareIdent("k"), keys...)),
		attrs,
	)
}

// infoDataLabelsFrag builds the info series' data-label map: every label
// that is NOT an identifying label and NOT `__name__`, optionally further
// restricted to DataLabelFilter (the non-`__name__` matchers of the
// optional `{…}` selector).
//
//	mapFilter((k, v) -> NOT (k IN (identifying…, '__name__'))
//	                    [ AND k IN (dataFilter…) ], <attrs>)
func infoDataLabelsFrag(j *chplan.InfoJoin, attrs Frag) Frag {
	excluded := make([]Frag, 0, len(j.IdentifyingLabels)+1)
	for _, l := range j.IdentifyingLabels {
		excluded = append(excluded, Lit(l))
	}
	// `__name__` is never a data label — it selects which info metric to
	// consider, not a value to copy onto the base series.
	excluded = append(excluded, Lit(metricNameLabel))

	pred := Not(Paren(In(BareIdent("k"), excluded...)))
	if len(j.DataLabelFilter) > 0 {
		keep := make([]Frag, len(j.DataLabelFilter))
		for i, l := range j.DataLabelFilter {
			keep[i] = Lit(l)
		}
		pred = And(pred, In(BareIdent("k"), keep...))
	}
	return Call("mapFilter", Lambda2("k", "v", pred), attrs)
}

// metricNameLabel is the reserved PromQL series-name label. It is never
// copied onto a base series by info() — it identifies the info metric,
// it is not a data label.
const metricNameLabel = "__name__"
