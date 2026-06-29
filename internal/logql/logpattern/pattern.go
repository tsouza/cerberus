// Package logpattern is a clean-room reimplementation of grafana/loki's
// pkg/logql/log/pattern (AGPLv3), so the cerberus binary stays
// Apache-only. It parses Loki "pattern" expressions — alternating
// literal runs and `<name>` / `<_>` captures — used by the `| pattern`
// parser stage and the `|>` / `!>` line filters.
//
// A capture is `<` then an identifier (`[A-Za-z_][A-Za-z0-9_]*`) then
// `>`; `<_>` is the unnamed capture. Any `<` not forming a valid capture
// is an ordinary literal. Consecutive non-capture runes merge into one
// literal node. The tokenisation and node shape match upstream
// byte-for-byte (asserted by the agpl_oracle A/B test).
package logpattern

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Validation errors, matching upstream's exported error values.
var (
	ErrNoCapture         = errors.New("at least one capture is required")
	ErrCaptureNotAllowed = errors.New("named captures are not allowed")
	ErrInvalidExpr       = errors.New("invalid expression")
)

type node interface {
	fmt.Stringer
}

type capture string

func (c capture) String() string  { return "<" + string(c) + ">" }
func (c capture) Name() string    { return string(c) }
func (c capture) isUnnamed() bool { return c == "_" }

type literals []byte

func (l literals) String() string { return string(l) }

type expr []node

func (e expr) captures() (out []string) {
	for _, n := range e {
		if c, ok := n.(capture); ok && !c.isUnnamed() {
			out = append(out, c.Name())
		}
	}
	return out
}

func (e expr) captureCount() int { return len(e.captures()) }

// hasCapture reports whether the pattern carries at least one NAMED
// capture. Upstream treats a pattern with only unnamed `<_>` captures as
// having no captures (captures() excludes unnamed), so `| pattern "<_>"`
// is rejected by New.
func (e expr) hasCapture() bool { return e.captureCount() != 0 }

func (e expr) validate() error {
	if !e.hasCapture() {
		return ErrNoCapture
	}
	if err := e.validateNoConsecutiveCaptures(); err != nil {
		return err
	}
	uniq := map[string]struct{}{}
	for _, c := range e.captures() {
		if _, ok := uniq[c]; ok {
			return fmt.Errorf("duplicate capture name (%s): %w", c, ErrInvalidExpr)
		}
		uniq[c] = struct{}{}
	}
	return nil
}

func (e expr) validateNoConsecutiveCaptures() error {
	for i, n := range e {
		if i+1 >= len(e) {
			break
		}
		if _, ok := n.(capture); ok {
			if next, ok := e[i+1].(capture); ok {
				return fmt.Errorf("found consecutive capture '%s': %w", n.String()+next.String(), ErrInvalidExpr)
			}
		}
	}
	return nil
}

func (e expr) validateNoNamedCaptures() error {
	for _, n := range e {
		if c, ok := n.(capture); ok && !c.isUnnamed() {
			return fmt.Errorf("%w: found '%s'", ErrCaptureNotAllowed, n.String())
		}
	}
	return nil
}

// errEmptyPattern mirrors upstream's grammar rejecting an empty token
// stream (its `expr` production requires at least one node).
var errEmptyPattern = errors.New("syntax error: empty pattern")

// parse tokenises the pattern into an alternating sequence of literal
// runs and captures. It errors only on empty input (zero nodes), matching
// upstream's grammar; otherwise validation is the caller's job.
func parse(input []byte) (expr, error) {
	var nodes []node
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		nodes = append(nodes, runesToLiterals(run))
		run = run[:0]
	}
	for i := 0; i < len(input); {
		if input[i] == '<' {
			if name, consumed, ok := matchCapture(input[i:]); ok {
				flush()
				nodes = append(nodes, capture(name))
				i += consumed
				continue
			}
		}
		r, size := utf8.DecodeRune(input[i:])
		run = append(run, r)
		i += size
	}
	flush()
	if len(nodes) == 0 {
		return nil, errEmptyPattern
	}
	return expr(nodes), nil
}

// matchCapture reports whether s (which starts with '<') opens a valid
// `<identifier>` capture, returning the inner name and the number of
// bytes consumed (through the closing '>').
func matchCapture(s []byte) (name string, consumed int, ok bool) {
	if len(s) < 2 || !isIdentStart(s[1]) {
		return "", 0, false
	}
	j := 2
	for j < len(s) && isIdentCont(s[j]) {
		j++
	}
	if j < len(s) && s[j] == '>' {
		return string(s[1:j]), j + 1, true
	}
	return "", 0, false
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// runesToLiterals re-encodes a rune run to its byte form, matching
// upstream (invalid bytes decode to utf8.RuneError, which re-encodes to
// the replacement character).
func runesToLiterals(rs []rune) literals {
	res := make([]byte, len(rs)*utf8.UTFMax)
	count := 0
	for _, r := range rs {
		count += utf8.EncodeRune(res[count:], r)
	}
	return res[:count]
}

// Matcher is a compiled pattern. It exposes the capture names and runs
// the line-extraction the `| pattern` stage needs.
type Matcher struct {
	e        expr
	captures [][]byte
	names    []string
}

// New compiles a `| pattern` expression. It requires at least one
// capture, rejects consecutive captures, and rejects duplicate names.
func New(in string) (*Matcher, error) {
	e, err := parse([]byte(in))
	if err != nil {
		return nil, err
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return &Matcher{
		e:        e,
		captures: make([][]byte, 0, e.captureCount()),
		names:    e.captures(),
	}, nil
}

// ParseLineFilter compiles a `|>` / `!>` line-filter pattern: captures
// may not be consecutive and may only be the unnamed `<_>`.
func ParseLineFilter(in []byte) (*Matcher, error) {
	if len(in) == 0 {
		return new(Matcher), nil
	}
	e, err := parse(in)
	if err != nil {
		return nil, err
	}
	if err := e.validateNoConsecutiveCaptures(); err != nil {
		return nil, err
	}
	if err := e.validateNoNamedCaptures(); err != nil {
		return nil, err
	}
	return &Matcher{e: e}, nil
}

// ParseLiterals returns the pattern's literal runs in order.
func ParseLiterals(in string) ([][]byte, error) {
	e, err := parse([]byte(in))
	if err != nil {
		return nil, err
	}
	lit := make([][]byte, 0, len(e))
	for _, n := range e {
		if l, ok := n.(literals); ok {
			lit = append(lit, l)
		}
	}
	return lit, nil
}

// Names returns the named captures, in order.
func (m *Matcher) Names() []string { return m.names }

// Matches extracts the named-capture values from in, or nil if the
// pattern cannot be anchored. The returned slice is invalidated by the
// next call.
func (m *Matcher) Matches(in []byte) [][]byte {
	if len(in) == 0 || len(m.e) == 0 {
		return nil
	}
	captures := m.captures[:0]
	e := m.e
	if ls, ok := e[0].(literals); ok {
		if bytes.Index(in, ls) != 0 {
			return nil
		}
		in = in[len(ls):]
		e = e[1:]
	}
	if len(e) == 0 {
		return nil
	}
	for len(e) != 0 {
		if len(e) == 1 { // ending on a capture
			if !e[0].(capture).isUnnamed() {
				captures = append(captures, in)
			}
			return captures
		}
		capt := e[0].(capture)
		ls := e[1].(literals)
		e = e[2:]
		i := bytes.Index(in, ls)
		if i == -1 {
			if !capt.isUnnamed() {
				captures = append(captures, in)
			}
			return captures
		}
		if capt.isUnnamed() {
			in = in[len(ls)+i:]
			continue
		}
		captures = append(captures, in[:i])
		in = in[len(ls)+i:]
	}
	return captures
}

// Test reports whether in matches the pattern, with the greedy,
// first-occurrence, no-empty-capture semantics of upstream's filter form.
func (m *Matcher) Test(in []byte) bool {
	if len(in) == 0 || len(m.e) == 0 {
		return len(in) == 0 && len(m.e) == 0
	}
	var off int
	for i := 0; i < len(m.e); i++ {
		lit, ok := m.e[i].(literals)
		if !ok {
			continue
		}
		j := bytes.Index(in[off:], lit)
		if j == -1 {
			return false
		}
		if i != 0 && j == 0 {
			return false
		}
		off += j + len(lit)
	}
	_, reqRem := m.e[len(m.e)-1].(capture)
	hasRem := off != len(in)
	return reqRem == hasRem
}
