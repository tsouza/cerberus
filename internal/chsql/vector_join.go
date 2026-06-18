package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitVectorJoin renders a PromQL vector-vector binary expression as an
// INNER JOIN of per-series latest samples. The shape depends on the
// `Card` modifier:
//
//   - CardOneToOne (default): both sides aggregate to one row per
//     matching key, with a `throwIf(uniqExact(Attributes) > 1, ...)`
//     side-effect column that fails the query at execution time when
//     the matching key collapses multiple distinct series ("many-to-
//     many matching not allowed: matching labels must be unique on one
//     side"). Output Attributes is the left side's label set reduced to
//     the matching labels (on()/ignoring()) per Prometheus resultMetric
//     — see outputMatchSetFrag. With default matching (no on/ignoring)
//     that reduction is a no-op so the full LHS label set survives.
//
//   - CardManyToOne (`group_left(<labels>)`): left side keeps
//     per-series granularity (the "many"); right side aggregates per
//     matching key with the same uniqueness throwIf guard (the "one").
//     Output Attributes = left.Attributes merged with right's Include
//     labels overlaid via `mapConcat` — CH's later-key-wins map merge.
//
//   - CardOneToMany (`group_right(<labels>)`): mirror of CardManyToOne.
//
// The outer SELECT, each per-side aggregation subquery, and the INNER
// JOIN slot all flow through typed QueryBuilder slots. The bare-alias
// glue (`AS L` / `AS R`) is an emitter-chosen synthetic token spliced
// via verbatim inside aliasedFrag — CH accepts unquoted single-letter
// aliases and the existing fixtures pin that shape.
func (e *emitter) emitVectorJoin(j *chplan.VectorJoin) error {
	if err := e.validateVectorJoinCols(j); err != nil {
		return err
	}

	leftRole, rightRole := vectorJoinRoles(j)
	outerSide := "L"
	if j.Card == chplan.CardOneToMany {
		// group_right — the "many" side is R; the output's
		// representative MetricName + TimeUnix come from there.
		outerSide = "R"
	}

	leftFrag, err := e.vectorJoinSideFrag(j, j.Left, leftRole)
	if err != nil {
		return err
	}
	rightFrag, err := e.vectorJoinSideFrag(j, j.Right, rightRole)
	if err != nil {
		return err
	}

	sb := NewQuery().
		Select(
			outputMetricNameFrag(j, outerSide),
			outputAttributesFrag(j),
			// Aliased for the same reason as the Attributes slot in
			// writeOutputAttributes: an unaliased `R.TimeUnix`
			// projection keeps the `R.` qualifier in its output
			// column name, breaking every wrapping SELECT for
			// group_right (outerSide == "R").
			As(qualColFrag(outerSide, j.TimestampColumn), j.TimestampColumn),
			vectorJoinValueExprFrag(j),
		).
		From(aliasedFrag(leftFrag, "L")).
		Join(
			InnerJoin,
			aliasedFrag(rightFrag, "R"),
			vectorMatchPredicateFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned),
		)
	// Bare V-V comparison (no `bool` modifier): PromQL's
	// "preserve LHS where comparison holds" rule applies. The Value
	// expression (set by vectorJoinValueExprFrag) already projects
	// `L.Value` for this case, so add the comparison as a WHERE filter
	// so rows where the comparison is false are dropped. Without the
	// filter the join keeps every matched pair, producing rows whose
	// Value is the LHS even when the comparison would have dropped
	// them in Prometheus.
	//
	// Without `bool` the emit would otherwise project a UInt8 column
	// (CH's `Float64 <cmp> Float64` return type) into Value, which
	// clickhouse-go cannot scan into `*float64` — surfacing as a 502
	// at the compat lane for every plain V-V comparison with `on(...)`.
	if isComparisonOp(j.Op) && !j.ReturnBool {
		sb.Where(vectorJoinCompareFilterFrag(j))
	}
	e.emitSelect(sb)
	return nil
}

func (e *emitter) validateVectorJoinCols(j *chplan.VectorJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: VectorJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: VectorJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: VectorJoin.TimestampColumn unset", ErrUnsupported)
	case j.ValueColumn == "":
		return fmt.Errorf("%w: VectorJoin.ValueColumn unset", ErrUnsupported)
	}
	return nil
}

// sideRole describes the per-side aggregation shape: "many" keeps
// per-series granularity, "one" collapses to one row per matching key
// with a runtime uniqueness guard.
type sideRole int

const (
	// roleMany keeps per-series granularity: argMax over
	// (MetricName, Attributes). Used for the "many" side of
	// group_left/right, and for both sides of one-to-one when the
	// matching key is the full Attributes map (uniqueness is then
	// guaranteed by construction).
	roleMany sideRole = iota
	// roleOne collapses to one row per matching key with a runtime
	// throwIf(uniqExact(Attributes) > 1, ...) guard. Used for the
	// "one" side of group_left/right and for both sides of
	// one-to-one when matching is on a subset of labels
	// (on(...) / ignoring(...)) where many-to-many ambiguity is
	// possible.
	roleOne
)

// vectorJoinRoles resolves the per-side aggregation roles for the join.
//
//   - CardManyToOne (`group_left`)   → L is many, R is one.
//   - CardOneToMany (`group_right`)  → R is many, L is one.
//   - CardOneToOne with subset match → both sides are "one"
//     (uniqueness must be enforced at runtime).
//   - CardOneToOne with full-Attributes match → both sides are
//     "many"; the per-series aggregation already guarantees one row
//     per matching key.
func vectorJoinRoles(j *chplan.VectorJoin) (sideRole, sideRole) {
	switch j.Card {
	case chplan.CardManyToOne:
		return roleMany, roleOne
	case chplan.CardOneToMany:
		return roleOne, roleMany
	}
	if len(j.Match.Labels) == 0 && !j.Match.On {
		return roleMany, roleMany
	}
	return roleOne, roleOne
}

// vectorJoinSideFrag renders one side of the join as a Frag that emits
// a parenthesised SELECT subquery. roleMany keeps per-series
// granularity (one row per `(MetricName, Attributes)` group). roleOne
// collapses to one row per matching key with a
// `throwIf(uniqExact(Attributes) > 1, ...)` side-effect column — the
// "many-to-many matching not allowed" Prometheus error surfaces at CH
// query-execution time rather than as a silent cross-product.
//
// The per-side aggregation projects its outputs through `_join_*`
// aliases (`_join_MetricName`, `_join_Attributes`, `_join_TimeUnix`,
// `_join_Value`) then an outer subquery renames them back to the
// canonical names. Reason: when the input is already an LWR'd Sample-
// shaped subquery (the post-PR #275 default for instant
// VectorSelectors), CH's analyzer otherwise traces the alias chain
// `TimeUnix → lwr_ts → max(TimeUnix)` through the subquery boundary
// and rejects the per-side aggregation with ILLEGAL_AGGREGATION
// ("max(TimeUnix) AS TimeUnix is found inside another aggregate
// function"). Renaming the per-side aggregates breaks the chain — CH
// sees the inner subquery as having columns named `_join_*` not
// `TimeUnix` / `Value`, so the agg-inside-agg detector doesn't
// misfire. The outer Project renames back so the JOIN's ON clause
// and outer SELECT continue to reference `L.Attributes` / `R.Value`
// naturally.
func (e *emitter) vectorJoinSideFrag(j *chplan.VectorJoin, n chplan.Node, role sideRole) (Frag, error) {
	sub, err := e.subqueryFrag(n)
	if err != nil {
		return nil, err
	}
	// Range-vector operands (instant rate/irate/increase/delta and
	// aggregations over them) emit a "derived" shape — [group-keys...,
	// Value] — that carries no TimeUnix column, exactly as it carries no
	// MetricName (see joinMetricNameFrag). When such an operand feeds an
	// instant-mode join, the per-side aggregation must NOT read the
	// operand's TimeUnix: max(TimeUnix) / argMax(Value, TimeUnix) would
	// reference a column that does not materialise and ClickHouse fails
	// with code 47 "Unknown expression identifier 'TimeUnix'". Synthesize
	// the join-side timestamp the same way the top-level instant-sample
	// projection does (handler.go wrapWithSampleProjection ->
	// synthesizedAnchor), and collapse the already-unique per-series row
	// with any(Value). StepAligned (range) joins always run over a
	// matrix-shape operand that surfaces anchor_ts AS TimeUnix, so they
	// keep the real-timestamp path.
	derived := !j.StepAligned && !vectorJoinOperandCarriesTimestamp(n, j.TimestampColumn)
	inner := NewQuery().From(sub)
	if role == roleMany {
		// Step-aligned ("range mode") joins keep TimeUnix in the
		// per-side group key so the per-(series, anchor) row survives
		// instead of being collapsed to one row per series. The
		// selector projects TimeUnix directly (it's a group key,
		// already unique within the group). Instant mode leaves
		// TimeUnix off the GROUP BY and uses max(TimeUnix) to pick
		// the latest LWR sample — byte-stable for the existing
		// fixtures.
		switch {
		case j.StepAligned:
			inner.Select(
				joinMetricNameFrag(j),
				As(Col(j.AttributesColumn), joinAlias(j.AttributesColumn)),
				As(Col(j.TimestampColumn), joinAlias(j.TimestampColumn)),
				argMaxAs(j.ValueColumn, j.TimestampColumn, joinAlias(j.ValueColumn)),
			).GroupBy(
				Col(j.AttributesColumn),
				Col(j.TimestampColumn),
			)
		case derived:
			inner.Select(
				joinMetricNameFrag(j),
				As(Col(j.AttributesColumn), joinAlias(j.AttributesColumn)),
				joinTimestampFrag(j),
				aggAnyAs(j.ValueColumn, joinAlias(j.ValueColumn)),
			).GroupBy(
				Col(j.AttributesColumn),
			)
		default:
			inner.Select(
				joinMetricNameFrag(j),
				As(Col(j.AttributesColumn), joinAlias(j.AttributesColumn)),
				aggMaxAs(j.TimestampColumn, joinAlias(j.TimestampColumn)),
				argMaxAs(j.ValueColumn, j.TimestampColumn, joinAlias(j.ValueColumn)),
			).GroupBy(
				Col(j.AttributesColumn),
			)
		}
	} else {
		// "one" side — aggregate by matching key with a runtime
		// uniqueness guard. argMax(Attributes, TimeUnix) picks one
		// representative series per match key; throwIf fires when
		// there's more than one.
		//
		// Step-aligned joins extend the match key with TimeUnix so
		// each (match-key, anchor) gets its own row and the throwIf
		// uniqueness guard fires per-anchor rather than across the
		// full per-step matrix. The selector keeps argMax /
		// max(TimeUnix) for symmetry with instant mode — within a
		// single (match-key, anchor) group all rows share the same
		// TimeUnix by construction.
		groupFrags := []Frag{matchKeyGroupExprFrag(j.Match, j.AttributesColumn)}
		if j.StepAligned {
			groupFrags = append(groupFrags, Col(j.TimestampColumn))
		}
		if derived {
			// Range-vector operand: no TimeUnix to argMax by. The
			// instant operand is already one row per series, so any()
			// picks the representative Attributes/Value and the
			// timestamp is synthesized (see the roleMany note above).
			inner.Select(
				joinMetricNameFrag(j),
				aggAnyAs(j.AttributesColumn, joinAlias(j.AttributesColumn)),
				joinTimestampFrag(j),
				aggAnyAs(j.ValueColumn, joinAlias(j.ValueColumn)),
				matchCheckFrag(j.AttributesColumn),
			).GroupBy(groupFrags...)
		} else {
			inner.Select(
				joinMetricNameFrag(j),
				argMaxAs(j.AttributesColumn, j.TimestampColumn, joinAlias(j.AttributesColumn)),
				aggMaxAs(j.TimestampColumn, joinAlias(j.TimestampColumn)),
				argMaxAs(j.ValueColumn, j.TimestampColumn, joinAlias(j.ValueColumn)),
				matchCheckFrag(j.AttributesColumn),
			).GroupBy(groupFrags...)
		}
	}

	// Outer Project: rename `_join_*` back to canonical column names
	// so the JOIN's ON clause and the outer SELECT reference
	// `L.Attributes` / `R.Value` / etc. directly. The throwIf
	// side-effect column from roleOne is dropped here — CH still
	// evaluates it as part of the inner aggregation, but the join's
	// ON / projection don't need it.
	outer := NewQuery().
		Select(
			As(Col(joinAlias(j.MetricNameColumn)), j.MetricNameColumn),
			As(Col(joinAlias(j.AttributesColumn)), j.AttributesColumn),
			As(Col(joinAlias(j.TimestampColumn)), j.TimestampColumn),
			As(Col(joinAlias(j.ValueColumn)), j.ValueColumn),
		).
		From(inner.Frag())
	return outer.Frag(), nil
}

// joinAlias returns the per-side aggregation's internal alias for a
// canonical Sample column. Using a `_join_` prefix keeps the alias
// distinct from any input column name, which is what breaks CH's
// alias-chain trace through the inner aggregation subquery (see the
// vectorJoinSideFrag docstring).
func joinAlias(col string) string {
	return "_join_" + col
}

// joinMetricNameFrag projects a constant empty string into the per-side
// `_join_MetricName` slot instead of reading the operand's MetricName
// column. A vector-vector binary op always drops `__name__` from its
// output (vectorJoinDropsName) and the join key / label matching derive
// solely from Attributes (PromQL vector matching excludes `__name__`), so
// the side never consumes the operand's MetricName value. Reading
// `Col(MetricName)` here was invalid for range-vector operands
// (rate/irate/increase/delta), whose RangeWindow subquery projects only
// [Attributes, anchor_ts, TimeUnix, Value] and carries no MetricName
// column -- producing ClickHouse code 47 "Unknown expression identifier
// '_join_MetricName'". A literal keeps the codegen valid for every
// operand shape; the outer rename (`_join_MetricName AS MetricName`) and
// the byte value match outputMetricNameFrag's existing `” AS MetricName`.
func joinMetricNameFrag(j *chplan.VectorJoin) Frag {
	return As(Lit(""), joinAlias(j.MetricNameColumn))
}

// joinTimestampFrag projects a synthesized instant anchor into the
// per-side `_join_TimeUnix` slot for range-vector operands that carry no
// real timestamp column (instant rate/irate/increase/delta and
// aggregations over them). The byte shape `now64(9) -
// toIntervalNanosecond(5000000000)` matches chplan.NowNanoMinusStaleness
// — the same anchor the top-level instant-sample projection
// (api/prom/handler.go synthesizedAnchor) stamps on these derived-shape
// rows — so a V-V binop over rate() carries the identical timestamp it
// would have carried unjoined. It is a scalar constant, so it is
// projected directly rather than wrapped in an aggregate (the per-side
// GROUP BY is Attributes-only and a constant needs no aggregation).
func joinTimestampFrag(j *chplan.VectorJoin) Frag {
	return As(
		Sub(Call("now64", InlineLit(int64(chplan.NanoScale))),
			Call("toIntervalNanosecond", InlineLit(stalenessLookbackNanos))),
		joinAlias(j.TimestampColumn),
	)
}

// stalenessLookbackNanos mirrors chplan's instant-anchor staleness
// lookback (5s in nanoseconds): the synthesized join-side timestamp
// lands 5 seconds before the server clock, matching the top-level
// synthesizedAnchor so a joined rate() row stamps the same instant it
// would unjoined.
const stalenessLookbackNanos = int64(5_000_000_000)

// aggMaxAs returns a Frag for `max(<col>) AS <alias>`.
func aggMaxAs(col, alias string) Frag {
	return As(Call("max", Col(col)), alias)
}

// aggAnyAs returns a Frag for `any(<col>) AS <alias>`. Used on the
// derived-shape (range-vector) join side where the operand is already
// one row per series, so any() picks that single representative without
// needing a TimeUnix to argMax by.
func aggAnyAs(col, alias string) Frag {
	return As(Call("any", Col(col)), alias)
}

// vectorJoinOperandCarriesTimestamp reports whether the join operand n
// projects a real per-row timestamp column named tsCol. It mirrors the
// inverse of api/prom/handler.go isDerivedShape: RangeWindow /
// RangeWindowNative / Aggregate roots emit a [group-keys..., Value]
// derived shape that carries no TimeUnix, while an LWR-style Project
// that names the canonical timestamp output (or any other node) does.
// A Project is canonical-carrying only when one of its projections
// outputs tsCol; otherwise the value-rewrite Project shape (e.g. abs /
// clamp over a RangeWindow) stays derived. Filter / canonical Project
// are transparent and recurse into their input.
func vectorJoinOperandCarriesTimestamp(n chplan.Node, tsCol string) bool {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		// Matrix-shape (OuterRange > 0) surfaces anchor_ts AS TimeUnix;
		// instant-shape carries only [group-keys..., Value].
		return v.OuterRange > 0
	case *chplan.RangeWindowNative:
		// Always matrix-shape: explodes the grid and surfaces a per-row
		// anchor_ts under the timestamp column.
		return true
	case *chplan.Aggregate:
		// sum/avg/... by(...) over a range vector projects only the
		// group keys + aggregated Value; no per-row timestamp survives.
		return false
	case *chplan.Filter:
		return vectorJoinOperandCarriesTimestamp(v.Input, tsCol)
	case *chplan.Project:
		for _, p := range v.Projections {
			if projectionOutputsColumn(p, tsCol) {
				return true
			}
		}
		return vectorJoinOperandCarriesTimestamp(v.Input, tsCol)
	}
	// Scan, LWR, and every other canonical-shape node carry TimeUnix.
	return true
}

// projectionOutputsColumn reports whether projection p exposes an output
// column named col — either via an explicit Alias or a bare ColumnRef to
// col with no rewrite. Mirrors api/prom/handler.go projectionOutputName.
func projectionOutputsColumn(p chplan.Projection, col string) bool {
	if p.Alias != "" {
		return p.Alias == col
	}
	if cr, ok := p.Expr.(*chplan.ColumnRef); ok {
		return cr.Name == col
	}
	return false
}

// argMaxAs returns a Frag for `argMax(<valCol>, <byCol>) AS <alias>`.
func argMaxAs(valCol, byCol, alias string) Frag {
	return As(Call("argMax", Col(valCol), Col(byCol)), alias)
}

// matchCheckFrag returns a Frag for the runtime uniqueness guard:
//
//	throwIf(uniqExact(<attrsCol>) > 1, ?) AS _cerberus_match_check
//
// The error message is bound as a positional `?` argument. The alias
// `_cerberus_match_check` is rendered bare (no backticks) — the
// fixtures pin that shape; CH accepts unquoted aliases for ASCII
// underscore-prefixed names.
func matchCheckFrag(attrsCol string) Frag {
	check := Call(
		"throwIf",
		Gt(Call("uniqExact", Col(attrsCol)), InlineLit(1)),
		Lit("many-to-many matching not allowed: matching labels must be unique on one side"),
	)
	// `_cerberus_match_check` is an emitter-pinned bare alias (no
	// backticks); the AS suffix rides verbatim, not the quoting As Frag.
	return func(b *Builder) {
		check(b)
		verbatim(" AS _cerberus_match_check")(b)
	}
}

// matchKeyGroupExprFrag returns a Frag for the GROUP BY expression
// that collapses rows onto a single matching key. For default matching
// (full Attributes) this is just the Attributes column; for on(labels)
// it's `mapFilter((k, v) -> k IN (...), Attributes)`; for
// ignoring(labels) it's the complementary mapFilter.
func matchKeyGroupExprFrag(m chplan.VectorMatch, attrsCol string) Frag {
	if len(m.Labels) == 0 && !m.On {
		return Col(attrsCol)
	}
	if m.On && len(m.Labels) == 0 {
		// on() with no labels - group everything onto a single
		// match-key. CH doesn't allow an empty IN list, so emit a
		// constant tuple.
		return Call("tuple")
	}
	if m.On {
		// mapFilter((k, v) -> k IN (?, ?, …), Attributes) — keep only
		// the on(...) labels.
		lbls := make([]Frag, len(m.Labels))
		for i, lbl := range m.Labels {
			lbls[i] = Lit(lbl)
		}
		return Call(
			"mapFilter",
			Lambda2("k", "v", In(BareIdent("k"), lbls...)),
			Col(attrsCol),
		)
	}
	// ignoring(...) — the complementary mapFilter. MapFilterExcept is a
	// Builder helper that renders `mapFilter((k, v) -> NOT (k IN (…)), col)`.
	return func(b *Builder) { b.MapFilterExcept(attrsCol, m.Labels...) }
}

// outputMatchSetFrag returns a Frag for the qualified output Attributes
// of a CardOneToOne join, reduced to the matching label set per
// Prometheus's `resultMetric` (promql/engine.go):
//
//	if matching.Card == parser.CardOneToOne {
//	    if matching.On { enh.lb.Keep(matching.MatchingLabels...) }
//	    else           { enh.lb.Del(matching.MatchingLabels...) }
//	}
//
// This reduction is gated only on the cardinality being one-to-one - it
// runs for arithmetic, bool-comparison, AND bare-comparison ops alike.
// The op only governs `__name__` (shouldDropMetricName), handled
// separately by outputMetricNameFrag/vectorJoinDropsName. The shapes:
//
//   - default matching (Labels empty, On false): full Attributes,
//     unchanged - Del() of nothing drops nothing.
//   - on(labels): mapFilter((k, v) -> k IN (...), Attributes) - keep
//     only the on(...) labels (Keep()).
//   - on() with no labels: an empty map (Keep() with no labels) - the
//     constant-false mapFilter preserves the Map(String, String) type
//     while yielding {}.
//   - ignoring(labels): the complementary mapFilter - drop the ignored
//     labels (Del()).
//   - ignoring() with no labels: full Attributes, unchanged - Del() of
//     nothing.
//
// `side` is the bare L / R qualifier the output projection reads from
// (always "L" for one-to-one).
func outputMatchSetFrag(m chplan.VectorMatch, side, attrsCol string) Frag {
	qual := qualColFrag(side, attrsCol)
	if len(m.Labels) == 0 {
		if m.On {
			// on() - Keep() with no labels yields an empty map. A
			// constant-false predicate keeps the Map(String, String)
			// type so wrapping SELECTs still see a map column.
			return Call(
				"mapFilter",
				Lambda2("k", "v", InlineLit(int64(0))),
				qual,
			)
		}
		// default matching or ignoring() - no labels to drop.
		return qual
	}
	if m.On {
		lbls := make([]Frag, len(m.Labels))
		for i, lbl := range m.Labels {
			lbls[i] = Lit(lbl)
		}
		return Call(
			"mapFilter",
			Lambda2("k", "v", In(BareIdent("k"), lbls...)),
			qual,
		)
	}
	// ignoring(labels) - drop the ignored labels via the complementary
	// mapFilter.
	lbls := make([]Frag, len(m.Labels))
	for i, lbl := range m.Labels {
		lbls[i] = Lit(lbl)
	}
	return Call(
		"mapFilter",
		Lambda2("k", "v", Not(Paren(In(BareIdent("k"), lbls...)))),
		qual,
	)
}

// outputAttributesFrag returns a Frag for the output Attributes
// expression. For CardOneToOne the output is the LHS Attributes reduced
// to the matching label set (on()/ignoring()), matching Prometheus's
// `resultMetric` Keep/Del logic — see outputMatchSetFrag. For
// group_left(<labels>) the output merges the named labels from the
// "one" side onto the "many" side's full Attributes via mapConcat (CH's
// later-key-wins map merge); group_right mirrors with roles swapped. The
// CardOneToOne reduction does NOT apply to group_left/right - Prometheus
// skips the Keep/Del block for those cards, keeping the many side's full
// labels (then overlaying Include).
//
// When no Include labels are present (bare `group_left` without an
// explicit label list, which is uncommon but parser-legal), the
// "many" side's Attributes flows through unchanged - this matches
// Prometheus's behaviour where bare group_left/right copies nothing
// beyond the matching key.
func outputAttributesFrag(j *chplan.VectorJoin) Frag {
	attrs := j.AttributesColumn
	manySide := ""
	switch j.Card {
	case chplan.CardManyToOne:
		manySide = "L"
	case chplan.CardOneToMany:
		manySide = "R"
	}
	if manySide == "" {
		// CardOneToOne — output is the LHS Attributes reduced to the
		// matching label set per Prometheus resultMetric. The explicit
		// `AS Attributes` alias re-canonicalises the column name.
		return As(outputMatchSetFrag(j.Match, "L", attrs), attrs)
	}
	if len(j.Include) == 0 {
		// Bare group_left/right (no Include labels) - output is the
		// "many" side's full Attributes (L for ManyToOne, R for
		// OneToMany). The CardOneToOne Keep/Del reduction does NOT
		// apply here (Prometheus skips it for group_x). The explicit
		// `AS Attributes` alias is load-bearing for the R side:
		// ClickHouse keeps the `R.` qualifier in the output column name
		// of an unaliased right-table projection (the left side
		// collapses to the bare column name), so without the alias
		// every wrapping SELECT fails with "Unknown expression
		// identifier 'Attributes'" - the group_right bug the
		// showcase-promql sweep surfaced.
		return As(qualColFrag(manySide, attrs), attrs)
	}

	// group_left/right with Include labels — overlay the "one" side's
	// matching labels onto the "many" side's full Attributes. Use
	// mapConcat: the later argument's keys overwrite the earlier's.
	oneSide := "R"
	if manySide == "R" {
		oneSide = "L"
	}
	includes := make([]Frag, len(j.Include))
	for i, lbl := range j.Include {
		includes[i] = Lit(lbl)
	}
	merged := Call(
		"mapConcat",
		qualColFrag(manySide, attrs),
		Call(
			"mapFilter",
			Lambda2("k", "v", In(BareIdent("k"), includes...)),
			qualColFrag(oneSide, attrs),
		),
	)
	return As(merged, attrs)
}

// outputMetricNameFrag returns a Frag for the joined output's
// MetricName slot. PromQL's V-V binop rule, per Prometheus's reference
// implementation: every V-V binop is a transformation — the output
// sample is derived, not a passthrough of the LHS sample. So
// `__name__` is dropped for every shape:
//
//   - Arithmetic op (any non-comparison): drops `__name__`.
//   - Comparison op with `bool` modifier (`ReturnBool == true`):
//     drops `__name__` — the result is a 1.0/0.0 derived sample.
//   - Bare comparison op: also drops `__name__`. Although LHS labels
//     other than `__name__` survive the comparison-as-filter, Prom
//     still strips the metric name from the output — comparing two
//     time series is a transformation, not a passthrough.
//
// The dropped case emits a parameterised empty string aliased back to
// the MetricName column. See Pool-AU's audit (#355) and Pool-AT's
// #356 — this site accounts for ~18 of the 107 compat-lane
// `__name__`-retention diffs across V-V arithmetic / `bool`-compare /
// bare-compare + group_left variants.
func outputMetricNameFrag(j *chplan.VectorJoin, outerSide string) Frag {
	if vectorJoinDropsName(j) {
		return As(Lit(""), j.MetricNameColumn)
	}
	return qualColFrag(outerSide, j.MetricNameColumn)
}

// vectorJoinDropsName reports whether the V-V binop output drops
// `__name__`. Prometheus drops the metric name for every V-V binop
// — arithmetic, comparison-with-bool, and bare comparison alike,
// because each is a transformation rather than a passthrough.
func vectorJoinDropsName(_ *chplan.VectorJoin) bool {
	return true
}

// vectorJoinValueExprFrag returns a Frag for the joined value
// expression. The default shape is `(L.<val> <op> R.<val>) AS <val>`.
//
// PromQL `^` (exponentiation) is special-cased: in ClickHouse the `^`
// token is bitwise-XOR (integer-only), not exponentiation, so emitting
// it directly against Float64 columns yields a CH ILLEGAL_TYPE_OF_ARGUMENT
// error (HTTP 502 at the compat lane). PromQL semantics call for
// floating-point exponentiation, which CH spells `pow(x, y)` — matching
// the scalar `exprBinary` path's special case for OpPow.
//
// When ReturnBool is set on a comparison op (PromQL `lhs > bool rhs`),
// the binary result is wrapped with `toFloat64(...)` so every matched
// pair emits 1.0 or 0.0 instead of being dropped by the comparison —
// matching Prometheus's `bool` semantics for V-V comparisons.
//
// Bare comparison op (no `bool` modifier): Prom's V-V comparison-as-
// filter rule applies — preserve the LHS sample value where the
// comparison holds and drop rows where it doesn't. emitVectorJoin
// pairs this projection with a `WHERE (L.Value <op> R.Value)` clause
// via vectorJoinCompareFilterFrag; here we project `L.Value` directly
// rather than the comparison expression so the surviving rows carry
// the LHS sample value (Float64) rather than a UInt8 comparison
// result. Without the LHS projection the rendered Value column would
// be `Float64 <cmp> Float64` → UInt8, which clickhouse-go cannot scan
// into `*float64` (the 502 the compat lane surfaces).
func vectorJoinValueExprFrag(j *chplan.VectorJoin) Frag {
	left := qualColFrag("L", j.ValueColumn)
	right := qualColFrag("R", j.ValueColumn)
	if isComparisonOp(j.Op) && !j.ReturnBool {
		// Plain V-V comparison: filter wraps the join via the WHERE
		// clause; the Value column carries the LHS sample value
		// (Float64) to satisfy the scan contract.
		return As(left, j.ValueColumn)
	}
	var inner Frag
	switch j.Op {
	case chplan.OpPow:
		inner = Call("pow", left, right)
	case chplan.OpAtan2:
		// PromQL `l atan2 r` is Go's math.Atan2(l, r); ClickHouse has
		// no infix atan2 spelling, so render the function-call form —
		// same posture as OpPow above and exprBinary's scalar path.
		inner = Call("atan2", left, right)
	default:
		inner = Paren(binOp(string(j.Op), left, right))
	}
	if j.ReturnBool {
		inner = Call("toFloat64", inner)
	}
	return As(inner, j.ValueColumn)
}

// vectorJoinCompareFilterFrag returns the WHERE-clause Frag used for
// bare V-V comparisons (no `bool` modifier). PromQL's semantics keep
// LHS samples where `L.Value <op> R.Value` holds and drop the rest;
// the comparison expression is emitted as a WHERE predicate so CH
// drops the non-matching rows at the join's output level.
//
// PromQL's vector-vector comparison ops are limited to the six listed
// in [isComparisonOp]; OpPow (a special case in the value expression)
// is intentionally not a comparison so this branch never sees it.
func vectorJoinCompareFilterFrag(j *chplan.VectorJoin) Frag {
	left := qualColFrag("L", j.ValueColumn)
	right := qualColFrag("R", j.ValueColumn)
	return Paren(binOp(string(j.Op), left, right))
}

// isComparisonOp reports whether op is one of PromQL's six comparison
// binary ops. Kept local to the emitter so the chsql package doesn't
// take a dep on internal/promql for the BinaryOp predicate.
func isComparisonOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// qualColFrag returns a Frag for `<bareSide>.<col>` — the bare-alias
// L / R qualifier the legacy emitter pins. The side is a synthetic,
// emitter-chosen single-letter alias (never user input) so it rides
// `verbatim` rather than Ident's backtick quoting; the column is a real
// identifier and flows through Col.
func qualColFrag(side, col string) Frag {
	return func(b *Builder) {
		verbatim(side + ".")(b)
		Col(col)(b)
	}
}

// aliasedFrag wraps inner in a trailing ` AS <bareAlias>`. The alias
// is rendered bare (no backticks) — CH accepts unquoted single-letter
// aliases, and the vector_join / structural_join fixtures pin that
// shape. The bare alias is an emitter-chosen synthetic token (L / R /
// _seed / ns / …), so the AS-suffix is a verbatim synthetic-token
// splice rather than the backtick-quoting As Frag.
func aliasedFrag(inner Frag, bareAlias string) Frag {
	return func(b *Builder) {
		inner(b)
		verbatim(" AS " + bareAlias)(b)
	}
}

// vectorMatchPredicateFrag returns a Frag for the join's ON clause.
//
//   - default (Labels empty, On false) → L.Attributes = R.Attributes
//   - on(l1, l2)                       → AND of L.Attributes[k] = R.Attributes[k]
//   - ignoring(l1, l2)                 → mapFilter-stripped equality
//   - on() with no labels              -> 1 = 1 (per-side aggregation
//     already collapses to one row via the throwIf guard).
//
// When stepAligned is true the rendered predicate additionally ANDs in
// `L.<tsCol> = R.<tsCol>` so each anchor joins its own per-anchor
// pair — required when both sides emit a per-step matrix (range mode).
// Without that conjunct the join would degenerate into a full cross
// across anchors (roleMany) or fold the matrix down to a single per-
// match-key row at the surviving anchor (roleOne).
func vectorMatchPredicateFrag(m chplan.VectorMatch, attrsCol, tsCol string, stepAligned bool) Frag {
	key := matchKeyPredicateFrag(m, attrsCol)
	if !stepAligned {
		return key
	}
	// Step-aligned (range mode): additionally pin each anchor to its own
	// per-anchor pair — `<key> AND L.<tsCol> = R.<tsCol>`.
	return And(key, Eq(qualColFrag("L", tsCol), qualColFrag("R", tsCol)))
}

// matchKeyPredicateFrag renders just the label-matching half of the
// join's ON clause — extracted so the step-alignment AND can be
// composed on top without copying the per-shape branches.
func matchKeyPredicateFrag(m chplan.VectorMatch, attrsCol string) Frag {
	if len(m.Labels) == 0 && !m.On {
		return Eq(qualColFrag("L", attrsCol), qualColFrag("R", attrsCol))
	}
	if m.On && len(m.Labels) == 0 {
		// on() with no labels - every row on the left pairs with
		// every row on the right. The per-side aggregation already
		// collapses each side to one row via the throwIf-guard, so
		// the join condition is just a constant TRUE.
		return Eq(InlineLit(1), InlineLit(1))
	}
	if m.On {
		// `L.<attrs>[?] = R.<attrs>[?]` per on(...) label, AND-joined.
		perLabel := make([]Frag, len(m.Labels))
		for i, lbl := range m.Labels {
			perLabel[i] = Eq(
				Subscript(qualColFrag("L", attrsCol), Lit(lbl)),
				Subscript(qualColFrag("R", attrsCol), Lit(lbl)),
			)
		}
		return And(perLabel...)
	}
	// ignoring(...) — equate the complementary mapFilter on each side.
	ignFilter := func(side string) Frag {
		lbls := make([]Frag, len(m.Labels))
		for i, lbl := range m.Labels {
			lbls[i] = Lit(lbl)
		}
		return Call(
			"mapFilter",
			Lambda2("k", "v", Not(Paren(In(BareIdent("k"), lbls...)))),
			qualColFrag(side, attrsCol),
		)
	}
	return Eq(ignFilter("L"), ignFilter("R"))
}
