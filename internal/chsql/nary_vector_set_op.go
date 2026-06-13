package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitNaryVectorSetOp renders the linearised N-ary form of an
// associative PromQL vector set-op chain (`a or b or c …` /
// `a and b and c …`) as ONE windowed single-pass over the arms'
// UNION ALL — the true-linearisation the optimizer's FlattenVectorSetOp
// rule unlocks.
//
// The binary VectorSetOp emitter already collapses a single `a or b`
// into one windowed pass (#88), but a left-assoc chain stays nested:
// `((a or b) or c) or d` wraps the previous windowed SELECT in another
// windowed SELECT per level, so a K-arm chain runs K-1 stacked window
// passes. Flattening to a single NaryVectorSetOp lets this emitter tag
// every arm with its source position and decide survival with ONE
// window aggregate over the unified arms — each arm is scanned exactly
// once.
//
//	SELECT MetricName, Attributes, TimeUnix, Value FROM (
//	  SELECT MetricName, Attributes, TimeUnix, Value, _setop_side,
//	         <survival window> AS _setop_<flag>
//	    FROM (
//	      (SELECT <canonical arm0 cols>, 0 AS _setop_side FROM (<a>))
//	      UNION ALL
//	      (SELECT <canonical arm1 cols>, 1 AS _setop_side FROM (<b>))
//	      UNION ALL … one arm per chain link, side = source position
//	    )
//	) WHERE <survival predicate>
//
// The survival shape differs by operator, but both reduce to a SINGLE
// window aggregate partitioned by the match-key signature:
//
//   - `or`  — earliest-arm-wins. `_setop_min_side =
//     min(_setop_side) OVER (PARTITION BY <sig>)` is the lowest arm
//     index that contributed a row for the signature. A row survives
//     iff `_setop_side = _setop_min_side`: the earliest arm's rows all
//     pass; every later arm's rows for the same signature drop. This
//     generalises the binary `_setop_has_left = 0` test (where the only
//     two indices are 0 / 1) to K arms and is byte-identical to the
//     nested left-assoc `or` result.
//   - `and` — present-in-every-arm. `_setop_sides =
//     groupBitOr(bitShiftLeft(1, _setop_side)) OVER (PARTITION BY
//     <sig>)` is the bitmask of arms that contributed a row for the
//     signature. Only arm-0 rows survive (`and` keeps the LHS values),
//     and only when the mask is all-ones — `(1 << K) - 1` — i.e. the
//     signature appears in EVERY arm. This matches the nested
//     left-assoc semi-join `((a and b) and c)` exactly: a row of `a`
//     survives iff its signature is present in `b` and in `c`.
//
// Each arm is canonicalised to the 4-column Sample tuple
// (MetricName, Attributes, TimeUnix, Value) via the same per-arm
// shape-normalisation the binary emitter uses, so positional UNION-ALL
// column unification never hits the String-vs-Map (NO_COMMON_TYPE)
// supertype error when an arm is a derived-shape RangeWindow /
// Aggregate that drops `__name__`. The side marker is appended as a 5th
// column, always last, so it never disturbs that unification. The
// window-bearing inner SELECT projects `_setop_side` explicitly so the
// outer WHERE can bind it (CH 24.x cannot resolve a UNION-arm alias
// from a window-SELECT's own WHERE).
func (e *emitter) emitNaryVectorSetOp(s *chplan.NaryVectorSetOp) error {
	if err := e.validateNaryVectorSetOpShape(s); err != nil {
		return err
	}

	sig := matchKeyGroupExprFrag(s.Match, s.AttributesColumn)
	outCols := naryVectorSetOpOutputCols(s)

	sideArms := make([]Frag, len(s.Arms))
	for i, arm := range s.Arms {
		armFrag, err := e.subqueryFrag(arm)
		if err != nil {
			return err
		}
		canonical := naryVectorSetOpCanonicalArmFrag(s, arm, armFrag)
		sideArms[i] = naryVectorSetOpSideArmFrag(s, canonical, i)
	}

	switch s.Op {
	case chplan.VectorSetOr:
		// earliest-arm-wins: survive iff this row's side is the minimum
		// side present for its signature.
		windowed := NewQuery().
			Select(append(
				outCols,
				Col(setOpSideCol),
				As(
					Window(Call("min", Col(setOpSideCol)), []Frag{sig}, nil),
					narySetOpMinSideCol,
				),
			)...).
			From(Paren(UnionAll(sideArms...)))
		outer := NewQuery().
			Select(outCols...).
			From(windowed.Frag()).
			Where(Eq(Col(setOpSideCol), Col(narySetOpMinSideCol)))
		e.emitSelect(outer)
		return nil
	case chplan.VectorSetAnd:
		// present-in-every-arm: keep arm-0 rows whose signature's
		// contributing-arm bitmask is all-ones (every arm present).
		fullMask := (int64(1) << uint(len(s.Arms))) - 1
		windowed := NewQuery().
			Select(append(
				outCols,
				Col(setOpSideCol),
				As(
					Window(
						Call("groupBitOr", narySetOpSideBitFrag()),
						[]Frag{sig},
						nil,
					),
					narySetOpSidesMaskCol,
				),
			)...).
			From(Paren(UnionAll(sideArms...)))
		outer := NewQuery().
			Select(outCols...).
			From(windowed.Frag()).
			Where(And(
				Eq(Col(setOpSideCol), InlineLit(0)),
				Eq(Col(narySetOpSidesMaskCol), InlineLit(fullMask)),
			))
		e.emitSelect(outer)
		return nil
	}
	return fmt.Errorf("%w: n-ary vector set op %q", ErrUnsupported, s.Op)
}

// Synthetic bare-identifier column names the NaryVectorSetOp single-pass
// emission pins, alongside setOpSideCol (shared with the binary
// emitter). `_setop_min_side` carries the earliest contributing arm
// index per signature for the `or` survival test; `_setop_sides_mask`
// carries the contributing-arm bitmask for the `and` test. Neither name
// takes user input; both match CH's bare-identifier grammar.
const (
	narySetOpMinSideCol   = "_setop_min_side"
	narySetOpSidesMaskCol = "_setop_sides_mask"
)

// narySetOpSideBitFrag renders `bitShiftLeft(toUInt64(1), _setop_side)`
// — the single-arm bit contributed to the per-signature
// contributing-arm bitmask aggregated by `groupBitOr` for the `and`
// survival test. toUInt64 keeps the shift width safe up to 64 arms,
// well past any realistic chain length.
func narySetOpSideBitFrag() Frag {
	return Call("bitShiftLeft", Call("toUInt64", InlineLit(1)), Col(setOpSideCol))
}

// naryVectorSetOpSideArmFrag wraps an already-canonicalised arm Frag in
// `SELECT MetricName, Attributes, TimeUnix, Value, <side> AS _setop_side
// FROM (<arm>)` so the single-pass UNION ALL carries the source-position
// marker as a 5th column. The marker is an inline shape constant (the
// arm's index), not user data, so InlineLit is correct.
func naryVectorSetOpSideArmFrag(s *chplan.NaryVectorSetOp, armFrag Frag, side int) Frag {
	q := NewQuery().
		Select(append(
			naryVectorSetOpOutputCols(s),
			As(InlineLit(side), setOpSideCol),
		)...).
		From(armFrag)
	return Paren(q.Frag())
}

// naryVectorSetOpOutputCols returns the canonical 4-column Sample
// projection (MetricName, Attributes, TimeUnix, Value) every
// NaryVectorSetOp arm + outer SELECT pins, matching the binary
// emitter's vectorSetOpOutputCols shape so the round-trip runner can
// recognise the Map column.
func naryVectorSetOpOutputCols(s *chplan.NaryVectorSetOp) []Frag {
	return []Frag{
		Col(s.MetricNameColumn),
		Col(s.AttributesColumn),
		Col(s.TimestampColumn),
		Col(s.ValueColumn),
	}
}

// naryVectorSetOpCanonicalArmFrag canonicalises one arm to the 4-column
// Sample tuple, reusing the binary emitter's shape-classification logic
// via a synthesised VectorSetOp view that carries the shared column
// names. Derived-shape arms (RangeWindow / Aggregate / …) get their
// missing MetricName / TimeUnix synthesised exactly as the binary path
// does, so a flattened chain emits byte-identical arm projections to the
// nested form it replaces.
func naryVectorSetOpCanonicalArmFrag(s *chplan.NaryVectorSetOp, arm chplan.Node, armFrag Frag) Frag {
	view := &chplan.VectorSetOp{
		Op:               s.Op,
		Match:            s.Match,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
	return vectorSetOpCanonicalArmFrag(view, arm, armFrag)
}

// validateNaryVectorSetOpShape rejects a NaryVectorSetOp the flatten
// rule should never have produced: fewer than two arms, an unset
// canonical column name, or a non-associative operator. `unless` is not
// associative, so it must never reach the N-ary node — the flatten rule
// only ever linearises `or` / `and` chains.
func (e *emitter) validateNaryVectorSetOpShape(s *chplan.NaryVectorSetOp) error {
	switch {
	case len(s.Arms) < 2:
		return fmt.Errorf("%w: NaryVectorSetOp needs >= 2 arms, got %d", ErrUnsupported, len(s.Arms))
	case s.AttributesColumn == "":
		return fmt.Errorf("%w: NaryVectorSetOp.AttributesColumn unset", ErrUnsupported)
	case s.MetricNameColumn == "":
		return fmt.Errorf("%w: NaryVectorSetOp.MetricNameColumn unset", ErrUnsupported)
	case s.TimestampColumn == "":
		return fmt.Errorf("%w: NaryVectorSetOp.TimestampColumn unset", ErrUnsupported)
	case s.ValueColumn == "":
		return fmt.Errorf("%w: NaryVectorSetOp.ValueColumn unset", ErrUnsupported)
	case s.Op != chplan.VectorSetOr && s.Op != chplan.VectorSetAnd:
		return fmt.Errorf("%w: NaryVectorSetOp op %q is not associative", ErrUnsupported, s.Op)
	}
	return nil
}
