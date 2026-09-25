package chsql

import (
	"fmt"
	"math"
	"regexp/syntax"
	"unicode/utf8"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file makes the regular expressions ClickHouse evaluates answer as
// Go's regexp — the Prometheus, Loki and Tempo reference engines' — on
// values that are not valid UTF-8.
//
// Go's regexp decodes its input one rune at a time and reads every byte
// that does not begin a valid UTF-8 sequence as U+FFFD, one rune per byte:
// `.`, a negated class such as `[^-]` and a class or literal containing
// U+FFFD all match such a byte. ClickHouse evaluates a pattern with RE2 in
// UTF-8 mode, where nothing matches an invalid byte; ClickHouse 26.7's
// native regex compiler matches bytes instead, but only for the shapes it
// compiles, and only once a pattern has been used often enough.
//
// The emitted shape therefore takes one of two paths per value:
//
//   - a valid UTF-8 value — every value an OTLP writer produces — is
//     evaluated exactly as before, so the compiled and interpreted paths
//     keep the shapes and the speed they had;
//   - a value isValidUTF8 rejects is evaluated on [goRunes] of it: the
//     same text with each invalid byte replaced by U+FFFD, which RE2 and
//     the compiler both read as Go reads the original.
//
// A pattern that no invalid byte can take part in — one with no `.`, no
// class or literal containing U+FFFD and no word-boundary assertion (see
// [regexReadsInvalidUTF8]) — answers alike under every engine, and is
// emitted unguarded.
//
// A function that returns text taken from its input (extract, extractAll,
// extractAllGroupsHorizontal, extractGroups, replaceRegexpOne,
// replaceRegexpAll) must also return the original bytes where Go would,
// not U+FFFD. It is evaluated twice on an invalid value: once on the
// U+FFFD form, and once on [substituteRunes], where each invalid byte
// becomes a distinct code point of the substitute block that encodes the
// byte. The pattern reads both alike (see [substitutePattern]), so the two
// results align rune for rune: they differ exactly where an invalid byte
// was copied into the result, and [restoreInvalidBytes] writes that byte
// back there.

// regexReadsInvalidUTF8 reports whether an invalid UTF-8 byte can change
// what pattern matches — whether Go's regexp and ClickHouse's RE2 can
// answer differently on some input. A pattern Go cannot parse is reported
// as reading invalid bytes, so it keeps the guarded shape.
//
// RE2 reads some invalid sequences as one character where Go reads one
// U+FFFD per byte: an encoded surrogate as that surrogate, an overlong or
// out-of-range sequence as U+FFFD. So an atom that can match U+FFFD, and a
// class holding a surrogate, read invalid bytes. So does a word-boundary
// assertion, which Go tests between runes and RE2 between bytes; every
// other empty-width assertion looks only at the text's ends or at a
// newline, which both engines read alike.
func regexReadsInvalidUTF8(pattern string) bool {
	re, err := syntax.Parse(pattern, goRegexpFlags)
	if err != nil {
		return true
	}
	return readsInvalidByte(re)
}

func readsInvalidByte(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return true
	case syntax.OpCharClass:
		if classContains(re.Rune, utf8.RuneError) {
			return true
		}
		if classOverlaps(re.Rune, surrogateFirst, surrogateLast) {
			return true
		}
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if r == utf8.RuneError || (surrogateFirst <= r && r <= surrogateLast) {
				return true
			}
		}
	}
	for _, sub := range re.Sub {
		if readsInvalidByte(sub) {
			return true
		}
	}
	return false
}

// The UTF-16 surrogate code points, which UTF-8 may not encode.
const (
	surrogateFirst rune = 0xD800
	surrogateLast  rune = 0xDFFF
)

// classContains reports whether the rune ranges of a parsed character
// class (lo, hi pairs) contain r.
func classContains(ranges []rune, r rune) bool {
	for i := 0; i < len(ranges); i += 2 {
		if ranges[i] <= r && r <= ranges[i+1] {
			return true
		}
	}
	return false
}

// The substitute block is the code points substituteBlockBase+b for every
// byte value b that can be invalid in UTF-8 (utf8.RuneSelf..math.MaxUint8):
// U+10FF80..U+10FFFF, the last code points of Supplementary Private Use
// Area-B. Each encodes as four bytes whose last two carry b's top two and
// low six bits, so the byte is recovered with integer arithmetic alone.
const substituteBlockBase rune = 0x10FF00

var (
	substituteBlockFirst = substituteBlockBase + utf8.RuneSelf
	substituteBlockLast  = substituteBlockBase + math.MaxUint8
	// substituteEncoding is the UTF-8 encoding of substituteBlockBase; a
	// substitute rune's encoding shares its first three bytes but for the
	// third's low bits.
	substituteEncoding = []byte(string(substituteBlockBase))
	// substituteBlockClass is a pattern matching any substitute-block
	// character.
	substituteBlockClass = fmt.Sprintf(`[\x{%X}-\x{%X}]`, substituteBlockFirst, substituteBlockLast)
)

// UTF-8 structure the SQL below reads a byte by.
const (
	// utf8ContinuationMask selects the two bits that mark a continuation
	// byte (10xxxxxx) as utf8ContinuationTag.
	utf8ContinuationMask = 0xC0
	utf8ContinuationTag  = 0x80
	// utf8ContinuationBits is the payload a continuation byte carries.
	utf8ContinuationBits = 6
	// utf8ContinuationPayload masks those bits.
	utf8ContinuationPayload = 1<<utf8ContinuationBits - 1
	// invalidByteHighBits masks what is left of a byte once its low
	// utf8ContinuationBits are carried by a continuation byte.
	invalidByteHighBits = math.MaxUint8 >> utf8ContinuationBits
	// utf8ThreeByteLead and utf8FourByteLead are the smallest lead bytes
	// of a three- and a four-byte sequence; a byte below utf8.RuneSelf is
	// a sequence of its own and every other byte below utf8ThreeByteLead
	// leads (or, invalid, pretends to lead) a two-byte one.
	utf8ThreeByteLead = 0xE0
	utf8FourByteLead  = 0xF0
)

// Lambda parameters of the SQL below. They are emitter-chosen, and every
// lambda body reads only its own parameters, so they cannot capture a
// name from the value the SQL is applied to.
const (
	utf8ByteParam  = "utf8_byte"
	utf8ChunkParam = "utf8_chunk"
	utf8SubParam   = "utf8_sub"
	utf8RuneParam  = "utf8_rune"
)

// isValidUTF8 renders ClickHouse's validity test, which rejects exactly
// the byte strings Go's utf8.ValidString rejects (overlong forms,
// surrogates and code points above U+10FFFF included).
func isValidUTF8(s Frag) Frag { return Call("isValidUTF8", s) }

// utf8Chunks renders s as an array of byte arrays, each one a byte that
// is not a continuation byte followed by the continuation bytes after it
// (the first may start with continuation bytes). Go's decoder starts a
// rune at every such byte, so no rune of s spans two chunks.
func utf8Chunks(s Frag) Frag {
	return Call(
		"arraySplit",
		Lambda1(utf8ByteParam, Neq(
			Call("bitAnd", byteValue(BareIdent(utf8ByteParam)), InlineLit(utf8ContinuationMask)),
			InlineLit(utf8ContinuationTag),
		)),
		Call("splitByString", InlineLit(""), s),
	)
}

// byteValue renders the value of a one-byte string.
func byteValue(b Frag) Frag { return Call("reinterpretAsUInt8", b) }

// goRunes renders s with every byte Go's decoder reads as an invalid rune
// replaced by invalid(<that byte>): within each [utf8Chunks] chunk the
// sequence its lead byte announces is kept when it is valid UTF-8, and
// each byte after it — or every byte, when it is not — is invalid, one
// rune per byte, as utf8.DecodeRuneInString reads them.
func goRunes(s Frag, invalid func(b Frag) Frag) Frag {
	chunk := BareIdent(utf8ChunkParam)
	lead := byteValue(Subscript(chunk, InlineLit(1)))
	width := Call(
		"multiIf",
		Lt(lead, InlineLit(utf8.RuneSelf)), InlineLit(1),
		Lt(lead, InlineLit(utf8ThreeByteLead)), InlineLit(2),
		Lt(lead, InlineLit(utf8FourByteLead)), InlineLit(3),
		InlineLit(utf8.UTFMax),
	)
	head := Call("arrayStringConcat", Call("arraySlice", chunk, InlineLit(1), width))
	invalidFrom := func(offset Frag) Frag {
		return Call("arrayStringConcat", Call(
			"arrayMap",
			Lambda1(utf8ByteParam, invalid(BareIdent(utf8ByteParam))),
			Call("arraySlice", chunk, offset),
		))
	}
	return Call("arrayStringConcat", Call(
		"arrayMap",
		Lambda1(utf8ChunkParam, If(
			isValidUTF8(head),
			Call("concat", head, invalidFrom(Add(width, InlineLit(1)))),
			invalidFrom(InlineLit(1)),
		)),
		utf8Chunks(s),
	))
}

// replacementRunes renders s as Go's regexp reads it: each invalid byte
// becomes U+FFFD.
func replacementRunes(s Frag) Frag {
	return goRunes(s, func(Frag) Frag { return InlineLit(string(utf8.RuneError)) })
}

// substituteRunes renders s with each invalid byte b replaced by the
// substitute rune substituteBlockBase+b.
func substituteRunes(s Frag) Frag {
	return goRunes(s, func(b Frag) Frag {
		v := byteValue(b)
		return Call(
			"char",
			InlineLit(int(substituteEncoding[0])),
			InlineLit(int(substituteEncoding[1])),
			Call("bitOr", InlineLit(int(substituteEncoding[2])), Call("bitShiftRight", v, InlineLit(utf8ContinuationBits))),
			Call("bitOr", InlineLit(utf8ContinuationTag), Call("bitAnd", v, InlineLit(utf8ContinuationPayload))),
		)
	})
}

// restoreInvalidBytes renders the result of a text-returning regex
// function evaluated on an invalid value, from its two evaluations: sub
// on [substituteRunes] of the value and fffd on [replacementRunes] of it.
// They align rune for rune and differ only where an invalid byte was
// copied, where sub holds its substitute rune: the byte is written there,
// followed by any continuation bytes the result carried after it, and
// fffd everywhere else.
func restoreInvalidBytes(sub, fffd Frag) Frag {
	return Call("arrayStringConcat", Call(
		"arrayMap",
		Lambda2(utf8SubParam, utf8RuneParam, restoreRune(BareIdent(utf8SubParam), BareIdent(utf8RuneParam))),
		runeStrings(sub),
		runeStrings(fffd),
	))
}

// restoreRune is [restoreInvalidBytes] for one aligned pair of chunks.
func restoreRune(sub, fffd Frag) Frag {
	third := byteValue(Call("substring", sub, InlineLit(utf8.UTFMax-1), InlineLit(1)))
	fourth := byteValue(Call("substring", sub, InlineLit(utf8.UTFMax), InlineLit(1)))
	original := Call(
		"bitOr",
		Call("bitShiftLeft", Call("bitAnd", third, InlineLit(invalidByteHighBits)), InlineLit(utf8ContinuationBits)),
		Call("bitAnd", fourth, InlineLit(utf8ContinuationPayload)),
	)
	return If(Eq(sub, fffd), fffd,
		Call("concat", Call("char", original), Call("substring", sub, InlineLit(utf8.UTFMax+1))))
}

// runeStrings renders s as the array of its [utf8Chunks], each joined
// back into a string.
func runeStrings(s Frag) Frag {
	return Call(
		"arrayMap",
		Lambda1(utf8ChunkParam, Call("arrayStringConcat", BareIdent(utf8ChunkParam))),
		utf8Chunks(s),
	)
}

// restoreInvalidBytesArray is [restoreInvalidBytes] applied element-wise
// to two aligned Array(String) results.
func restoreInvalidBytesArray(sub, fffd Frag) Frag {
	return Call(
		"arrayMap",
		Lambda2(utf8SubParam+"s", utf8RuneParam+"s", restoreInvalidBytes(BareIdent(utf8SubParam+"s"), BareIdent(utf8RuneParam+"s"))),
		sub, fffd,
	)
}

// restoreInvalidBytesNested is [restoreInvalidBytesArray] applied to two
// aligned Array(Array(String)) results.
func restoreInvalidBytesNested(sub, fffd Frag) Frag {
	return Call(
		"arrayMap",
		Lambda2(utf8SubParam+"_groups", utf8RuneParam+"_groups",
			restoreInvalidBytesArray(BareIdent(utf8SubParam+"_groups"), BareIdent(utf8RuneParam+"_groups"))),
		sub, fffd,
	)
}

// goRegexMatch renders `match(subject, pattern)` as Go's regexp answers
// it. readsInvalid is [regexReadsInvalidUTF8] of the pattern, true when
// the pattern is not known when the SQL is emitted.
func goRegexMatch(subject, pattern Frag, readsInvalid bool) Frag {
	if !readsInvalid {
		return Call("match", subject, pattern)
	}
	return If(
		isValidUTF8(subject),
		Call("match", subject, pattern),
		Call("match", replacementRunes(subject), pattern),
	)
}

// substitutePattern returns the pattern a text-returning function reads
// [substituteRunes] with, given the pattern it reads the original with.
//
// When pattern treats every substitute rune as it treats U+FFFD — it names
// neither and has no class holding one but not the other, which is every
// pattern that does not single U+FFFD or private-use characters out — it
// is returned unchanged, with guard false. Otherwise the returned pattern
// is pattern with the substitute block added to every class and literal
// that holds U+FFFD and removed from every other class, rendered from the
// parse flags ClickHouse reads it under (dotNL: `.` matches a newline);
// it reads substitute runes as pattern reads U+FFFD but also reads a
// genuine substitute-block character as U+FFFD, so guard is true: the
// caller must not rely on it for a value holding one. ok is false when
// pattern does not parse.
func substitutePattern(pattern string, dotNL bool) (rewritten string, guard, ok bool) {
	flags := goRegexpFlags
	if dotNL {
		flags |= syntax.DotNL
	}
	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		return "", false, false
	}
	if substituteUniform(re) {
		return pattern, false, true
	}
	return substituteLikeReplacement(re).String(), true, true
}

// substituteUniform reports whether re reads every substitute rune as it
// reads U+FFFD.
func substituteUniform(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if r == utf8.RuneError || inSubstituteBlock(r) {
				return false
			}
		}
	case syntax.OpCharClass:
		fffd := classContains(re.Rune, utf8.RuneError)
		if fffd != classHolds(re.Rune, substituteBlockFirst, substituteBlockLast) ||
			fffd != classOverlaps(re.Rune, substituteBlockFirst, substituteBlockLast) {
			return false
		}
	}
	for _, sub := range re.Sub {
		if !substituteUniform(sub) {
			return false
		}
	}
	return true
}

func inSubstituteBlock(r rune) bool { return substituteBlockFirst <= r && r <= substituteBlockLast }

// classHolds reports whether the ranges of a parsed character class hold
// all of lo..hi. The parser merges adjacent and overlapping ranges, so a
// class holds a contiguous span only inside one of its ranges.
func classHolds(ranges []rune, lo, hi rune) bool {
	for i := 0; i < len(ranges); i += 2 {
		if ranges[i] <= lo && hi <= ranges[i+1] {
			return true
		}
	}
	return false
}

// classOverlaps reports whether the ranges of a parsed character class
// hold any of lo..hi.
func classOverlaps(ranges []rune, lo, hi rune) bool {
	for i := 0; i < len(ranges); i += 2 {
		if ranges[i] <= hi && lo <= ranges[i+1] {
			return true
		}
	}
	return false
}

// substituteLikeReplacement rewrites re in place so that it reads every
// substitute rune as it reads U+FFFD, and returns it.
func substituteLikeReplacement(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpLiteral:
		return literalWithSubstitutes(re)
	case syntax.OpCharClass:
		re.Rune = withSubstituteBlock(re.Rune, classContains(re.Rune, utf8.RuneError))
		return re
	}
	for i, sub := range re.Sub {
		re.Sub[i] = substituteLikeReplacement(sub)
	}
	return re
}

// literalWithSubstitutes splits a literal at each U+FFFD or substitute
// rune into a concatenation in which that rune is a class reading the
// substitute block as U+FFFD — holding both when the rune is U+FFFD, and
// neither's counterpart otherwise.
func literalWithSubstitutes(re *syntax.Regexp) *syntax.Regexp {
	var parts []*syntax.Regexp
	for _, r := range re.Rune {
		if r == utf8.RuneError || inSubstituteBlock(r) {
			parts = append(parts, &syntax.Regexp{
				Op:    syntax.OpCharClass,
				Flags: re.Flags,
				Rune:  withSubstituteBlock([]rune{r, r}, r == utf8.RuneError),
			})
			continue
		}
		if last := len(parts) - 1; last >= 0 && parts[last].Op == syntax.OpLiteral {
			parts[last].Rune = append(parts[last].Rune, r)
			continue
		}
		parts = append(parts, &syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: []rune{r}})
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Flags: re.Flags, Sub: parts}
}

// withSubstituteBlock returns ranges with the substitute block removed,
// and added back whole when include is true.
func withSubstituteBlock(ranges []rune, include bool) []rune {
	var out []rune
	for i := 0; i < len(ranges); i += 2 {
		lo, hi := ranges[i], ranges[i+1]
		if lo < substituteBlockFirst {
			out = append(out, lo, min(hi, substituteBlockFirst-1))
		}
		if hi > substituteBlockLast {
			out = append(out, max(lo, substituteBlockLast+1), hi)
		}
	}
	if include {
		out = append(out, substituteBlockFirst, substituteBlockLast)
	}
	return out
}

// regexResult is how a ClickHouse regex function's result carries text
// of its subject.
type regexResult int

const (
	// regexResultMatch is a match test: no subject text.
	regexResultMatch regexResult = iota
	// regexResultText is one string built from the subject.
	regexResultText
	// regexResultTexts is an Array(String) of such strings.
	regexResultTexts
	// regexResultTextGroups is an Array(Array(String)) of them.
	regexResultTextGroups
)

// regexFn describes a regex function the emitter renders from a
// chplan.FuncCall whose first argument is the subject and second the
// pattern.
type regexFn struct {
	result regexResult
	// dotNL lists how ClickHouse builds may read `.` in the pattern:
	// matching a newline (true) or not. replaceRegexpOne and
	// replaceRegexpAll read it one way on 26.7 and 26.8 and the other on
	// the 24.8 floor.
	dotNL []bool
	// allMatches marks a function that scans for every match. ClickHouse
	// steps one byte past an empty match, where Go steps one rune, so the
	// two evaluations [restoreInvalidBytes] pairs can fall out of
	// alignment for a pattern that can match the empty string.
	allMatches bool
}

var regexFns = map[chplan.Fn]regexFn{
	chplan.FnRegexMatch:                      {result: regexResultMatch, dotNL: []bool{true}},
	chplan.FnRegexExtractFirst:               {result: regexResultText, dotNL: []bool{true}},
	chplan.FnRegexExtractAll:                 {result: regexResultTexts, dotNL: []bool{true}, allMatches: true},
	chplan.FnRegexExtractAllGroupsHorizontal: {result: regexResultTextGroups, dotNL: []bool{true}, allMatches: true},
	chplan.FnRegexReplaceFirst:               {result: regexResultText, dotNL: []bool{true, false}},
	chplan.FnRegexReplaceAll:                 {result: regexResultText, dotNL: []bool{true, false}, allMatches: true},
}

// goRegexCall renders a regex function call so that a subject that is
// not valid UTF-8 is read as Go's regexp reads it, or reports false when
// the call needs no such treatment and renders as written: its pattern
// cannot read an invalid byte ([regexReadsInvalidUTF8]), or, for a
// function returning subject text, the pattern is not a literal.
func goRegexCall(name string, spec regexFn, args []chplan.Expr) (Frag, bool) {
	if len(args) < 2 {
		return nil, false
	}
	frags := make([]Frag, len(args))
	for i, a := range args {
		frags[i] = func(b *Builder) { _ = b.Expr(a) }
	}
	subject := frags[0]
	lit, literal := args[1].(*chplan.LitString)
	if spec.result == regexResultMatch {
		if literal && !regexReadsInvalidUTF8(lit.V) {
			return nil, false
		}
		return goRegexMatch(subject, frags[1], true), true
	}
	if !literal || !regexReadsInvalidUTF8(lit.V) {
		return nil, false
	}
	call := func(s Frag, pattern string) Frag {
		return Call(name, append([]Frag{s, Lit(pattern)}, frags[2:]...)...)
	}
	fffd := call(replacementRunes(subject), lit.V)
	invalid := fffd
	if sub, guard, ok := substitutePatternUnder(lit.V, spec.dotNL); ok && (!spec.allMatches || !nullable(lit.V)) {
		invalid = restoreByResult(spec.result, call(substituteRunes(subject), sub), fffd)
		if guard {
			invalid = If(Call("match", subject, Lit(substituteBlockClass)), fffd, invalid)
		}
	}
	return If(isValidUTF8(subject), call(subject, lit.V), invalid), true
}

// restoreByResult applies [restoreInvalidBytes] at the depth result
// nests strings.
func restoreByResult(result regexResult, sub, fffd Frag) Frag {
	switch result {
	case regexResultTexts:
		return restoreInvalidBytesArray(sub, fffd)
	case regexResultTextGroups:
		return restoreInvalidBytesNested(sub, fffd)
	}
	return restoreInvalidBytes(sub, fffd)
}

// substitutePatternUnder is [substitutePattern] for a pattern that may be
// read under each of readings, of which there is at least one; ok is
// false unless every reading yields the same pattern.
func substitutePatternUnder(pattern string, readings []bool) (rewritten string, guard, ok bool) {
	for i, dotNL := range readings {
		r, g, parsed := substitutePattern(pattern, dotNL)
		if !parsed || (i > 0 && r != rewritten) {
			return "", false, false
		}
		rewritten, guard = r, g
	}
	return rewritten, guard, true
}

// nullable reports whether pattern can match the empty string somewhere,
// assertions aside; an unparseable pattern is reported as nullable.
func nullable(pattern string) bool {
	re, err := syntax.Parse(pattern, goRegexpFlags)
	if err != nil {
		return true
	}
	return matchesEmpty(re)
}

func matchesEmpty(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral, syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL, syntax.OpNoMatch:
		return false
	case syntax.OpStar, syntax.OpQuest:
		return true
	case syntax.OpRepeat:
		return re.Min == 0 || matchesEmpty(re.Sub[0])
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if !matchesEmpty(sub) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if matchesEmpty(sub) {
				return true
			}
		}
		return false
	case syntax.OpCapture, syntax.OpPlus:
		return matchesEmpty(re.Sub[0])
	}
	// Empty-width assertions and the empty match.
	return true
}
