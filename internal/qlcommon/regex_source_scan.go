package qlcommon

import "strings"

// This file answers "which bytes" about a regex source, and nothing about
// what those bytes mean.
//
// `regexp/syntax` records no byte offsets — its nodes cannot say where in
// the source they came from — so a rewrite that has to insert text at a
// particular structural position cannot get the position from the parse
// tree. The alternative, mutating the tree and serialising it back with
// `Regexp.String()`, would rest a user's query semantics on that
// printer's round-tripping. Scanning the source instead keeps the rewrite
// to an insertion into the user's own text, and leaves every question of
// MEANING to `regexp/syntax`, which [probeSpanFor] asks by handing back
// the substring a span names.
//
// The scanner is deliberately partial. It recognises the group syntax Go
// accepts and nothing else, and every construct it cannot classify makes
// it decline outright rather than guess an offset. Declining costs the
// caller a rewrite it might have earned; a wrong offset would bracket the
// wrong span and answer a different question than the one asked.

// sourceGroup is one parenthesised group of a regex source.
type sourceGroup struct {
	// open is the offset of the group's `(`, or -1 for the virtual group
	// standing for the whole pattern.
	open int
	// bodyStart is the offset just past the group's opening syntax —
	// after `(`, `(?:`, or `(?P<name>`.
	bodyStart int
	// bodyEnd is the offset of the group's `)`, or len(src) for the
	// virtual whole-pattern group.
	bodyEnd int
	// capturing distinguishes `(…)` and `(?P<name>…)` from `(?:…)`.
	capturing bool
	// capIndex is the group's 1-based capture number, matching what
	// `Regexp.SubexpNames` reports; 0 when the group does not capture.
	capIndex int
	// parent indexes the enclosing group, or -1 for the virtual one.
	parent int
	// alternations holds the offsets of the `|` bytes at the TOP level of
	// this group's body — the ones that split it into branches. A `|`
	// inside a nested group belongs to that group instead.
	alternations []int
}

// wholePatternGroup is the index of the virtual group standing for the
// entire pattern, which gives a group at the top level a parent to walk
// up to.
const wholePatternGroup = 0

// scanSourceGroups locates every group in src, reporting ok=false for any
// pattern whose group syntax this does not fully recognise — including an
// unbalanced one, which Go's own parser would reject anyway.
func scanSourceGroups(src string) ([]sourceGroup, bool) {
	groups := []sourceGroup{{open: -1, bodyStart: 0, bodyEnd: len(src), parent: -1}}
	open := []int{wholePatternGroup}
	captures := 0

	// Single-advance walker, the form [lsyntax.NormalizeDottedLabels] uses
	// and for the same reason. Every iteration ends with exactly one
	// `i = next`, and `next` holds the following byte BEFORE the dispatch
	// runs, so an arm that computes no offset of its own still consumes a
	// byte.
	//
	// Nothing forced that when each arm advanced the cursor itself. An arm
	// that returned the cursor it was given spun on that byte forever,
	// appending to `groups` or `alternations` on every lap — and the pattern
	// is a user's own regex arriving over HTTP, so the spin was an unbounded
	// allocation driven by untrusted text. A guard could catch that after the
	// fact; this shape makes it UNREPRESENTABLE, which is the stronger of the
	// two. Forgetting to advance is no longer a thing an arm can do.
	//
	// What the shape does not decide on its own is the three arms that DO
	// write `next`: each takes its offset from endOfQuotedRun, skipCharClass
	// or classifyGroupOpen. All three return an offset strictly past the byte
	// they were handed, which is their own contract rather than this loop's,
	// so it is asserted directly over a corpus by
	// [TestScannerHelpersAlwaysConsumeAByte].
	for i := 0; i < len(src); {
		next := i + 1
		switch src[i] {
		case '\\':
			// An escape covers the next byte whatever it is, so a `\(`
			// never opens a group and a `\[` never opens a class.
			if i+1 >= len(src) {
				return nil, false
			}
			if src[i+1] == 'Q' {
				// `\Q…\E` quotes everything up to the `\E`, or to the end
				// of the pattern when there is none. Reading two bytes and
				// carrying on would scan the QUOTED text as pattern, so a
				// `(` inside it would be counted as a group — shifting every
				// later capture index and pointing the rewrite at literal
				// text.
				next = endOfQuotedRun(src, i)
			} else {
				next = i + 2
			}
		case '[':
			end, ok := skipCharClass(src, i)
			if !ok {
				return nil, false
			}
			next = end
		case '|':
			innermost := open[len(open)-1]
			groups[innermost].alternations = append(groups[innermost].alternations, i)
		case '(':
			bodyStart, capturing, ok := classifyGroupOpen(src, i)
			if !ok {
				return nil, false
			}
			g := sourceGroup{
				open:      i,
				bodyStart: bodyStart,
				capturing: capturing,
				parent:    open[len(open)-1],
			}
			if capturing {
				captures++
				g.capIndex = captures
			}
			groups = append(groups, g)
			open = append(open, len(groups)-1)
			next = bodyStart
		case ')':
			if len(open) == 1 {
				return nil, false
			}
			groups[open[len(open)-1]].bodyEnd = i
			open = open[:len(open)-1]
		}
		i = next
	}
	if len(open) != 1 {
		return nil, false
	}
	return groups, true
}

// skipCharClass returns the offset just past the character class opening
// at src[i], which the caller has established is a `[`.
//
// The leading-`]` rule matters: in `[]a]` and `[^]a]` the first `]` is a
// member rather than the terminator, so treating it as the end would let
// the rest of the class — parentheses included — be scanned as pattern.
func skipCharClass(src string, i int) (int, bool) {
	j := i + 1
	if j < len(src) && src[j] == '^' {
		j++
	}
	if j < len(src) && src[j] == ']' {
		j++
	}
	// Single-advance walker — see scanSourceGroups, whose runaway this scan
	// shares. Only the two arms that skip more than one byte write `next`,
	// and both compute an offset strictly past `j`; every other byte of a
	// class is stepped over by the default advance.
	for j < len(src) {
		next := j + 1
		switch src[j] {
		case '\\':
			// No bounds guard here, unlike the escape arm of
			// scanSourceGroups: nothing reads src[j+1], and a trailing
			// backslash puts j past the end, which ends the loop and falls
			// through to the same `return 0, false` a guard would have
			// taken.
			next = j + 2
		case '[':
			// A POSIX name such as `[:alpha:]` nests inside the class and
			// carries its own `]`, which is not the class's terminator. A
			// `[` opening no POSIX name is an ordinary member, which the
			// default advance already steps over.
			if j+1 < len(src) && src[j+1] == ':' {
				end := strings.Index(src[j+2:], ":]")
				if end < 0 {
					return 0, false
				}
				next = j + 2 + end + 2
			}
		case ']':
			return j + 1, true
		}
		j = next
	}
	return 0, false
}

// endOfQuotedRun returns the offset just past the `\Q…\E` run starting at
// src[i], which the caller has established is the `\` of a `\Q`. An
// unterminated run quotes the rest of the pattern.
func endOfQuotedRun(src string, i int) int {
	if end := strings.Index(src[i+2:], `\E`); end >= 0 {
		return i + 2 + end + 2
	}
	return len(src)
}

// groupFlagBytes are the inline-flag letters Go accepts between `(?` and
// the `:` or `)` that ends the flag list.
const groupFlagBytes = "imsU-"

// classifyGroupOpen reads the group opening at src[i], which the caller
// has established is a `(`.
//
// A bare `(?flags)` SETTING makes the whole scan decline, which is not
// fussiness but the one construct that would make an inserted group
// change the pattern's meaning. Such a setting applies from where it
// appears to the end of the group enclosing it, CROSSING alternation
// branches: in `(?:(?i)a|b)` the `(?i)` reaches the `b` as well, so the
// pattern matches "B". Wrapping the first branch in a probe group —
// `(?:(?P<probe>(?i)a)|b)` — confines the setting to that branch, and
// "B" stops matching. The rewrite would then be reading capture groups
// out of a pattern that accepts a different language than the query
// asked for, which no amount of index bookkeeping can put right.
//
// The scoped spelling `(?flags:…)` is a group of its own and carries its
// setting with it wherever it is wrapped, so it stays allowed.
//
// Every group this returns therefore opens one, which is why there is no
// "opened nothing" outcome: the `)` arm exists only to recognise the
// setting in order to refuse it.
func classifyGroupOpen(src string, i int) (bodyStart int, capturing, ok bool) {
	if i+1 >= len(src) || src[i+1] != '?' {
		return i + 1, true, true
	}
	j := i + 2
	if j >= len(src) {
		return 0, false, false
	}
	// `(?P<name>…)` and its `(?<name>…)` spelling both capture.
	if src[j] == 'P' || src[j] == '<' {
		if src[j] == 'P' {
			j++
			if j >= len(src) || src[j] != '<' {
				return 0, false, false
			}
		}
		end := strings.IndexByte(src[j+1:], '>')
		if end < 0 {
			return 0, false, false
		}
		return j + 1 + end + 1, true, true
	}
	for j < len(src) && strings.IndexByte(groupFlagBytes, src[j]) >= 0 {
		j++
	}
	if j >= len(src) {
		return 0, false, false
	}
	if src[j] == ':' {
		return j + 1, false, true
	}
	return 0, false, false
}
