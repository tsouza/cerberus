// Package logpattern implements the LogQL pattern language used by the
// `| pattern` parser stage and the `|>` / `!>` line filters, as an
// Apache-2.0 package written from the language's documented syntax and the
// reference engine's observable behaviour.
//
// A pattern is a sequence of literal text and captures. A capture is `<`,
// an identifier (`[A-Za-z_][A-Za-z0-9_]*`) and `>`; `<_>` is the unnamed
// capture, which matches without extracting. A `<` that does not open a
// capture is literal text, and adjacent literal text forms one run.
package logpattern

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Validation errors. Their text is the reference's, which clients see in
// a rejected query's error message.
var (
	ErrNoCapture         = errors.New("at least one capture is required")
	ErrCaptureNotAllowed = errors.New("named captures are not allowed")
	ErrInvalidExpr       = errors.New("invalid expression")
)

// errEmptyPattern rejects a pattern with no text before its end or its
// first invalid UTF-8 sequence, in the reference's words: no token was
// read, so the position is the first column.
var errEmptyPattern = errors.New("parse error at line 1, col 1: syntax error: unexpected $end, expecting IDENTIFIER or LITERAL")

// unnamedCapture is the capture name that matches without extracting.
const unnamedCapture = "_"

// part is one element of a scanned pattern: a capture when name is set,
// otherwise a run of literal text.
type part struct {
	name string
	text []byte
}

func (p part) isCapture() bool { return p.name != "" }

// extracts reports whether the part is a capture whose value is returned.
func (p part) extracts() bool { return p.isCapture() && p.name != unnamedCapture }

// source renders the part as it is written in a pattern.
func (p part) source() string {
	if p.isCapture() {
		return "<" + p.name + ">"
	}
	return string(p.text)
}

// scan splits a pattern into literal runs and captures, in order. The
// pattern ends at its first invalid UTF-8 sequence: nothing from there on
// is part of it, while a valid encoding of U+FFFD is ordinary text. Only
// an empty pattern is an error; every other rule is checked by the
// callers.
func scan(src []byte) ([]part, error) {
	var parts []part
	var run []byte
	endRun := func() {
		if len(run) > 0 {
			parts = append(parts, part{text: run})
			run = nil
		}
	}
	for len(src) > 0 {
		if name, width := captureAt(src); width > 0 {
			endRun()
			parts = append(parts, part{name: name})
			src = src[width:]
			continue
		}
		r, size := utf8.DecodeRune(src)
		if r == utf8.RuneError && size <= 1 {
			break
		}
		run = append(run, src[:size]...)
		src = src[size:]
	}
	endRun()
	if len(parts) == 0 {
		return nil, errEmptyPattern
	}
	return parts, nil
}

// captureAt returns the name of the capture src opens and the number of
// bytes it spans, or a zero width when src does not open one.
func captureAt(src []byte) (string, int) {
	const shortestCapture = len("<a>")
	if len(src) < shortestCapture || src[0] != '<' || !isNameStart(src[1]) {
		return "", 0
	}
	closer := 2
	for closer < len(src) && isNamePart(src[closer]) {
		closer++
	}
	if closer == len(src) || src[closer] != '>' {
		return "", 0
	}
	return string(src[1:closer]), closer + 1
}

func isNameStart(b byte) bool {
	return b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

func isNamePart(b byte) bool { return isNameStart(b) || ('0' <= b && b <= '9') }

// rejectAdjacentCaptures fails on the first two captures with no literal
// text between them: nothing would decide where one ends.
func rejectAdjacentCaptures(parts []part) error {
	for i := 1; i < len(parts); i++ {
		if parts[i-1].isCapture() && parts[i].isCapture() {
			pair := parts[i-1].source() + parts[i].source()
			return fmt.Errorf("found consecutive capture '%s': %w", pair, ErrInvalidExpr)
		}
	}
	return nil
}

// extractedNames lists the names of the extracting captures, in order.
func extractedNames(parts []part) []string {
	var names []string
	for _, p := range parts {
		if p.extracts() {
			names = append(names, p.name)
		}
	}
	return names
}

// rejectRepeatedName fails on the first name that occurs twice.
func rejectRepeatedName(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			return fmt.Errorf("duplicate capture name (%s): %w", n, ErrInvalidExpr)
		}
		seen[n] = true
	}
	return nil
}

// hole is a capture together with the literal text that ends it. until is
// empty for a capture that ends the pattern.
type hole struct {
	name  string
	until []byte
}

// Matcher is a compiled pattern: an optional leading literal, then
// captures, each followed by the literal that ends it.
type Matcher struct {
	lead  []byte
	holes []hole
	names []string
	// out is the reusable result buffer for Matches, sized to names.
	out [][]byte
}

// compile builds the matcher of a pattern that has no adjacent captures.
func compile(parts []part) *Matcher {
	m := &Matcher{}
	if len(parts) > 0 && !parts[0].isCapture() {
		m.lead = parts[0].text
		parts = parts[1:]
	}
	for i := 0; i < len(parts); i += 2 {
		h := hole{name: parts[i].name}
		if i+1 < len(parts) {
			h.until = parts[i+1].text
		}
		m.holes = append(m.holes, h)
	}
	return m
}

func (m *Matcher) empty() bool { return len(m.lead) == 0 && len(m.holes) == 0 }

// New compiles a `| pattern` expression. It requires at least one named
// capture, and rejects adjacent captures and a repeated name.
func New(in string) (*Matcher, error) {
	parts, err := scan([]byte(in))
	if err != nil {
		return nil, err
	}
	names := extractedNames(parts)
	if len(names) == 0 {
		return nil, ErrNoCapture
	}
	if err := rejectAdjacentCaptures(parts); err != nil {
		return nil, err
	}
	if err := rejectRepeatedName(names); err != nil {
		return nil, err
	}
	m := compile(parts)
	m.names = names
	m.out = make([][]byte, 0, len(names))
	return m, nil
}

// ParseLineFilter compiles a `|>` / `!>` line-filter pattern. The empty
// pattern is a valid filter; otherwise captures may not be adjacent and
// may only be the unnamed `<_>`.
func ParseLineFilter(in []byte) (*Matcher, error) {
	if len(in) == 0 {
		return &Matcher{}, nil
	}
	parts, err := scan(in)
	if err != nil {
		return nil, err
	}
	if err := rejectAdjacentCaptures(parts); err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p.extracts() {
			return nil, fmt.Errorf("%w: found '%s'", ErrCaptureNotAllowed, p.source())
		}
	}
	return compile(parts), nil
}

// ParseLiterals returns the pattern's literal runs in order.
func ParseLiterals(in string) ([][]byte, error) {
	parts, err := scan([]byte(in))
	if err != nil {
		return nil, err
	}
	runs := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if !p.isCapture() {
			runs = append(runs, p.text)
		}
	}
	return runs, nil
}

// Names returns the named captures, in order.
func (m *Matcher) Names() []string { return m.names }

// Literals returns the pattern's literal runs, in order.
func (m *Matcher) Literals() [][]byte {
	var runs [][]byte
	if len(m.lead) > 0 {
		runs = append(runs, m.lead)
	}
	for _, h := range m.holes {
		if len(h.until) > 0 {
			runs = append(runs, h.until)
		}
	}
	return runs
}

// StartsWithLiteral reports whether the pattern's first element is literal
// text.
func (m *Matcher) StartsWithLiteral() bool { return len(m.lead) > 0 }

// EndsWithCapture reports whether the pattern's last element is a capture.
func (m *Matcher) EndsWithCapture() bool {
	return len(m.holes) > 0 && len(m.holes[len(m.holes)-1].until) == 0
}

// Matches returns the value of each named capture in line, or nil when the
// line is empty, the pattern has no capture, or the line does not start
// with the pattern's leading literal. Each capture extends to the first
// occurrence of the literal after it; when that literal is absent, or the
// capture ends the pattern, it takes the rest of the line and matching
// stops. The returned slice is reused by the next call.
func (m *Matcher) Matches(line []byte) [][]byte {
	if len(line) == 0 || m.empty() {
		return nil
	}
	rest, ok := bytes.CutPrefix(line, m.lead)
	if !ok || len(m.holes) == 0 {
		return nil
	}
	out := m.out[:0]
	for _, h := range m.holes {
		value, after, closed := cutAtLiteral(rest, h.until)
		if h.name != unnamedCapture {
			out = append(out, value)
		}
		if !closed {
			break
		}
		rest = after
	}
	return out
}

// cutAtLiteral splits s around the first occurrence of lit. closed is false
// when lit is empty or absent, in which case value is all of s.
func cutAtLiteral(s, lit []byte) (value, after []byte, closed bool) {
	if len(lit) == 0 {
		return s, nil, false
	}
	return bytes.Cut(s, lit)
}

// Test reports whether line matches the pattern as a line filter. The
// leading literal may occur anywhere; every capture must match at least
// one byte, up to the first occurrence of the literal after it; and the
// line must end where the pattern does. The empty pattern matches only
// the empty line.
func (m *Matcher) Test(line []byte) bool {
	if len(line) == 0 || m.empty() {
		return len(line) == 0 && m.empty()
	}
	pos := 0
	if len(m.lead) > 0 {
		at := bytes.Index(line, m.lead)
		if at < 0 {
			return false
		}
		pos = at + len(m.lead)
	}
	for _, h := range m.holes {
		if len(h.until) == 0 {
			return pos < len(line)
		}
		at := bytes.Index(line[pos:], h.until)
		if at <= 0 {
			return false
		}
		pos += at + len(h.until)
	}
	return pos == len(line)
}
