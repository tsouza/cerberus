package chsql

import (
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unicode"
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

// TestSubstituteFor_ReadsSpellingsAsReplacement is the property the
// restore relies on: over any text, the returned pattern finds, in the
// text's substitute form, exactly the matches and submatches the original
// finds in its U+FFFD form, rune for rune — under each reading of `.` the
// pattern was rewritten for, and on text holding genuine characters of
// the block picked, its stand-in and U+FFFD next to invalid bytes.
func TestSubstituteFor_ReadsSpellingsAsReplacement(t *testing.T) {
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
		{`[\x{10FF80}-\x{10FFFF}]`, false},
		{`\x{10FFAA}.`, false},
		{`(?i:\x{FFFD}x)`, true},
		{`[\x{0}-\x{FFFF}]+`, true},
		{`\x{FFFD}.(?s:.)(?-s:.)`, true},
		{`[\x{F0000}-\x{10FFFF}]`, true},
	}
	type unit struct{ fffd, sub string }
	for _, eitherDot := range []bool{false, true} {
		for _, p := range patterns {
			k, rewritten, ok := substituteFor([]string{p.pattern}, eitherDot)
			if !ok {
				t.Fatalf("substituteFor(%q, eitherDot %v) did not serve it", p.pattern, eitherDot)
			}
			if (rewritten[0] != p.pattern) != p.rewritten {
				t.Fatalf("substituteFor(%q, eitherDot %v) = %q; want a rewrite: %v", p.pattern, eitherDot, rewritten[0], p.rewritten)
			}
			// Each input rune as the U+FFFD form and the substitute form
			// hold it: an invalid byte (U+FFFD vs its spelling), a genuine
			// block character (itself vs the stand-in), or any other
			// genuine character (itself in both).
			invalid := func(b byte) unit { return unit{fffd: string(utf8.RuneError), sub: string(k.base + rune(b))} }
			inBlock := func(r rune) unit { return unit{fffd: string(r), sub: string(k.base)} }
			genuine := func(s string) unit { return unit{fffd: s, sub: s} }
			alphabet := []unit{
				genuine("a"), genuine("-"), genuine("\n"), genuine("é"), genuine("\uFFFD"), genuine(string(k.base)),
				inBlock(k.first()), inBlock(k.last()), invalid(0xFF), invalid(0x80),
			}
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
			for _, dotNL := range dotReadings(eitherDot) {
				flag := "(?-s)"
				if dotNL {
					flag = "(?s)"
				}
				orig := regexp.MustCompile(flag + p.pattern)
				sub := regexp.MustCompile(flag + rewritten[0])
				for _, in := range inputs {
					var fffd, subs strings.Builder
					for _, u := range in {
						fffd.WriteString(u.fffd)
						subs.WriteString(u.sub)
					}
					want := runeSpans(fffd.String(), orig.FindAllStringSubmatchIndex(fffd.String(), -1))
					got := runeSpans(subs.String(), sub.FindAllStringSubmatchIndex(subs.String(), -1))
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s%q (rewritten %q) on %q: submatch runes %v; the U+FFFD form gives %v", flag, p.pattern, rewritten[0], subs.String(), got, want)
					}
				}
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
		// A nullable pattern (`[^0-9]*`, matching the empty string) is no
		// longer reachable here: rejectNullablePattern rejects every-match
		// functions with a nullable pattern before SQL emission (#3726),
		// so there is no invalid-UTF8 guard behavior left to assert for it.
		{"fn-groups", &chplan.FuncCall{Fn: chplan.FnRegexExtractAllGroupsHorizontal, Args: []chplan.Expr{v, &chplan.LitString{V: "(.)(.)"}}}, true, true},
		{"fn-replaceOne", &chplan.FuncCall{Fn: chplan.FnRegexReplaceFirst, Args: []chplan.Expr{v, &chplan.LitString{V: "[^a]"}, &chplan.LitString{V: "_"}}}, true, true},
		{"fn-replaceAll-plain", &chplan.FuncCall{Fn: chplan.FnRegexReplaceAll, Args: []chplan.Expr{v, &chplan.LitString{V: "^[+-]"}, &chplan.LitString{V: ""}}}, false, false},
		{"fn-replaceAll-dot-version-dependent", &chplan.FuncCall{Fn: chplan.FnRegexReplaceAll, Args: []chplan.Expr{v, &chplan.LitString{V: `\x{FFFD}.`}, &chplan.LitString{V: "_"}}}, true, true},
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

// TestLabelReplace_RestoresThroughRewrite pins that a regex singling out
// U+FFFD restores invalid bytes through a rewritten pattern, with no
// fallback to the U+FFFD form for values holding block characters.
func TestLabelReplace_RestoresThroughRewrite(t *testing.T) {
	t.Parallel()
	for _, regex := range []string{`(\x{FFFD})(.*)`, `(.)(.*)`, `(\p{Co}*)(.*)`} {
		sql, _ := renderExpr(t, labelReplaceOf(regex))
		if !strings.Contains(sql, "utf8_sub") {
			t.Errorf("label_replace %q does not restore invalid bytes:\n%s", regex, sql)
		}
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

// TestSubstituteFor_PicksUniformBlock pins which block serves a pattern:
// the highest whose span the pattern reads alike, and whether the pattern
// is rewritten there.
func TestSubstituteFor_PicksUniformBlock(t *testing.T) {
	t.Parallel()
	const top, next = substituteBaseLast, substituteBaseLast - substituteBlockSpan
	cases := []struct {
		pattern   string
		base      rune
		rewritten bool
	}{
		{`[^a]`, top, false},
		{`.`, top, false},
		{`a\x{FFFD}`, top, true},
		{`\x{10FF00}`, next, false},
		{`\x{10FFFF}`, next, false},
		{`\x{10FEFF}`, top, false},
		{`\p{Co}`, next, true},
		{`[\x{FFFD}\x{F0000}-\x{10FFFF}]`, top, false},
		{`[\x{FFFD}\x{10FF00}-\x{10FFFF}]`, top, false},
		{`[\x{10FF00}-\x{10FF7F}]`, next, false},
		{`[^\x{10FF00}-\x{10FFFF}]`, top, true},
	}
	for _, c := range cases {
		k, got, ok := substituteFor([]string{c.pattern}, false)
		if !ok || k.base != c.base || (got[0] != c.pattern) != c.rewritten {
			t.Errorf("substituteFor(%q) = %#x, %q, ok %v; want base %#x, rewritten: %v", c.pattern, k.base, got, ok, c.base, c.rewritten)
		}
	}
	// A block serves every pattern of a call: the one that leaves the top
	// block moves them all.
	k, _, ok := substituteFor([]string{`[^a]`, `\x{10FF81}`}, false)
	if !ok || k.base != next {
		t.Errorf("substituteFor of two patterns picked %#x, ok %v; want %#x", k.base, ok, next)
	}
}

// TestSubstituteFor_KeepsBareDotOpen pins that a pattern rewritten for
// both readings of `.` spells a bare `.` as itself, so ClickHouse reads it
// as it reads the original, while `.` with an explicit flag keeps it.
func TestSubstituteFor_KeepsBareDotOpen(t *testing.T) {
	t.Parallel()
	const fffdOrBlock = "[\uFFFD\\x{10ff80}-\\x{10ffff}]"
	cases := map[string]string{
		`\x{FFFD}.`:         fffdOrBlock + ".",
		`\x{FFFD}.*(?s:.)`:  fffdOrBlock + ".*(?s:.)",
		`(\x{FFFD})(.)|x.y`: "(" + fffdOrBlock + ")(.)|x.y",
		// The code points just below the block the pattern is served by
		// are its own to name: none of them may serve as a mark.
		`\x{FFFD}.(?s:.)(?-s:.)\x{10FEFF}\x{10FEFE}\x{10FEFD}`: fffdOrBlock + `.(?s:.)(?-s:.)\x{10feff}\x{10fefe}\x{10fefd}`,
	}
	for pattern, want := range cases {
		_, got, ok := substituteFor([]string{pattern}, true)
		if !ok || got[0] != want {
			t.Errorf("substituteFor(%q) = %q, ok %v; want %q", pattern, got, ok, want)
		}
	}
}

// TestLiteralWithSubstitutes pins how a rewritten literal splits around
// U+FFFD.
func TestLiteralWithSubstitutes(t *testing.T) {
	t.Parallel()
	// Go's regexp/syntax prints U+FFFD as itself and code points above the
	// BMP in lower-case hex.
	const fffdOrBlock = "[\uFFFD\\x{10ff80}-\\x{10ffff}]"
	cases := map[string]string{
		`ab\x{FFFD}cd`:      "ab" + fffdOrBlock + "cd",
		`\x{FFFD}`:          fffdOrBlock,
		`x\x{FFFD}\x{FFFD}`: "x" + fffdOrBlock + fffdOrBlock,
	}
	for pattern, want := range cases {
		_, got, ok := substituteFor([]string{pattern}, false)
		if !ok || got[0] != want {
			t.Errorf("substituteFor(%q) = %q; want %q", pattern, got, want)
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
	runes := func(valid, invalid string) string {
		tail := func(from string) string {
			return "arrayStringConcat(arrayMap(utf8_byte -> " + invalid + ", arraySlice(utf8_chunk, " + from + ")))"
		}
		return "arrayStringConcat(arrayMap(utf8_chunk -> if(isValidUTF8(" + head + "), concat(" + valid + ", " + tail(width+" + 1") + "), " + tail("1") + "), " + chunks + "))"
	}
	if got, _ := Render(replacementRunes(BareIdent("v"))); got != runes(head, "'\uFFFD'") {
		t.Errorf("replacementRunes:\n%s\nwant\n%s", got, runes(head, "'\uFFFD'"))
	}
	sub := "char(244, 143, bitOr(188, bitShiftRight(reinterpretAsUInt8(utf8_byte), 6)), bitOr(128, bitAnd(reinterpretAsUInt8(utf8_byte), 63)))"
	standIn := "if(" + head + " >= '\U0010FF80' AND " + head + " <= '\U0010FFFF', '\U0010FF00', " + head + ")"
	if got, _ := Render(substituteRunes(BareIdent("v"), substituteBlock{base: substituteBaseLast})); got != runes(standIn, sub) {
		t.Errorf("substituteRunes:\n%s\nwant\n%s", got, runes(standIn, sub))
	}
	strs := func(x string) string {
		return "arrayMap(utf8_chunk -> arrayStringConcat(utf8_chunk), arraySplit(utf8_byte -> bitAnd(reinterpretAsUInt8(utf8_byte), 192) != 128, splitByString('', " + x + ")))"
	}
	want := "arrayStringConcat(arrayMap((utf8_sub, utf8_rune) -> if(utf8_sub = utf8_rune OR utf8_rune != '\uFFFD', utf8_rune, concat(char(bitOr(bitShiftLeft(bitAnd(reinterpretAsUInt8(substring(utf8_sub, 3, 1)), 3), 6), bitAnd(reinterpretAsUInt8(substring(utf8_sub, 4, 1)), 63))), substring(utf8_sub, 5))), " + strs("s") + ", " + strs("f") + "))"
	if got, _ := Render(restoreInvalidBytes(BareIdent("s"), BareIdent("f"))); got != want {
		t.Errorf("restoreInvalidBytes:\n%s\nwant\n%s", got, want)
	}
}

// TestSubstituteFor_ServesEveryPatternOfACall pins that a pattern served
// unchanged does not end the call: every later pattern is still rewritten.
func TestSubstituteFor_ServesEveryPatternOfACall(t *testing.T) {
	t.Parallel()
	const fffdOrBlock = "[\uFFFD\\x{10ff80}-\\x{10ffff}]"
	_, got, ok := substituteFor([]string{`[^a]`, `\x{FFFD}`}, false)
	if want := []string{`[^a]`, fffdOrBlock}; !ok || !slices.Equal(got, want) {
		t.Errorf("substituteFor([^a], \\x{FFFD}) = %q, ok %v; want %q", got, ok, want)
	}
}

// TestSubstituteFor_ReachesTheLowestBlock pins the bottom of the search:
// the lowest block of plane 15 serves a pattern that names a code point of
// every block above it, and nothing serves one that names its own too.
func TestSubstituteFor_ReachesTheLowestBlock(t *testing.T) {
	t.Parallel()
	var above strings.Builder
	for base := substituteBaseFirst + substituteBlockSpan; base <= substituteBaseLast; base += substituteBlockSpan {
		above.WriteString(regexp.QuoteMeta(string(base + 1)))
	}
	k, _, ok := substituteFor([]string{above.String()}, false)
	if !ok || k.base != substituteBaseFirst {
		t.Errorf("substituteFor(a code point of every block above the lowest) = %#x, ok %v; want %#x", k.base, ok, substituteBaseFirst)
	}
	every := above.String() + regexp.QuoteMeta(string(substituteBaseFirst+1))
	if k, _, ok := substituteFor([]string{every}, false); ok {
		t.Errorf("substituteFor(a code point of every block) = %#x, ok; want no block", k.base)
	}
}

// TestWithSubstituteBlock pins the class arithmetic on ranges that meet the
// block at either end, as well as ranges around and away from it.
func TestWithSubstituteBlock(t *testing.T) {
	t.Parallel()
	k := substituteBlock{base: substituteBaseFirst}
	first, last := k.first(), k.last()
	cases := []struct {
		name    string
		ranges  []rune
		include bool
		want    []rune
	}{
		{"away from the block", []rune{'a', 'z'}, false, []rune{'a', 'z'}},
		{"from the block's first code point", []rune{first, first + 5}, false, nil},
		{"to the block's last code point", []rune{last - 5, last}, false, nil},
		{"up to the block's last code point", []rune{'a', last}, false, []rune{'a', first - 1}},
		{"above the block", []rune{last + 1, last + 5}, false, []rune{last + 1, last + 5}},
		{"around the block", []rune{'a', unicode.MaxRune}, false, []rune{'a', first - 1, last + 1, unicode.MaxRune}},
		{"added back", []rune{utf8.RuneError, utf8.RuneError}, true, []rune{utf8.RuneError, utf8.RuneError, first, last}},
	}
	for _, c := range cases {
		if got := withSubstituteBlock(c.ranges, k, c.include); !slices.Equal(got, c.want) {
			t.Errorf("%s: withSubstituteBlock(%#x, include %v) = %#x; want %#x", c.name, c.ranges, c.include, got, c.want)
		}
	}
}

// TestDotMarksPlace_RejectsParsesOfDifferentShape pins place's refusal of
// two trees that differ anywhere but at a `.`.
func TestDotMarksPlace_RejectsParsesOfDifferentShape(t *testing.T) {
	t.Parallel()
	lit := func(r rune) *syntax.Regexp { return &syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{r}} }
	marks := dotMarks{bare: 1, anyChar: 2, notNL: 3}
	cases := []struct {
		name         string
		dotNL, plain *syntax.Regexp
	}{
		{"another op", lit('a'), &syntax.Regexp{Op: syntax.OpCharClass, Rune: []rune{'a', 'a'}}},
		{"another arity", &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{lit('a'), lit('b')}}, &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{lit('a')}}},
	}
	for _, c := range cases {
		var placed [3]int
		if marks.place(c.dotNL, c.plain, &placed) {
			t.Errorf("%s: place accepted %v against %v", c.name, c.dotNL, c.plain)
		}
	}
}
