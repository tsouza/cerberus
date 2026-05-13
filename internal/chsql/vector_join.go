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
// All new SQL flows through chsql.Builder; the legacy emitter.b buffer
// receives only the rendered fragments via splice.
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

	// Outer SELECT prefix — composed via Builder, then spliced.
	header := NewBuilder()
	header.WriteSQL("SELECT ")
	writeSideCol(header, outerSide, j.MetricNameColumn)
	header.WriteSQL(", ")
	writeOutputAttributes(header, j)
	header.WriteSQL(", ")
	writeSideCol(header, outerSide, j.TimestampColumn)
	header.WriteSQL(", (")
	writeSideCol(header, "L", j.ValueColumn)
	header.WriteSQL(" " + string(j.Op) + " ")
	writeSideCol(header, "R", j.ValueColumn)
	header.WriteSQL(") AS ")
	header.Ident(j.ValueColumn)
	header.WriteSQL(" FROM ")
	e.splice(header)

	// Left side — aggregation shape depends on its role.
	if err := e.emitVectorJoinSide(j, j.Left, leftRole); err != nil {
		return err
	}
	mid := NewBuilder()
	mid.WriteSQL(" AS L INNER JOIN ")
	e.splice(mid)

	if err := e.emitVectorJoinSide(j, j.Right, rightRole); err != nil {
		return err
	}

	tail := NewBuilder()
	tail.WriteSQL(" AS R ON ")
	writeVectorMatchPredicate(tail, j.Match, j.AttributesColumn)
	e.splice(tail)
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
func vectorJoinRoles(j *chplan.VectorJoin) (left, right sideRole) {
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

// emitVectorJoinSide renders one side of the join as a parenthesised
// subquery. roleMany keeps per-series granularity (one row per
// `(MetricName, Attributes)` group). roleOne collapses to one row
// per matching key with a `throwIf(uniqExact(Attributes) > 1, ...)`
// side-effect column — the "many-to-many matching not allowed"
// Prometheus error surfaces at CH query-execution time rather than
// as a silent cross-product.
func (e *emitter) emitVectorJoinSide(j *chplan.VectorJoin, n chplan.Node, role sideRole) error {
	b := NewBuilder()
	b.WriteSQL("(SELECT ")
	if role == roleMany {
		b.Ident(j.MetricNameColumn)
		b.WriteSQL(", ")
		b.Ident(j.AttributesColumn)
		b.WriteSQL(", max(")
		b.Ident(j.TimestampColumn)
		b.WriteSQL(") AS ")
		b.Ident(j.TimestampColumn)
		b.WriteSQL(", argMax(")
		b.Ident(j.ValueColumn)
		b.WriteSQL(", ")
		b.Ident(j.TimestampColumn)
		b.WriteSQL(") AS ")
		b.Ident(j.ValueColumn)
		b.WriteSQL(" FROM ")
		e.splice(b)
		if err := e.emitSubquery(n); err != nil {
			return err
		}
		grp := NewBuilder()
		grp.WriteSQL(" GROUP BY ")
		grp.Ident(j.MetricNameColumn)
		grp.WriteSQL(", ")
		grp.Ident(j.AttributesColumn)
		grp.WriteSQL(")")
		e.splice(grp)
		return nil
	}

	// "one" side — aggregate by matching key with a runtime uniqueness
	// guard. argMax(Attributes, TimeUnix) picks one representative
	// series per match key; throwIf fires when there's more than one.
	b.WriteSQL("argMax(")
	b.Ident(j.MetricNameColumn)
	b.WriteSQL(", ")
	b.Ident(j.TimestampColumn)
	b.WriteSQL(") AS ")
	b.Ident(j.MetricNameColumn)
	b.WriteSQL(", argMax(")
	b.Ident(j.AttributesColumn)
	b.WriteSQL(", ")
	b.Ident(j.TimestampColumn)
	b.WriteSQL(") AS ")
	b.Ident(j.AttributesColumn)
	b.WriteSQL(", max(")
	b.Ident(j.TimestampColumn)
	b.WriteSQL(") AS ")
	b.Ident(j.TimestampColumn)
	b.WriteSQL(", argMax(")
	b.Ident(j.ValueColumn)
	b.WriteSQL(", ")
	b.Ident(j.TimestampColumn)
	b.WriteSQL(") AS ")
	b.Ident(j.ValueColumn)
	b.WriteSQL(", throwIf(uniqExact(")
	b.Ident(j.AttributesColumn)
	b.WriteSQL(") > 1, ")
	b.Arg("many-to-many matching not allowed: matching labels must be unique on one side")
	b.WriteSQL(") AS _cerberus_match_check FROM ")
	e.splice(b)
	if err := e.emitSubquery(n); err != nil {
		return err
	}
	grp := NewBuilder()
	grp.WriteSQL(" GROUP BY ")
	writeMatchKeyGroupExpr(grp, j.Match, j.AttributesColumn)
	grp.WriteSQL(")")
	e.splice(grp)
	return nil
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
		b.WriteSQL("tuple()")
		return
	}
	if m.On {
		b.WriteSQL("mapFilter((k, v) -> k IN (")
		for i, lbl := range m.Labels {
			if i > 0 {
				b.WriteSQL(", ")
			}
			b.Arg(lbl)
		}
		b.WriteSQL("), ")
		b.Ident(attrsCol)
		b.WriteSQL(")")
		return
	}
	b.MapFilterExcept(attrsCol, m.Labels...)
}

// writeOutputAttributes emits the output Attributes expression for the
// join. For CardOneToOne the output equals L.Attributes. For
// group_left(<labels>) the output merges the named labels from
// R.Attributes onto L.Attributes via mapConcat (CH's later-key-wins
// map merge). group_right mirrors with roles swapped.
//
// When no Include labels are present (bare `group_left` without an
// explicit label list, which is uncommon but parser-legal), the
// "many" side's Attributes flows through unchanged — this matches
// Prometheus's behaviour where bare group_left/right copies nothing
// beyond the matching key.
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
	b.WriteSQL("mapConcat(")
	writeSideCol(b, manySide, j.AttributesColumn)
	b.WriteSQL(", mapFilter((k, v) -> k IN (")
	for i, lbl := range j.Include {
		if i > 0 {
			b.WriteSQL(", ")
		}
		b.Arg(lbl)
	}
	b.WriteSQL("), ")
	writeSideCol(b, oneSide, j.AttributesColumn)
	b.WriteSQL(")) AS ")
	b.Ident(j.AttributesColumn)
}

// writeSideCol emits `<side>.<col>` where <side> is the unquoted alias
// (L or R) and <col> is the backtick-quoted column identifier. The
// unquoted alias matches the legacy emitter's output so existing
// fixtures that pre-date the Builder port stay stable.
func writeSideCol(b *Builder, side, col string) {
	b.WriteSQL(side)
	b.WriteSQL(".")
	b.Ident(col)
}

// writeVectorMatchPredicate emits the ON clause for the join.
//
//   - default (Labels empty, On false) → L.Attributes = R.Attributes
//   - on(l1, l2)                       → AND of L.Attributes[k] = R.Attributes[k]
//   - ignoring(l1, l2)                 → mapFilter-stripped equality
func writeVectorMatchPredicate(b *Builder, m chplan.VectorMatch, attrsCol string) {
	if len(m.Labels) == 0 && !m.On {
		writeSideCol(b, "L", attrsCol)
		b.WriteSQL(" = ")
		writeSideCol(b, "R", attrsCol)
		return
	}
	if m.On && len(m.Labels) == 0 {
		// on() with no labels — every row on the left pairs with
		// every row on the right. The per-side aggregation already
		// collapses each side to one row via the throwIf-guard, so
		// the join condition is just a constant TRUE.
		b.WriteSQL("1 = 1")
		return
	}
	if m.On {
		for i, lbl := range m.Labels {
			if i > 0 {
				b.WriteSQL(" AND ")
			}
			writeSideCol(b, "L", attrsCol)
			b.WriteSQL("[")
			b.Arg(lbl)
			b.WriteSQL("] = ")
			writeSideCol(b, "R", attrsCol)
			b.WriteSQL("[")
			b.Arg(lbl)
			b.WriteSQL("]")
		}
		return
	}
	for i, side := range []string{"L", "R"} {
		if i == 1 {
			b.WriteSQL(" = ")
		}
		b.WriteSQL("mapFilter((k, v) -> NOT (k IN (")
		for j, lbl := range m.Labels {
			if j > 0 {
				b.WriteSQL(", ")
			}
			b.Arg(lbl)
		}
		b.WriteSQL(")), ")
		writeSideCol(b, side, attrsCol)
		b.WriteSQL(")")
	}
}
