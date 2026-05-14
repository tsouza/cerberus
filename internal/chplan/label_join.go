package chplan

import "slices"

// LabelJoin is a Map-valued expression that concatenates the values of
// named source labels with a separator and binds the result to a
// destination label. Matches PromQL's
// `label_join(v, dst, separator, src1, src2, ...)`:
//
//   - For each input series, evaluate `arrayStringConcat([map[src1],
//     map[src2], ...], separator)` and bind it to `dst` in the output
//     Map(String, String).
//   - If every src label is absent (map returns the empty string for
//     missing keys), the joined value is just the separator repeated;
//     the emitter wraps the result with the standard empty-value drop
//     so the dst label disappears when the join is fully empty.
//
// Lowering:
//
//	mapFilter((k, v) -> v != '',
//	    mapUpdate(<map>, map(<dst>, arrayStringConcat([<map>[<src1>], <map>[<src2>], ...], <separator>))))
//
// `Map` is the input Map(String, String) expression (typically the
// Attributes column ref). `Dst`, `Separator`, and `Srcs` are the PromQL-
// static string arguments captured during lowering.
type LabelJoin struct {
	Map       Expr
	Dst       string
	Separator string
	Srcs      []string
}

func (*LabelJoin) exprNode() {}

func (l *LabelJoin) Equal(other Expr) bool {
	o, ok := other.(*LabelJoin)
	if !ok {
		return false
	}
	return l.Dst == o.Dst &&
		l.Separator == o.Separator &&
		slices.Equal(l.Srcs, o.Srcs) &&
		l.Map.Equal(o.Map)
}
