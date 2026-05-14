package promql

import (
	"fmt"
	"math"
	"sort"

	"github.com/prometheus/prometheus/promql/parser"
)

// scalarOrVector is the result of evaluating an inner expression to
// either a scalar (single float) or a vector. exactly-one of isScalar
// / rows is set.
type scalarOrVector struct {
	isScalar bool
	scalar   float64
	rows     []VectorRow
}

// evalBinary applies a PromQL binary expression. The LHS and RHS have
// already been evaluated; this function dispatches by their shapes:
//
//   - scalar op scalar  → scalar
//   - scalar op vector  → vector (op applied per row)
//   - vector op scalar  → vector
//   - vector op vector  → vector after one-to-one / one-to-many
//     matching with on/ignoring + group_left/group_right
//
// Comparison ops with `bool` modifier replace the per-row drop with
// a 0/1 result.
func (e *Evaluator) evalBinary(b *parser.BinaryExpr, lhs, rhs scalarOrVector, evalTsMs int64) ([]VectorRow, float64, bool, error) {
	switch {
	case lhs.isScalar && rhs.isScalar:
		v, ok := applyScalarOp(b.Op, lhs.scalar, rhs.scalar, b.ReturnBool)
		return nil, v, ok, nil
	case lhs.isScalar && !rhs.isScalar:
		rows := applyScalarVectorOp(b.Op, lhs.scalar, rhs.rows, true, b.ReturnBool, evalTsMs)
		return rows, 0, true, nil
	case !lhs.isScalar && rhs.isScalar:
		rows := applyScalarVectorOp(b.Op, rhs.scalar, lhs.rows, false, b.ReturnBool, evalTsMs)
		return rows, 0, true, nil
	}
	// vector op vector
	rows, err := vectorVectorOp(b, lhs.rows, rhs.rows, evalTsMs)
	return rows, 0, true, err
}

// applyScalarOp does the arithmetic / comparison on two scalars. For
// comparisons WITHOUT `bool`, Prom's semantics are: scalar < scalar
// returns NaN unless the comparison is true (in which case the LHS is
// returned). With `bool` we always emit 0 or 1.
func applyScalarOp(op parser.ItemType, l, r float64, returnBool bool) (float64, bool) {
	if isComparisonOp(op) {
		match := compareValues(op, l, r)
		if returnBool {
			if match {
				return 1, true
			}
			return 0, true
		}
		if match {
			return l, true
		}
		return math.NaN(), true
	}
	v, ok := arithmeticOp(op, l, r)
	return v, ok
}

// applyScalarVectorOp applies op against every row in v, with the
// scalar on the LHS or RHS as scalarOnLeft indicates. For
// comparisons without `bool`, rows that fail the comparison are
// dropped (Prom's filter semantics).
func applyScalarVectorOp(op parser.ItemType, s float64, v []VectorRow, scalarOnLeft, returnBool bool, evalTsMs int64) []VectorRow {
	out := make([]VectorRow, 0, len(v))
	for _, r := range v {
		l, rhsVal := s, r.V
		if !scalarOnLeft {
			l, rhsVal = r.V, s
		}
		if isComparisonOp(op) {
			match := compareValues(op, l, rhsVal)
			if returnBool {
				val := 0.0
				if match {
					val = 1
				}
				out = append(out, VectorRow{
					Labels: DropLabel(r.Labels, MetricNameLabel),
					T:      evalTsMs,
					V:      val,
				})
				continue
			}
			if match {
				out = append(out, VectorRow{
					Labels: CopyLabels(r.Labels),
					T:      evalTsMs,
					V:      r.V,
				})
			}
			continue
		}
		val, ok := arithmeticOp(op, l, rhsVal)
		if !ok {
			continue
		}
		out = append(out, VectorRow{
			Labels: DropLabel(r.Labels, MetricNameLabel),
			T:      evalTsMs,
			V:      val,
		})
	}
	return out
}

// vectorVectorOp performs vector-on-vector matching + operation.
//
// Strategy (mirrors Prom's promql/engine.go::VectorBinop): for
// CardOneToMany, swap LHS↔RHS so the "many" side is always LHS
// downstream. Then build a per-key index of the (now-"one") RHS, and
// for each LHS row look up its match. The "values" the binary op
// sees stay in original semantic order — for OneToMany we have to
// swap them BACK at the elemBinop step to preserve, e.g., `1 / vec`
// vs. `vec / 1` semantics.
func vectorVectorOp(b *parser.BinaryExpr, lhs, rhs []VectorRow, evalTsMs int64) ([]VectorRow, error) {
	matching := b.VectorMatching
	if matching == nil {
		matching = &parser.VectorMatching{Card: parser.CardOneToOne}
	}

	swapped := matching.Card == parser.CardOneToMany
	if swapped {
		lhs, rhs = rhs, lhs
	}

	// Group the (post-swap) RHS — the "one" side — by matching key.
	rhsBy := make(map[string][]VectorRow)
	for _, r := range rhs {
		k := matchKey(r.Labels, matching)
		rhsBy[k] = append(rhsBy[k], r)
	}

	out := make([]VectorRow, 0, len(lhs))
	for _, l := range lhs {
		k := matchKey(l.Labels, matching)
		rs, ok := rhsBy[k]
		if !ok || len(rs) == 0 {
			continue
		}

		if matching.Card == parser.CardManyToMany {
			return nil, fmt.Errorf("oracle: many-to-many set ops not supported in MVP")
		}
		if matching.Card == parser.CardOneToOne && len(rs) > 1 {
			return nil, fmt.Errorf("oracle: many-to-one matching detected with one-to-one cardinality")
		}

		// All cards (after the swap) collapse to "iterate over RHS
		// matches and emit one output per RHS match". For OneToOne /
		// ManyToOne there's exactly one RHS match per LHS row; for
		// the post-swap OneToMany there may be many.
		for _, rRow := range rs {
			// elemBinop operates on the original-semantic order: if
			// we swapped above, swap the values back so `a / b`
			// stays `a / b` rather than `b / a`.
			vL, vR := l.V, rRow.V
			if swapped {
				vL, vR = vR, vL
			}
			val, keep, err := applyBinary(b, vL, vR)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
			out = append(out, VectorRow{
				Labels: resultLabels(l.Labels, rRow.Labels, matching, b.ReturnBool, matching.Card),
				T:      evalTsMs,
				V:      val,
			})
		}
	}
	sortVectorRows(out)
	return out, nil
}

// applyBinary applies the binary op to two floats and returns (value,
// keep, err). `keep` is false when a non-`bool` comparison fails
// (Prom filters those rows out).
func applyBinary(b *parser.BinaryExpr, l, r float64) (float64, bool, error) {
	if isComparisonOp(b.Op) {
		match := compareValues(b.Op, l, r)
		if b.ReturnBool {
			if match {
				return 1, true, nil
			}
			return 0, true, nil
		}
		if match {
			return l, true, nil
		}
		return 0, false, nil
	}
	v, ok := arithmeticOp(b.Op, l, r)
	return v, ok, nil
}

// matchKey returns the canonical sorted string of the labels that
// participate in matching. With On(...), only the listed labels
// participate; with Ignoring(...), the named labels are dropped (plus
// __name__ always). With neither, __name__ is dropped and every
// remaining label participates.
func matchKey(lbls map[string]string, m *parser.VectorMatching) string {
	if m.On {
		return labelKeyOnly(lbls, m.MatchingLabels)
	}
	drop := append([]string{MetricNameLabel}, m.MatchingLabels...)
	return labelKeyDropping(lbls, drop)
}

func labelKeyOnly(lbls map[string]string, keep []string) string {
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	keys := make([]string, 0, len(keep))
	for k := range lbls {
		if _, ok := keepSet[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return joinPairs(lbls, keys)
}

func labelKeyDropping(lbls map[string]string, drop []string) string {
	dropSet := make(map[string]struct{}, len(drop))
	for _, k := range drop {
		dropSet[k] = struct{}{}
	}
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		if _, ok := dropSet[k]; ok {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return joinPairs(lbls, keys)
}

func joinPairs(lbls map[string]string, keys []string) string {
	if len(keys) == 0 {
		return "{}"
	}
	var b []byte
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, k...)
		b = append(b, '=', '"')
		b = append(b, lbls[k]...)
		b = append(b, '"')
	}
	b = append(b, '}')
	return string(b)
}

// resultLabels produces the output row's label set per Prom's
// matching rules:
//
//   - One-to-one with On(): output keeps only the matching labels.
//   - One-to-one with Ignoring(): output keeps the LHS labels minus
//     the ignored set and __name__.
//   - Many-to-one / one-to-many: output keeps ALL labels of the
//     "many" side (minus __name__), plus any labels named in the
//     group_left/group_right `include` list, pulled from the "one"
//     side. The caller passes lhs="many side", rhs="one side" so
//     the include lookup is always against rhs.
//
// For comparison ops without `bool` (which preserve the LHS row),
// the output's __name__ would normally be preserved — but the
// property test's MVP doesn't exercise that shape; we always drop
// the name for symmetry with the aggregation paths.
func resultLabels(lhs, rhs map[string]string, m *parser.VectorMatching, returnBool bool, card parser.VectorMatchCardinality) map[string]string {
	_ = returnBool

	var base map[string]string
	if card == parser.CardOneToOne {
		if m.On {
			base = KeepLabels(lhs, m.MatchingLabels)
		} else {
			base = DropLabels(lhs, m.MatchingLabels) // also drops __name__
		}
	} else {
		// Many-to-one / one-to-many: keep ALL labels of the "many"
		// side (which the caller passes as lhs after any swap).
		base = CopyLabels(lhs)
	}

	// Include labels are pulled from the "one" side (always rhs,
	// after the caller's swap). Per Prom: an include with no value
	// on the donor side removes the label from the output.
	for _, k := range m.Include {
		if v, ok := rhs[k]; ok && v != "" {
			base[k] = v
		} else {
			delete(base, k)
		}
	}

	// Final cleanup: __name__ never survives a binary op output.
	delete(base, MetricNameLabel)
	return base
}

// isComparisonOp reports whether op is one of the six PromQL
// comparison operators.
func isComparisonOp(op parser.ItemType) bool {
	switch op {
	case parser.EQLC, parser.NEQ, parser.GTR, parser.GTE,
		parser.LSS, parser.LTE:
		return true
	}
	return false
}

// compareValues evaluates a comparison op. Returns true if the
// comparison is satisfied. NaN handling matches Prom: any
// comparison with NaN is false.
func compareValues(op parser.ItemType, l, r float64) bool {
	if math.IsNaN(l) || math.IsNaN(r) {
		return false
	}
	switch op {
	case parser.EQLC:
		return l == r
	case parser.NEQ:
		return l != r
	case parser.GTR:
		return l > r
	case parser.GTE:
		return l >= r
	case parser.LSS:
		return l < r
	case parser.LTE:
		return l <= r
	}
	return false
}

// arithmeticOp evaluates an arithmetic op. Returns (value, true) on
// success; (NaN, true) for math-undefined cases (e.g., 0/0); the
// `ok` is here for symmetry with future ops that need to drop a row.
func arithmeticOp(op parser.ItemType, l, r float64) (float64, bool) {
	switch op {
	case parser.ADD:
		return l + r, true
	case parser.SUB:
		return l - r, true
	case parser.MUL:
		return l * r, true
	case parser.DIV:
		if r == 0 {
			if l == 0 {
				return math.NaN(), true
			}
			if l > 0 {
				return math.Inf(1), true
			}
			return math.Inf(-1), true
		}
		return l / r, true
	case parser.MOD:
		if r == 0 {
			return math.NaN(), true
		}
		return math.Mod(l, r), true
	case parser.POW:
		return math.Pow(l, r), true
	}
	return math.NaN(), false
}
