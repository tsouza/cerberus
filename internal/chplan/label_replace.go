package chplan

// LabelReplace is a Map-valued expression that rewrites a single label
// inside an Attributes-shaped Map(String, String). It matches PromQL's
// `label_replace(v, dst, replacement, src, regex)`:
//
//   - if `regex` matches the entire value of `src` inside the map, bind
//     `dst` to the regex-substituted `replacement` (CH's `replaceRegexpOne`
//     anchored with `^…$`);
//   - if the regex does not match (including when `src` is absent in the
//     map and thus reads as the empty string AND the regex doesn't
//     match the empty string), the map is returned unchanged — `dst`
//     keeps whatever value it had (often "absent").
//   - if `src` is empty but the regex DOES match the empty string (e.g.
//     `(.*)`), `dst` is bound to `EmptyReplacement` — the build-time
//     pre-computed result of substituting every capture group with "".
//     This is the Prom-spec branch for "source label absent but regex
//     matches": Prom emits the literal portion of the replacement (e.g.
//     `"value-$1"` against an empty match → `"value-"`).
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
//	       mapUpdate(<map>, map(<dst>,
//	          if(empty(<map>[<src>]),
//	             <emptyReplacement>,
//	             replaceRegexpOne(<map>[<src>], <regex>, <replacement>)))),
//	       <map>))
//
// `Map` is the input Map(String, String) expression (typically the
// Attributes column ref). `Dst`, `Src`, `Replacement`, `Regex` are the
// PromQL-static string arguments captured during lowering.
// `EmptyReplacement` is the build-time pre-computed result of
// substituting every capture group with `""` (see
// `qlcommon.EmptyCapturesReplacement`); it works around CH ≤ 24.8's
// `replaceRegexpOne(”, '^(.*)$', 'value-\1')` returning `""` instead
// of the spec-correct `"value-"`.
type LabelReplace struct {
	Map              Expr
	Dst              string
	Replacement      string
	Src              string
	Regex            string
	EmptyReplacement string
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
		l.EmptyReplacement == o.EmptyReplacement &&
		l.Map.Equal(o.Map)
}
