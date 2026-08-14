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

	for i := 0; i < len(src); {
		switch src[i] {
		case '\\':
			// An escape covers the next byte whatever it is, so a `\(`
			// never opens a group and a `\[` never opens a class.
			if i+1 >= len(src) {
				return nil, false
			}
			i += 2
		case '[':
			end, ok := skipCharClass(src, i)
			if !ok {
				return nil, false
			}
			i = end
		case '|':
			innermost := open[len(open)-1]
			groups[innermost].alternations = append(groups[innermost].alternations, i)
			i++
		case '(':
			bodyStart, capturing, opens, ok := classifyGroupOpen(src, i)
			if !ok {
				return nil, false
			}
			if !opens {
				// A `(?flags)` setting opens nothing; it just ends.
				i = bodyStart
				continue
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
			i = bodyStart
		case ')':
			if len(open) == 1 {
				return nil, false
			}
			groups[open[len(open)-1]].bodyEnd = i
			open = open[:len(open)-1]
			i++
		default:
			i++
		}
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
	for j < len(src) {
		switch src[j] {
		case '\\':
			if j+1 >= len(src) {
				return 0, false
			}
			j += 2
		case '[':
			// A POSIX name such as `[:alpha:]` nests inside the class and
			// carries its own `]`, which is not the class's terminator.
			if j+1 < len(src) && src[j+1] == ':' {
				end := strings.Index(src[j+2:], ":]")
				if end < 0 {
					return 0, false
				}
				j += 2 + end + 2
				continue
			}
			j++
		case ']':
			return j + 1, true
		default:
			j++
		}
	}
	return 0, false
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
// opens=false is therefore reserved for nothing at present; the return
// stays in the signature because the `)` arm is what recognises the
// setting in order to refuse it.
func classifyGroupOpen(src string, i int) (bodyStart int, capturing, opens, ok bool) {
	if i+1 >= len(src) || src[i+1] != '?' {
		return i + 1, true, true, true
	}
	j := i + 2
	if j >= len(src) {
		return 0, false, false, false
	}
	// `(?P<name>…)` and its `(?<name>…)` spelling both capture.
	if src[j] == 'P' || src[j] == '<' {
		if src[j] == 'P' {
			j++
			if j >= len(src) || src[j] != '<' {
				return 0, false, false, false
			}
		}
		end := strings.IndexByte(src[j+1:], '>')
		if end < 0 {
			return 0, false, false, false
		}
		return j + 1 + end + 1, true, true, true
	}
	for j < len(src) && strings.IndexByte(groupFlagBytes, src[j]) >= 0 {
		j++
	}
	if j >= len(src) {
		return 0, false, false, false
	}
	switch src[j] {
	case ':':
		return j + 1, false, true, true
	case ')':
		return 0, false, false, false
	}
	return 0, false, false, false
}
