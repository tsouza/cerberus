package chsql

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tsouza/cerberus/internal/chplan"
)

// guardedMatchSQL is the SQL [goRegexMatch] renders for a pattern an
// invalid UTF-8 byte can take part in.
func guardedMatchSQL(subject, pattern string) string {
	fffd, _ := Render(replacementRunes(verbatim(subject)))
	return "if(isValidUTF8(" + subject + "), match(" + subject + ", " + pattern + "), match(" + fffd + ", " + pattern + "))"
}

func TestRegexReadsInvalidUTF8(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		want    bool
	}{
		{`api-[0-9]+`, false},
		{`^(?:api|web)$`, false},
		{`(?i)kelvin`, false},
		{`\w+\d*`, false},
		{`[a-zé]+`, false},
		{`.`, true},
		{`(?s:.)`, true},
		{`api-.*`, true},
		{`[^-]+`, true},
		{`\S`, true},
		{`\W`, true},
		{`\D`, true},
		{`\x{FFFD}`, true},
		{`[\x{FFF0}-\x{FFFF}]`, true},
		{`\pS`, true},
		{`[\x{FFFD}-\x{FFFF}]`, true},
		{`[\x{FFF0}-\x{FFFD}]`, true},
		{`[\x{FFFE}-\x{FFFF}]`, false},
		{`[\x{D800}-\x{DFFF}]`, true},
		{`[\x{DFFF}-\x{E000}]`, true},
		{`[\x{D000}-\x{D800}]`, true},
		{`[\x{E000}-\x{E0FF}]`, false},
		{`\x{D800}`, true},
		{`\x{DFFF}`, true},
		{`\x{D7FF}`, false},
		{`\x{E000}`, false},
		{`\bapi`, true},
		{`\Bapi`, true},
		{`^api$`, false},
		{`(`, true},
	}
	for _, c := range cases {
		if got := regexReadsInvalidUTF8(c.pattern); got != c.want {
			t.Errorf("regexReadsInvalidUTF8(%q) = %v; want %v", c.pattern, got, c.want)
		}
	}
}

func TestNullable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		want    bool
	}{
		{`a`, false},
		{`a*`, true},
		{`a?b`, false},
		{`a+`, false},
		{`(a*)+`, true},
		{`a{0,2}`, true},
		{`a{2}`, false},
		{`a|b*`, true},
		{`a|b`, false},
		{`^`, true},
		{`\b`, true},
		{`[^a]`, false},
		{`(`, true},
	}
	for _, c := range cases {
		if got := nullable(c.pattern); got != c.want {
			t.Errorf("nullable(%q) = %v; want %v", c.pattern, got, c.want)
		}
	}
}

// TestSubstitutePattern_ReadsSubstitutesAsReplacement is the property the
// restore relies on: over any text, the returned pattern finds, in the
// text's substitute form, exactly the matches and submatches the original
// finds in its U+FFFD form, rune for rune.
func TestSubstitutePattern_ReadsSubstitutesAsReplacement(t *testing.T) {
	t.Parallel()
	patterns := []struct {
		pattern   string
		rewritten bool
	}{
		{`(.)(.)`, false},
		{`([^-]*)-(.*)`, false},
		{`[^a-z]+`, false},
		{`(?i)(k).`, false},
		{`\x{FFFD}`, true},
		{`(\x{FFFD}+)(.*)`, true},
		{`a\x{FFFD}b|(.)`, true},
		{`([^\x{FFFD}]*)(.*)`, true},
		{`(\p{Co}*)(.*)`, true},
		{`[\x{10FF80}-\x{10FFFF}]`, true},
		{`\x{10FFAA}.`, true},
		{`(?i:\x{FFFD}x)`, true},
		{`[\x{0}-\x{FFFF}]+`, true},
	}
	// Each input rune is written as the U+FFFD form and the substitute form
	// hold it: an invalid byte (U+FFFD vs its substitute rune) or a genuine
	// character (itself in both). No genuine character is from the
	// substitute block: a rewritten pattern reads one as U+FFFD, which is
	// why the emitter guards a rewrite against values holding one.
	type unit struct{ fffd, sub string }
	invalid := func(b byte) unit {
		return unit{fffd: string(utf8.RuneError), sub: string(substituteBlockBase + rune(b))}
	}
	genuine := func(s string) unit { return unit{fffd: s, sub: s} }
	alphabet := []unit{genuine("a"), genuine("b"), genuine("x"), genuine("-"), genuine("\n"), genuine("é"), genuine("\uFFFD"), genuine("\uE000"), invalid(0xFF), invalid(0x80), invalid(0xC3)}
	var inputs [][]unit
	var grow func(prefix []unit, depth int)
	grow = func(prefix []unit, depth int) {
		inputs = append(inputs, slices.Clone(prefix))
		if depth == 0 {
			return
		}
		for _, u := range alphabet {
			grow(append(prefix, u), depth-1)
		}
	}
	grow(nil, 3)

	for _, p := range patterns {
		rewritten, guard, ok := substitutePattern(p.pattern, true)
		if !ok {
			t.Fatalf("substitutePattern(%q) did not parse", p.pattern)
		}
		if guard != p.rewritten || (rewritten != p.pattern) != p.rewritten {
			t.Fatalf("substitutePattern(%q) = %q, guard %v; want a rewrite: %v", p.pattern, rewritten, guard, p.rewritten)
		}
		orig := regexp.MustCompile("(?s)" + p.pattern)
		sub := regexp.MustCompile("(?s)" + rewritten)
		for _, in := range inputs {
			var fffd, subs strings.Builder
			for _, u := range in {
				fffd.WriteString(u.fffd)
				subs.WriteString(u.sub)
			}
			want := runeSpans(fffd.String(), orig.FindAllStringSubmatchIndex(fffd.String(), -1))
			got := runeSpans(subs.String(), sub.FindAllStringSubmatchIndex(subs.String(), -1))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q (rewritten %q) on %q: submatch runes %v; the U+FFFD form gives %v", p.pattern, rewritten, subs.String(), got, want)
			}
		}
	}
}

// runeSpans converts byte offsets in s to rune offsets, leaving -1 alone.
func runeSpans(s string, spans [][]int) [][]int {
	out := make([][]int, len(spans))
	for i, span := range spans {
		out[i] = make([]int, len(span))
		for j, off := range span {
			if off < 0 {
				out[i][j] = off
				continue
			}
			out[i][j] = utf8.RuneCountInString(s[:off])
		}
	}
	return out
}

func TestGoRegexMatch_GuardsOnlyPatternsThatReadInvalidBytes(t *testing.T) {
	t.Parallel()
	plain, _ := Render(goRegexMatch(Col("v"), InlineLit("^(?:api)$"), false))
	if plain != "match(`v`, '^(?:api)$')" {
		t.Errorf("unguarded match rendered %q", plain)
	}
	guarded, _ := Render(goRegexMatch(Col("v"), InlineLit("^(?:.*)$"), true))
	if want := guardedMatchSQL("`v`", "'^(?:.*)$'"); guarded != want {
		t.Errorf("guarded match rendered\n%s\nwant\n%s", guarded, want)
	}
}

// TestRegexShapes_GuardInvalidUTF8 pins which emitted shapes take the
// invalid-UTF-8 path: every regex call site routes through the guard when
// its pattern reads invalid bytes, and renders as before otherwise.
func TestRegexShapes_GuardInvalidUTF8(t *testing.T) {
	t.Parallel()
	v := &chplan.ColumnRef{Name: "v"}
	cases := []struct {
		name    string
		expr    chplan.Expr
		guarded bool
		restore bool
	}{
		{"matcher-plain", &chplan.Binary{Op: chplan.OpMatch, Left: v, Right: &chplan.LitString{V: "api"}}, false, false},
		{"matcher-dot", &chplan.Binary{Op: chplan.OpNotMatch, Left: v, Right: &chplan.LitString{V: "api.*"}}, true, false},
		{"nested-dot", &chplan.NestedArrayExists{Column: "Events", SubField: "Attributes", Key: "k", Op: chplan.OpMatch, Value: &chplan.LitString{V: "a.b"}}, true, false},
		{"line-filter-plain", &chplan.LineContent{Source: v, Pattern: "timeout", IsRegex: true}, false, false},
		{"line-filter-class", &chplan.LineContent{Source: v, Pattern: "status=[^2]", IsRegex: true}, true, false},
		{"fn-match", &chplan.FuncCall{Fn: chplan.FnRegexMatch, Args: []chplan.Expr{v, &chplan.LitString{V: "[^0-9]"}}}, true, false},
		{"fn-match-plain", &chplan.FuncCall{Fn: chplan.FnRegexMatch, Args: []chplan.Expr{v, &chplan.LitString{V: "^[0-9]+$"}}}, false, false},
		{"fn-match-dynamic", &chplan.FuncCall{Fn: chplan.FnRegexMatch, Args: []chplan.Expr{v, &chplan.ColumnRef{Name: "p"}}}, true, false},
		{"fn-extract", &chplan.FuncCall{Fn: chplan.FnRegexExtractFirst, Args: []chplan.Expr{v, &chplan.LitString{V: "a(.)"}}}, true, true},
		{"fn-extract-dynamic", &chplan.FuncCall{Fn: chplan.FnRegexExtractFirst, Args: []chplan.Expr{v, &chplan.ColumnRef{Name: "p"}}}, false, false},
		{"fn-extractAll", &chplan.FuncCall{Fn: chplan.FnRegexExtractAll, Args: []chplan.Expr{v, &chplan.LitString{V: "[^0-9]+"}}}, true, true},
		{"fn-extractAll-nullable", &chplan.FuncCall{Fn: chplan.FnRegexExtractAll, Args: []chplan.Expr{v, &chplan.LitString{V: "[^0-9]*"}}}, true, false},
		{"fn-groups", &chplan.FuncCall{Fn: chplan.FnRegexExtractAllGroupsHorizontal, Args: []chplan.Expr{v, &chplan.LitString{V: "(.)(.)"}}}, true, true},
		{"fn-replaceOne", &chplan.FuncCall{Fn: chplan.FnRegexReplaceFirst, Args: []chplan.Expr{v, &chplan.LitString{V: "[^a]"}, &chplan.LitString{V: "_"}}}, true, true},
		{"fn-replaceAll-plain", &chplan.FuncCall{Fn: chplan.FnRegexReplaceAll, Args: []chplan.Expr{v, &chplan.LitString{V: "^[+-]"}, &chplan.LitString{V: ""}}}, false, false},
		{"fn-replaceAll-dot-version-dependent", &chplan.FuncCall{Fn: chplan.FnRegexReplaceAll, Args: []chplan.Expr{v, &chplan.LitString{V: `\x{FFFD}.`}, &chplan.LitString{V: "_"}}}, true, false},
		{"label_replace-plain", labelReplaceOf("([a-z]*)"), false, false},
		{"label_replace-dot", labelReplaceOf("(.*)"), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _ := renderExpr(t, c.expr)
			if got := strings.Contains(sql, "isValidUTF8("); got != c.guarded {
				t.Errorf("guarded = %v; want %v:\n%s", got, c.guarded, sql)
			}
			if got := strings.Contains(sql, "utf8_sub"); got != c.restore {
				t.Errorf("restores invalid bytes = %v; want %v:\n%s", got, c.restore, sql)
			}
		})
	}
}

func labelReplaceOf(regex string) *chplan.LabelReplace {
	return &chplan.LabelReplace{Map: &chplan.ColumnRef{Name: "m"}, Dst: "dst", Src: "src", Regex: regex, Replacement: `\1`}
}

// TestLabelReplace_SubstituteBlockGuard pins that a regex singling out
// U+FFFD reads a value holding a genuine substitute-block character on its
// U+FFFD form, since the rewritten pattern reads that character as U+FFFD.
func TestLabelReplace_SubstituteBlockGuard(t *testing.T) {
	t.Parallel()
	sql, args := renderExpr(t, labelReplaceOf(`(\x{FFFD})(.*)`))
	if !slices.Contains(args, any(substituteBlockClass)) || !strings.Contains(sql, "utf8_sub") {
		t.Fatalf("a regex naming U+FFFD must restore through a rewritten pattern behind the substitute-block guard:\n%s\n%v", sql, args)
	}
	sql, args = renderExpr(t, labelReplaceOf(`(.)(.*)`))
	if slices.Contains(args, any(substituteBlockClass)) || !strings.Contains(sql, "utf8_sub") {
		t.Fatalf("a regex reading U+FFFD like the substitute block needs no guard:\n%s\n%v", sql, args)
	}
}

func TestTextIndexLikeTokens_SplitOnReplacementCharacter(t *testing.T) {
	t.Parallel()
	got := textIndexLikeTokens("timeout\uFFFDafter lengthy\uFFFDwait")
	want := []string{"timeout", "after", "lengthy", "wait"}
	if !slices.Equal(got, want) {
		t.Errorf("textIndexLikeTokens = %q; want %q", got, want)
	}
}

// TestSubstitutePattern_BlockEdges pins which patterns read the substitute
// block (U+10FF80..U+10FFFF) as they read U+FFFD — returned unchanged —
// and which are rewritten, at the edges of the block.
func TestSubstitutePattern_BlockEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern   string
		rewritten bool
	}{
		{`\x{10FF7F}`, false},
		{`\x{10FF80}`, true},
		{`\x{10FFFF}`, true},
		{`a\x{FFFD}`, true},
		{`[\x{FFFD}\x{10FF80}-\x{10FFFF}]`, false},
		{`[\x{FFFD}\x{10FF81}-\x{10FFFF}]`, true},
		{`[\x{10FF00}-\x{10FF7F}]`, false},
		{`[\x{10FF00}-\x{10FF80}]`, true},
		{`[^\x{10FF80}-\x{10FFFF}]`, true},
		{`[^a]`, false},
	}
	for _, c := range cases {
		got, guard, ok := substitutePattern(c.pattern, true)
		if !ok || guard != c.rewritten || (got != c.pattern) != c.rewritten {
			t.Errorf("substitutePattern(%q) = %q, guard %v, ok %v; want rewritten: %v", c.pattern, got, guard, ok, c.rewritten)
		}
	}
}

// TestLiteralWithSubstitutes pins how a rewritten literal splits around
// U+FFFD and substitute-block runes.
func TestLiteralWithSubstitutes(t *testing.T) {
	t.Parallel()
	// Go's regexp/syntax prints U+FFFD as itself and code points above the
	// BMP in lower-case hex.
	const fffdOrBlock = "[\uFFFD\\x{10ff80}-\\x{10ffff}]"
	cases := map[string]string{
		`ab\x{FFFD}cd`:      "ab" + fffdOrBlock + "cd",
		`\x{FFFD}`:          fffdOrBlock,
		`\x{10FF90}x`:       `[^\x00-\x{10FFFF}]x`,
		`x\x{FFFD}\x{FFFD}`: "x" + fffdOrBlock + fffdOrBlock,
	}
	for pattern, want := range cases {
		got, _, ok := substitutePattern(pattern, true)
		if !ok || got != want {
			t.Errorf("substitutePattern(%q) = %q; want %q", pattern, got, want)
		}
	}
}

// TestInvalidUTF8Frags pins the SQL of the U+FFFD form, the substitute
// form and the restore; test/regexjit's TestRegexJIT_InvalidUTF8ShapesMatchGo
// is the evidence that ClickHouse evaluates them as Go's decoder reads the
// bytes.
func TestInvalidUTF8Frags(t *testing.T) {
	t.Parallel()
	chunks := "arraySplit(utf8_byte -> bitAnd(reinterpretAsUInt8(utf8_byte), 192) != 128, splitByString('', v))"
	width := "multiIf(reinterpretAsUInt8(utf8_chunk[1]) < 128, 1, reinterpretAsUInt8(utf8_chunk[1]) < 224, 2, reinterpretAsUInt8(utf8_chunk[1]) < 240, 3, 4)"
	head := "arrayStringConcat(arraySlice(utf8_chunk, 1, " + width + "))"
	runes := func(invalid string) string {
		tail := func(from string) string {
			return "arrayStringConcat(arrayMap(utf8_byte -> " + invalid + ", arraySlice(utf8_chunk, " + from + ")))"
		}
		return "arrayStringConcat(arrayMap(utf8_chunk -> if(isValidUTF8(" + head + "), concat(" + head + ", " + tail(width+" + 1") + "), " + tail("1") + "), " + chunks + "))"
	}
	if got, _ := Render(replacementRunes(BareIdent("v"))); got != runes("'\uFFFD'") {
		t.Errorf("replacementRunes:\n%s\nwant\n%s", got, runes("'\uFFFD'"))
	}
	sub := "char(244, 143, bitOr(188, bitShiftRight(reinterpretAsUInt8(utf8_byte), 6)), bitOr(128, bitAnd(reinterpretAsUInt8(utf8_byte), 63)))"
	if got, _ := Render(substituteRunes(BareIdent("v"))); got != runes(sub) {
		t.Errorf("substituteRunes:\n%s\nwant\n%s", got, runes(sub))
	}
	strs := func(x string) string {
		return "arrayMap(utf8_chunk -> arrayStringConcat(utf8_chunk), arraySplit(utf8_byte -> bitAnd(reinterpretAsUInt8(utf8_byte), 192) != 128, splitByString('', " + x + ")))"
	}
	want := "arrayStringConcat(arrayMap((utf8_sub, utf8_rune) -> if(utf8_sub = utf8_rune, utf8_rune, concat(char(bitOr(bitShiftLeft(bitAnd(reinterpretAsUInt8(substring(utf8_sub, 3, 1)), 3), 6), bitAnd(reinterpretAsUInt8(substring(utf8_sub, 4, 1)), 63))), substring(utf8_sub, 5))), " + strs("s") + ", " + strs("f") + "))"
	if got, _ := Render(restoreInvalidBytes(BareIdent("s"), BareIdent("f"))); got != want {
		t.Errorf("restoreInvalidBytes:\n%s\nwant\n%s", got, want)
	}
}
