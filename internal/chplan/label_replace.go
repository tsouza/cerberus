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
// A replacement template that references a capture group above CH's `\9`
// substitution ceiling cannot be spelled as a `replaceRegexpOne`
// template at all. Such a template is carried as `Segments` instead, and
// the emitter builds the value by concatenation:
//
//	concat(<literal>, extractGroups(<map>[<src>], '^<regex>$')[<n>], …)
//
// `extractGroups` returns every capture group as an `Array(String)`, so an
// arbitrary group index is an ordinary array subscript and the ceiling
// disappears. `Segments` and `Replacement` are alternatives: exactly one
// of them describes the value.
//
// The same decomposition also carries a `$name` reference that binds to
// several like-named capture groups, which no `replaceRegexpOne`
// substitution string can express either — the emitter selects among
// their subscripts with `arrayFirst`. See [LabelReplaceSegment.Fallbacks].
type LabelReplace struct {
	Map              Expr
	Dst              string
	Replacement      string
	Src              string
	Regex            string
	EmptyReplacement string
	// ProbedRegex, when non-empty, is the pattern the emitter must feed
	// `extractGroups` in place of Regex: the same pattern with synthetic
	// capture groups added so that a group whose own capture cannot report
	// whether it TOOK PART in the match has a neighbour that can. It
	// matches exactly the strings Regex matches — the added groups only
	// observe — but it numbers capture groups differently, so every index
	// in Segments is in ITS numbering, not Regex's. Regex still drives the
	// `match(...)` test, where the numbering is immaterial.
	ProbedRegex string
	// Segments, when non-empty, replaces Replacement as the description
	// of the substituted value. See LabelReplaceSegment.
	Segments []LabelReplaceSegment
}

// NoCaptureGroup marks a [LabelReplaceSegment] that carries literal text
// rather than a capture-group reference.
const NoCaptureGroup = -1

// WholeMatchGroup is the capture-group index of the whole match — Go's
// `$0`. The emitter reads it off the source value directly rather than
// from `extractGroups`, which numbers from the first real group: the
// regex is anchored, so a match spans the entire source string.
const WholeMatchGroup = 0

// LabelReplaceSegment is one piece of a decomposed replacement template:
// a run of literal text, a reference to one capture group, or a reference
// to a NAME that several capture groups share.
type LabelReplaceSegment struct {
	// Literal is the text this segment contributes when Group is
	// [NoCaptureGroup].
	Literal string
	// Fallbacks holds the further capture-group indices the search may
	// look through when a `$name` reference binds to several groups; it is
	// empty for every other segment shape. The segment contributes the
	// first of Group followed by Fallbacks whose capture is non-empty,
	// which the emitter renders as
	//
	//	arrayFirst(x -> x != '', [<group>, <fallback>, …])
	//
	// Go's `ExpandString` picks the first of the like-named groups that
	// TOOK PART in the match, and ClickHouse cannot see participation
	// directly. Group plus Fallbacks is therefore a PREFIX of the groups
	// carrying the name rather than all of them: the lowering truncates
	// the list at the first carrier every match must pass through, since
	// Go's own scan can never walk past that one either. It populates the
	// list only once it has established that no match can pair a first
	// participant capturing the empty string with a later listed group
	// capturing text — see qlcommon's captureGroups.expressibleCarriers.
	Fallbacks []int
	// Probes holds, for each searched carrier — Group followed by
	// Fallbacks, one element apiece and in that order — the capture group
	// whose NON-EMPTINESS reports that the carrier took part in the match.
	// It is empty when every carrier's own capture reports that, which is
	// the case whenever no carrier is nullable-and-skippable; the emitter
	// then searches the carriers themselves. When it is populated the
	// indices refer to [LabelReplace.ProbedRegex], whose synthetic groups
	// exist precisely to be read here.
	Probes []int
	// Group is the capture-group index to substitute, or [NoCaptureGroup]
	// when this segment is literal text.
	Group int
}

// Equal reports whether two segments describe the same contribution.
func (s LabelReplaceSegment) Equal(o LabelReplaceSegment) bool {
	if len(s.Probes) != len(o.Probes) {
		return false
	}
	for i := range s.Probes {
		if s.Probes[i] != o.Probes[i] {
			return false
		}
	}
	if len(s.Fallbacks) != len(o.Fallbacks) {
		return false
	}
	for i := range s.Fallbacks {
		if s.Fallbacks[i] != o.Fallbacks[i] {
			return false
		}
	}
	return s.Literal == o.Literal && s.Group == o.Group
}

func (*LabelReplace) exprNode() {}

func (l *LabelReplace) Equal(other Expr) bool {
	o, ok := other.(*LabelReplace)
	if !ok {
		return false
	}
	if len(l.Segments) != len(o.Segments) {
		return false
	}
	for i := range l.Segments {
		if !l.Segments[i].Equal(o.Segments[i]) {
			return false
		}
	}
	return l.Dst == o.Dst &&
		l.Replacement == o.Replacement &&
		l.Src == o.Src &&
		l.Regex == o.Regex &&
		l.ProbedRegex == o.ProbedRegex &&
		l.EmptyReplacement == o.EmptyReplacement &&
		l.Map.Equal(o.Map)
}
