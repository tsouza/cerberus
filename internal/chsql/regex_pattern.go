package chsql

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file renders the pattern strings bound as the regular-expression
// argument of match / extract / replaceRegexpOne / replaceRegexpAll. The
// patterns are query parameters, never SQL tokens; what they must get right
// is ClickHouse's evaluation of them.
//
// ClickHouse builds RE2 for match, extract and extractAll with `.` matching
// a newline (RE2's DOT_NL option). replaceRegexpOne and replaceRegexpAll
// do the same on 26.7 and 26.8 but not on 24.8, the supported floor. A
// pattern whose meaning depends on whether `.` matches `\n` therefore
// carries that choice in its own flags rather than relying on the
// function's defaults.
//
// ClickHouse 26.7 also compiles a subset of patterns to native code
// (`compile_regular_expressions`). The subset accepts leading inline flags
// such as `(?s)` but not a scoped flag group such as `(?s:...)`, so of two
// spellings of one pattern the emitter prefers the one written with leading
// flags.

// goRegexpFlags are the flags regexp.Compile parses with — the flags the
// Prometheus and Loki reference engines read a pattern under.
const goRegexpFlags = syntax.Perl

// anchorLabelReplaceRegex anchors a `label_replace` regex to a full-string
// match with `.` matching a newline, as reference Prometheus does
// (`promql/functions.go`: `"^(?s:" + regexStr + ")$"`).
//
// When regex parses on its own it is written `(?s)^(?:regex)$`. Its
// parentheses then balance, so the non-capturing group encloses exactly
// regex, and the leading `(?s)` sets for the whole pattern the flag the
// reference form sets for the group; `^` and `$` read no flag that `s`
// changes. The two spellings parse to the same program, and only the first
// is in ClickHouse's compilable subset. A regex that does not parse on its
// own — one whose `)` closes the reference form's group early, such as
// `a)|(b` — keeps the reference spelling byte for byte, since there `(?s)`
// would extend to text the reference group leaves outside it; so does one
// that parses alone but not once anchored (an unterminated `\Q` quoting the
// closing `)$`), which both spellings leave invalid.
//
// The `internal/qlcommon` capture-group resolver anchors with the reference
// spelling. Both spellings add no capture group, so the group indices it
// reads off l.Segments number the groups of the pattern emitted here.
// `internal/chsql` may not import `internal/qlcommon` (`.go-arch-lint.yml`);
// see [qlcommon.anchorRegex]'s doc comment for what a bare `^...$` gets
// wrong (alternation escaping the anchors, `.` not matching newline).
func anchorLabelReplaceRegex(regex string) string {
	leading := "(?s)^(?:" + regex + ")$"
	if parses(regex) && parses(leading) {
		return leading
	}
	return "^(?s:" + regex + ")$"
}

// parses reports whether Go's regexp accepts pattern.
func parses(pattern string) bool {
	_, err := syntax.Parse(pattern, goRegexpFlags)
	return err == nil
}

// lineFilterRegex renders a LogQL `|~` / `!~` pattern for match(). Loki
// evaluates a line filter with Go's regexp under its default flags, where
// `.` does not match a newline; match() evaluates with `.` matching one.
//
//   - A pattern with no `.` read under those defaults — the common case —
//     means the same under both engines and is returned unchanged.
//   - Otherwise each such `.` is spelled `[^\n]`, which matches under
//     either flag exactly what Go's `.` matches. The rewrite is kept only
//     when the result, read with `.` matching a newline, parses to the
//     program the pattern parses to under Go's defaults; a `[^\n]` run
//     under `*` or `+` is in ClickHouse's compilable subset, which the
//     alternative below is not.
//   - Failing that, the pattern is prefixed with `(?-s)`, which restores
//     Go's reading for the whole pattern while leaving any flag the pattern
//     sets itself in force.
//
// A pattern Go cannot parse is returned unchanged, for ClickHouse to reject
// as it would have.
func lineFilterRegex(pattern string) string {
	re, err := syntax.Parse(pattern, goRegexpFlags)
	if err != nil || !readsDotWithoutNewline(re) {
		return pattern
	}
	if spelled, ok := spellDotsAsNotNewline(pattern); ok {
		if got, err := syntax.Parse(spelled, goRegexpFlags|syntax.DotNL); err == nil && programOf(got) == programOf(re) {
			return spelled
		}
	}
	return "(?-s)" + pattern
}

// programOf prints re simplified, with every `.` written as the class it
// matches, so two parses print alike exactly when they match the same
// strings with the same submatches.
func programOf(re *syntax.Regexp) string {
	return dotsAsClasses(re.Simplify()).String()
}

// dotsAsClasses rewrites re's `.` nodes, in place, as the character
// classes they match: any rune, or any rune but `\n`.
func dotsAsClasses(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpAnyChar:
		re.Op, re.Rune = syntax.OpCharClass, []rune{0, unicode.MaxRune}
	case syntax.OpAnyCharNotNL:
		re.Op, re.Rune = syntax.OpCharClass, []rune{0, '\n' - 1, '\n' + 1, unicode.MaxRune}
	}
	for _, sub := range re.Sub {
		dotsAsClasses(sub)
	}
	return re
}

// readsDotWithoutNewline reports whether re contains a `.` parsed without
// the `s` flag.
func readsDotWithoutNewline(re *syntax.Regexp) bool {
	if re.Op == syntax.OpAnyCharNotNL {
		return true
	}
	return slices.ContainsFunc(re.Sub, readsDotWithoutNewline)
}

// notNewlineClass is `.` without the `s` flag, spelled as a class.
const notNewlineClass = `[^\n]`

// spellDotsAsNotNewline replaces every `.` metacharacter in pattern — one
// outside a character class, an escape and a `\Q...\E` quotation — with
// [notNewlineClass]. It declines (ok false) a pattern that sets or clears
// the `s` flag anywhere, where a `.`'s meaning depends on its position.
// lineFilterRegex checks the result against the parser, so this scanner
// only has to be right for the rewrite to be used, never for it to be
// correct.
func spellDotsAsNotNewline(pattern string) (string, bool) {
	var out []byte
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\' && i+1 < len(pattern) && pattern[i+1] == 'Q':
			end := len(pattern)
			if j := indexFrom(pattern, `\E`, i+2); j >= 0 {
				end = j + len(`\E`)
			}
			out = append(out, pattern[i:end]...)
			i = end - 1
		case c == '\\' && i+1 < len(pattern):
			out = append(out, c, pattern[i+1])
			i++
		case c == '[':
			end := classEnd(pattern, i)
			if end < 0 {
				return "", false
			}
			out = append(out, pattern[i:end]...)
			i = end - 1
		case c == '(' && i+1 < len(pattern) && pattern[i+1] == '?':
			j := i + 2
			for j < len(pattern) && pattern[j] != ')' && pattern[j] != ':' && pattern[j] != '<' && pattern[j] != 'P' {
				if pattern[j] == 's' {
					return "", false
				}
				j++
			}
			out = append(out, pattern[i:j]...)
			i = j - 1
		case c == '.':
			out = append(out, notNewlineClass...)
		default:
			out = append(out, c)
		}
	}
	return string(out), true
}

// classEnd returns the index just past the character class opening at
// pattern[start], or -1 when it does not close. A `]` directly after `[` or
// `[^` is a member, an escape is skipped whole, and a POSIX `[:name:]` is
// skipped to its own `:]`.
func classEnd(pattern string, start int) int {
	i := start + 1
	if i < len(pattern) && pattern[i] == '^' {
		i++
	}
	if i < len(pattern) && pattern[i] == ']' {
		i++
	}
	for i < len(pattern) {
		switch {
		case pattern[i] == '\\':
			i += 2
		case pattern[i] == '[' && i+1 < len(pattern) && pattern[i+1] == ':':
			j := indexFrom(pattern, ":]", i+2)
			if j < 0 {
				return -1
			}
			i = j + len(":]")
		case pattern[i] == ']':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// indexFrom is strings.Index over pattern[from:], offset back into pattern.
// Callers pass a from no greater than len(pattern).
func indexFrom(pattern, sub string, from int) int {
	if i := strings.Index(pattern[from:], sub); i >= 0 {
		return from + i
	}
	return -1
}

// lineFilterLiteralPrefix returns the literal every match of a line-filter
// pattern starts with under Go's regexp (Loki's reading), or "" when there is
// none or the pattern does not parse.
//
// A line that lacks that literal cannot match, so a substring search for it
// is a sound guard ahead of match(). The guard is what keeps a line filter
// cheap on ClickHouse 26.7 and later: RE2 behind match() searches a
// pattern's required literal before it runs the regex, but a pattern the
// server compiles to native code skips that search and runs the compiled
// matcher on every line, which is slower for a literal most lines lack.
func lineFilterLiteralPrefix(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	prefix, _ := re.LiteralPrefix()
	return prefix
}

// allMatchesFns are the ClickHouse functions that report every match of a
// pattern rather than the first. For a pattern that can match the empty
// string they disagree with Go's FindAll / ReplaceAll, which the reference
// engines use: ClickHouse steps one byte past an empty match (splitting a
// multi-byte rune), does not skip an empty match abutting the previous
// match, extractAll drops empty matches altogether, and 24.8 and 26.8
// disagree on the trailing empty match. A first-match function
// (extract, regexpExtract, replaceRegexpOne) starts at offset 0 exactly as
// Go does and is unaffected.
var allMatchesFns = map[chplan.Fn]bool{
	chplan.FnRegexExtractAll:                 true,
	chplan.FnRegexExtractAllGroupsHorizontal: true,
	chplan.FnRegexReplaceAll:                 true,
}

// regexPatternArg is the position of the pattern among an
// [allMatchesFns] or [chplan.FnRegexExtractGroup] function's arguments
// (haystack, pattern, ...).
const regexPatternArg = 1

// ErrNullableAllMatchesPattern rejects an [allMatchesFns] call whose pattern
// can match the empty string, or whose pattern is not a literal the emitter
// can prove cannot.
var ErrNullableAllMatchesPattern = errors.New("chsql: every-match regex function needs a pattern that cannot match the empty string")

// checkAllMatchesPattern refuses to emit an [allMatchesFns] call whose
// answer would diverge from Go's. A lowering that needs only the first
// match emits a first-match function instead.
func checkAllMatchesPattern(f *chplan.FuncCall) error {
	if !allMatchesFns[f.Fn] {
		return nil
	}
	var pattern string
	if len(f.Args) > regexPatternArg {
		switch p := f.Args[regexPatternArg].(type) {
		case *chplan.LitString:
			pattern = p.V
		case *chplan.InlineString:
			pattern = p.V
		default:
			return fmt.Errorf("%w: %s pattern is %T, not a literal", ErrNullableAllMatchesPattern, f.Fn, p)
		}
	} else {
		return fmt.Errorf("%w: %s has no pattern argument", ErrNullableAllMatchesPattern, f.Fn)
	}
	nullable, err := matchesEmpty(pattern)
	if err != nil {
		return fmt.Errorf("%w: %s pattern %q: %w", ErrNullableAllMatchesPattern, f.Fn, pattern, err)
	}
	if nullable {
		return fmt.Errorf("%w: %s pattern %q", ErrNullableAllMatchesPattern, f.Fn, pattern)
	}
	return nil
}

// matchesEmpty reports whether pattern can match the empty string at some
// position of some input: whether its program reaches a match without
// consuming a rune. Empty-width assertions (`^`, `\b`, ...) are treated as
// satisfiable, which over-approximates and so never calls a nullable
// pattern safe.
func matchesEmpty(pattern string) (bool, error) {
	re, err := syntax.Parse(pattern, goRegexpFlags)
	if err != nil {
		return false, err
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return false, err
	}
	seen := make([]bool, len(prog.Inst))
	var reach func(pc uint32) bool
	reach = func(pc uint32) bool {
		if seen[pc] {
			return false
		}
		seen[pc] = true
		inst := prog.Inst[pc]
		switch inst.Op {
		case syntax.InstMatch:
			return true
		case syntax.InstAlt, syntax.InstAltMatch:
			return reach(inst.Out) || reach(inst.Arg)
		case syntax.InstCapture, syntax.InstEmptyWidth, syntax.InstNop:
			return reach(inst.Out)
		default:
			return false
		}
	}
	start, err := boundedProgIndex(prog.Start, len(prog.Inst))
	if err != nil {
		return false, err
	}
	return reach(start), nil
}

// boundedProgIndex converts a [syntax.Prog] instruction index (documented
// non-negative and less than instCount, the same bound reach's own seen[pc]
// check relies on) to the uint32 [syntax.Inst] indexing expects. Routed
// through a real range check rather than a bare conversion so gosec's G115
// int->uint32 rule has something other than the cast itself to see —
// unlike a #nosec comment (unused anywhere else in this tree), the checked
// error path is real, not just silenced.
func boundedProgIndex(n, instCount int) (uint32, error) {
	if n < 0 || n >= instCount {
		return 0, fmt.Errorf("regexp/syntax: program index %d out of range [0, %d)", n, instCount)
	}
	return uint32(n), nil
}

// withGoDefaultFlagsPattern returns f with its pattern respelled by
// [lineFilterRegex] when f is a [chplan.FnRegexExtractGroup] call, whose
// contract is Go's FindStringSubmatch under Go's default flags — where `.`
// does not match a newline, while regexpExtract's does. The respelling adds
// no capture group, so the group index still names the same group. Any
// other call is returned as is.
func withGoDefaultFlagsPattern(f *chplan.FuncCall) *chplan.FuncCall {
	if f.Fn != chplan.FnRegexExtractGroup || len(f.Args) <= regexPatternArg {
		return f
	}
	lit, ok := f.Args[regexPatternArg].(*chplan.LitString)
	if !ok {
		return f
	}
	respelled := lineFilterRegex(lit.V)
	if respelled == lit.V {
		return f
	}
	args := slices.Clone(f.Args)
	args[regexPatternArg] = &chplan.LitString{V: respelled}
	return &chplan.FuncCall{Fn: f.Fn, Args: args}
}
