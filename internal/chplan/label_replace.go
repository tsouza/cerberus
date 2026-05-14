package chplan

// LabelReplace is a Map-valued expression that rewrites a single label
// inside an Attributes-shaped Map(String, String). It matches PromQL's
// `label_replace(v, dst, replacement, src, regex)`:
//
//   - if `regex` matches the entire value of `src` inside the map, bind
//     `dst` to the regex-substituted `replacement` (CH's `replaceRegexpOne`
//     anchored with `^…$`);
//   - if the regex does not match (including when `src` is absent in the
//     map and thus reads as the empty string), the map is returned
//     unchanged — `dst` keeps whatever value it had (often "absent").
//
// PromQL also drops labels whose value is the empty string; the emitter
// wraps the resulting map with `mapFilter((k, v) -> v != ”, …)` so
// `label_replace(m, "dst", "", "src", ".*")` correctly removes the dst
// label when present, matching ref Prometheus.
//
// Lowering:
//
//	mapFilter((k, v) -> v != '',
//	    if(match(<map>[<src>], '^<regex>$'),
//	       mapUpdate(<map>, map(<dst>, replaceRegexpOne(<map>[<src>], <regex>, <replacement>))),
//	       <map>))
//
// `Map` is the input Map(String, String) expression (typically the
// Attributes column ref). `Dst`, `Src`, `Replacement`, `Regex` are the
// PromQL-static string arguments captured during lowering.
type LabelReplace struct {
	Map         Expr
	Dst         string
	Replacement string
	Src         string
	Regex       string
}

func (*LabelReplace) exprNode() {}

func (l *LabelReplace) Equal(other Expr) bool {
	o, ok := other.(*LabelReplace)
	if !ok {
		return false
	}
	return l.Dst == o.Dst &&
		l.Replacement == o.Replacement &&
		l.Src == o.Src &&
		l.Regex == o.Regex &&
		l.Map.Equal(o.Map)
}
