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
//     side"). Output Attributes preserves the left side's full label
//     set (one representative series per match key, picked via
//     argMax(Attributes, TimeUnix)).
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
// JOIN slot all flow through typed QueryBuilder slots. The bare-alias glue (`AS L` /
// `AS R`) is operator-token-style writeSQL inside a Frag — CH accepts
// unquoted single-letter aliases and the existing fixtures pin that
// shape.
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
			qualColFrag(outerSide, j.MetricNameColumn),
			outputAttributesFrag(j),
			qualColFrag(outerSide, j.TimestampColumn),
			vectorJoinValueExprFrag(j),
		).
		From(aliasedFrag(leftFrag, "L")).
		Join(
			InnerJoin,
			aliasedFrag(rightFrag, "R"),
			vectorMatchPredicateFrag(j.Match, j.AttributesColumn),
		)
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
	inner := NewQuery().From(sub)
	if role == roleMany {
		inner.Select(
			As(Col(j.MetricNameColumn), joinAlias(j.MetricNameColumn)),
			As(Col(j.AttributesColumn), joinAlias(j.AttributesColumn)),
			aggMaxAs(j.TimestampColumn, joinAlias(j.TimestampColumn)),
			argMaxAs(j.ValueColumn, j.TimestampColumn, joinAlias(j.ValueColumn)),
		).GroupBy(
			Col(j.MetricNameColumn),
			Col(j.AttributesColumn),
		)
	} else {
		// "one" side — aggregate by matching key with a runtime
		// uniqueness guard. argMax(Attributes, TimeUnix) picks one
		// representative series per match key; throwIf fires when
		// there's more than one.
		inner.Select(
			argMaxAs(j.MetricNameColumn, j.TimestampColumn, joinAlias(j.MetricNameColumn)),
			argMaxAs(j.AttributesColumn, j.TimestampColumn, joinAlias(j.AttributesColumn)),
			aggMaxAs(j.TimestampColumn, joinAlias(j.TimestampColumn)),
			argMaxAs(j.ValueColumn, j.TimestampColumn, joinAlias(j.ValueColumn)),
			matchCheckFrag(j.AttributesColumn),
		).GroupBy(matchKeyGroupExprFrag(j.Match, j.AttributesColumn))
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

// aggMaxAs returns a Frag for `max(<col>) AS <alias>`.
func aggMaxAs(col, alias string) Frag {
	return As(func(b *Builder) {
		b.writeSQL("max(")
		b.Ident(col)
		b.writeSQL(")")
	}, alias)
}

// argMaxAs returns a Frag for `argMax(<valCol>, <byCol>) AS <alias>`.
func argMaxAs(valCol, byCol, alias string) Frag {
	return As(func(b *Builder) {
		b.writeSQL("argMax(")
		b.Ident(valCol)
		b.writeSQL(", ")
		b.Ident(byCol)
		b.writeSQL(")")
	}, alias)
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
	return func(b *Builder) {
		b.writeSQL("throwIf(uniqExact(")
		b.Ident(attrsCol)
		b.writeSQL(") > 1, ")
		b.Arg("many-to-many matching not allowed: matching labels must be unique on one side")
		b.writeSQL(") AS _cerberus_match_check")
	}
}

// matchKeyGroupExprFrag returns a Frag for the GROUP BY expression
// that collapses rows onto a single matching key. For default matching
// (full Attributes) this is just the Attributes column; for on(labels)
// it's `mapFilter((k, v) -> k IN (...), Attributes)`; for
// ignoring(labels) it's the complementary mapFilter.
func matchKeyGroupExprFrag(m chplan.VectorMatch, attrsCol string) Frag {
	return func(b *Builder) { writeMatchKeyGroupExpr(b, m, attrsCol) }
}

// writeMatchKeyGroupExpr emits the GROUP BY expression that collapses
// rows onto a single matching key. For default matching (full
// Attributes) this is just the Attributes column; for on(labels) it's
// `mapFilter((k,v) -> k IN (...), Attributes)`; for ignoring(labels)
// it's the complementary mapFilter.
func writeMatchKeyGroupExpr(b *Builder, m chplan.VectorMatch, attrsCol string) {
	if len(m.Labels) == 0 && !m.On {
		b.Ident(attrsCol)
		return
	}
	if m.On && len(m.Labels) == 0 {
		// on() with no labels — group everything onto a single
		// match-key. CH doesn't allow an empty IN list, so emit a
		// constant tuple.
		b.writeSQL("tuple()")
		return
	}
	if m.On {
		b.writeSQL("mapFilter((k, v) -> k IN (")
		for i, lbl := range m.Labels {
			if i > 0 {
				b.writeSQL(", ")
			}
			b.Arg(lbl)
		}
		b.writeSQL("), ")
		b.Ident(attrsCol)
		b.writeSQL(")")
		return
	}
	b.MapFilterExcept(attrsCol, m.Labels...)
}

// outputAttributesFrag returns a Frag for the output Attributes
// expression. For CardOneToOne the output equals the "many" side's
// Attributes. For group_left(<labels>) the output merges the named
// labels from the "one" side onto the "many" side via mapConcat (CH's
// later-key-wins map merge). group_right mirrors with roles swapped.
//
// When no Include labels are present (bare `group_left` without an
// explicit label list, which is uncommon but parser-legal), the
// "many" side's Attributes flows through unchanged — this matches
// Prometheus's behaviour where bare group_left/right copies nothing
// beyond the matching key.
func outputAttributesFrag(j *chplan.VectorJoin) Frag {
	return func(b *Builder) { writeOutputAttributes(b, j) }
}

func writeOutputAttributes(b *Builder, j *chplan.VectorJoin) {
	manySide := ""
	switch j.Card {
	case chplan.CardManyToOne:
		manySide = "L"
	case chplan.CardOneToMany:
		manySide = "R"
	}
	if manySide == "" || len(j.Include) == 0 {
		// Either CardOneToOne or bare group_left/right — output is
		// the "many" side's Attributes (L for OneToOne and ManyToOne,
		// R for OneToMany).
		side := "L"
		if manySide == "R" {
			side = "R"
		}
		writeSideCol(b, side, j.AttributesColumn)
		return
	}

	// group_left/right with Include labels — overlay the "one" side's
	// matching labels onto the "many" side's full Attributes. Use
	// mapConcat: the later argument's keys overwrite the earlier's.
	oneSide := "R"
	if manySide == "R" {
		oneSide = "L"
	}
	b.writeSQL("mapConcat(")
	writeSideCol(b, manySide, j.AttributesColumn)
	b.writeSQL(", mapFilter((k, v) -> k IN (")
	for i, lbl := range j.Include {
		if i > 0 {
			b.writeSQL(", ")
		}
		b.Arg(lbl)
	}
	b.writeSQL("), ")
	writeSideCol(b, oneSide, j.AttributesColumn)
	b.writeSQL(")) AS ")
	b.Ident(j.AttributesColumn)
}

// vectorJoinValueExprFrag returns a Frag for the joined value
// expression: `(L.<val> <op> R.<val>) AS <val>`.
func vectorJoinValueExprFrag(j *chplan.VectorJoin) Frag {
	return func(b *Builder) {
		b.writeSQL("(")
		writeSideCol(b, "L", j.ValueColumn)
		b.writeSQL(" " + string(j.Op) + " ")
		writeSideCol(b, "R", j.ValueColumn)
		b.writeSQL(") AS ")
		b.Ident(j.ValueColumn)
	}
}

// qualColFrag returns a Frag for `<bareSide>.<col>` — the bare-alias
// L / R qualifier the legacy emitter pins.
func qualColFrag(side, col string) Frag {
	return func(b *Builder) { writeSideCol(b, side, col) }
}

// aliasedFrag wraps inner in a trailing ` AS <bareAlias>`. The alias
// is rendered bare (no backticks) — CH accepts unquoted single-letter
// aliases, and the vector_join / structural_join fixtures pin that
// shape.
func aliasedFrag(inner Frag, bareAlias string) Frag {
	return func(b *Builder) {
		inner(b)
		b.writeSQL(" AS ")
		b.writeSQL(bareAlias)
	}
}

// writeSideCol emits `<side>.<col>` where <side> is the unquoted alias
// (L or R) and <col> is the backtick-quoted column identifier. The
// unquoted alias matches the legacy emitter's output so existing
// fixtures that pre-date the Builder port stay stable.
func writeSideCol(b *Builder, side, col string) {
	b.writeSQL(side)
	b.writeSQL(".")
	b.Ident(col)
}

// vectorMatchPredicateFrag returns a Frag for the join's ON clause.
//
//   - default (Labels empty, On false) → L.Attributes = R.Attributes
//   - on(l1, l2)                       → AND of L.Attributes[k] = R.Attributes[k]
//   - ignoring(l1, l2)                 → mapFilter-stripped equality
//   - on() with no labels              → 1 = 1 (per-side aggregation
//     already collapses to one row via the throwIf guard).
func vectorMatchPredicateFrag(m chplan.VectorMatch, attrsCol string) Frag {
	return func(b *Builder) { writeVectorMatchPredicate(b, m, attrsCol) }
}

func writeVectorMatchPredicate(b *Builder, m chplan.VectorMatch, attrsCol string) {
	if len(m.Labels) == 0 && !m.On {
		writeSideCol(b, "L", attrsCol)
		b.writeSQL(" = ")
		writeSideCol(b, "R", attrsCol)
		return
	}
	if m.On && len(m.Labels) == 0 {
		// on() with no labels — every row on the left pairs with
		// every row on the right. The per-side aggregation already
		// collapses each side to one row via the throwIf-guard, so
		// the join condition is just a constant TRUE.
		b.writeSQL("1 = 1")
		return
	}
	if m.On {
		for i, lbl := range m.Labels {
			if i > 0 {
				b.writeSQL(" AND ")
			}
			writeSideCol(b, "L", attrsCol)
			b.writeSQL("[")
			b.Arg(lbl)
			b.writeSQL("] = ")
			writeSideCol(b, "R", attrsCol)
			b.writeSQL("[")
			b.Arg(lbl)
			b.writeSQL("]")
		}
		return
	}
	for i, side := range []string{"L", "R"} {
		if i == 1 {
			b.writeSQL(" = ")
		}
		b.writeSQL("mapFilter((k, v) -> NOT (k IN (")
		for j, lbl := range m.Labels {
			if j > 0 {
				b.writeSQL(", ")
			}
			b.Arg(lbl)
		}
		b.writeSQL(")), ")
		writeSideCol(b, side, attrsCol)
		b.writeSQL(")")
	}
}
