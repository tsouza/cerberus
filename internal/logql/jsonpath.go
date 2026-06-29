package logql

import (
	"fmt"
	"strconv"
)

// jsonPathParse parses a Loki JSON-path expression (`foo.bar`,
// `foo["bar"]`, `foo[0].baz`, `["a"]["b"]`, …) into an ordered list of
// segments: a string segment is an object key, an int segment is an
// array index. These feed ClickHouse's variadic `JSONExtractString(Body,
// seg1, seg2, …)`.
//
// It is a clean-room reimplementation of grafana/loki's
// pkg/logql/log/jsonexpr package (AGPLv3) so the cerberus binary stays
// Apache-only. The tokeniser and accepted grammar are matched
// byte-for-byte against upstream (asserted by the agpl_oracle A/B test):
//
//	values : field | key_access | index_access
//	       | values key_access | values index_access | values DOT field
//	key_access   : '[' STRING ']'
//	index_access : '[' INDEX  ']'
//
// i.e. the path starts with a bare field, a ["quoted key"], or an
// [integer index], then chains any number of ["key"], [index], or .field
// accesses.
func jsonPathParse(expr string) ([]any, error) {
	sc := &jsonPathScanner{src: []rune(expr)}

	first, err := sc.next()
	if err != nil {
		return nil, err
	}
	var out []any
	switch first.kind {
	case jpField:
		out = append(out, first.str)
	case jpLSB:
		seg, err := sc.bracketSegment()
		if err != nil {
			return nil, err
		}
		out = append(out, seg)
	default:
		return nil, fmt.Errorf("unexpected token %q at start of json path", first.text())
	}

	for {
		tok, err := sc.next()
		if err != nil {
			return nil, err
		}
		switch tok.kind {
		case jpEOF:
			return out, nil
		case jpLSB:
			seg, err := sc.bracketSegment()
			if err != nil {
				return nil, err
			}
			out = append(out, seg)
		case jpDot:
			f, err := sc.next()
			if err != nil {
				return nil, err
			}
			if f.kind != jpField {
				return nil, fmt.Errorf("expected field after '.', got %q", f.text())
			}
			out = append(out, f.str)
		default:
			return nil, fmt.Errorf("unexpected token %q in json path", tok.text())
		}
	}
}

// bracketSegment parses the body of a `[ … ]` access after the '[' has
// been consumed: either a quoted STRING key or an integer INDEX,
// followed by the closing ']'.
func (sc *jsonPathScanner) bracketSegment() (any, error) {
	tok, err := sc.next()
	if err != nil {
		return nil, err
	}
	var seg any
	switch tok.kind {
	case jpString:
		seg = tok.str
	case jpIndex:
		seg = tok.idx
	default:
		return nil, fmt.Errorf("expected string or index inside '[]', got %q", tok.text())
	}
	closing, err := sc.next()
	if err != nil {
		return nil, err
	}
	if closing.kind != jpRSB {
		return nil, fmt.Errorf("expected ']', got %q", closing.text())
	}
	return seg, nil
}

type jpTokenKind int

const (
	jpEOF jpTokenKind = iota
	jpField
	jpString
	jpIndex
	jpDot
	jpLSB
	jpRSB
)

type jpToken struct {
	kind jpTokenKind
	str  string
	idx  int
}

func (t jpToken) text() string {
	switch t.kind {
	case jpEOF:
		return "<eof>"
	case jpField, jpString:
		return t.str
	case jpIndex:
		return strconv.Itoa(t.idx)
	case jpDot:
		return "."
	case jpLSB:
		return "["
	case jpRSB:
		return "]"
	default:
		return "?"
	}
}

type jsonPathScanner struct {
	src []rune
	pos int
}

func (sc *jsonPathScanner) read() (rune, bool) {
	if sc.pos >= len(sc.src) {
		return 0, false
	}
	r := sc.src[sc.pos]
	sc.pos++
	return r, true
}

func (sc *jsonPathScanner) unread() {
	if sc.pos > 0 {
		sc.pos--
	}
}

func (sc *jsonPathScanner) next() (jpToken, error) {
	for {
		r, ok := sc.read()
		if !ok {
			return jpToken{kind: jpEOF}, nil
		}
		if isJSONWhitespace(r) {
			continue
		}
		if r >= '0' && r <= '9' {
			sc.unread()
			n, err := sc.scanInt()
			if err != nil {
				return jpToken{}, err
			}
			return jpToken{kind: jpIndex, idx: n}, nil
		}
		switch {
		case r == '[':
			return jpToken{kind: jpLSB}, nil
		case r == ']':
			return jpToken{kind: jpRSB}, nil
		case r == '.':
			return jpToken{kind: jpDot}, nil
		case isJSONStartIdentifier(r):
			sc.unread()
			return jpToken{kind: jpField, str: sc.scanField()}, nil
		case r == '"':
			sc.unread()
			return jpToken{kind: jpString, str: sc.scanStr()}, nil
		default:
			return jpToken{}, fmt.Errorf("unexpected char %c", r)
		}
	}
}

func (sc *jsonPathScanner) scanField() string {
	var out []rune
	for {
		r, ok := sc.read()
		if !ok {
			break
		}
		if !isJSONIdentifier(r) {
			sc.unread()
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// scanStr reads a quoted key. Upstream consumes the opening quote then
// reads until the closing '"' OR a ']' OR end-of-input, and does not
// unread the terminator.
func (sc *jsonPathScanner) scanStr() string {
	r, ok := sc.read()
	if !ok || r != '"' {
		return ""
	}
	var out []rune
	for {
		r, ok := sc.read()
		if !ok {
			break
		}
		if r == '"' || r == ']' {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// scanInt reads a decimal array index. A '.' after digits is a
// float-index error; a non-digit (other than the terminators ' ', '.',
// ']') is an error.
func (sc *jsonPathScanner) scanInt() (int, error) {
	var digits []rune
	for {
		r, ok := sc.read()
		if !ok {
			break
		}
		if r == '.' && len(digits) > 0 {
			return 0, fmt.Errorf("cannot use float array index")
		}
		if isJSONWhitespace(r) || r == '.' || r == ']' {
			sc.unread()
			break
		}
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-integer value: %c", r)
		}
		digits = append(digits, r)
	}
	if len(digits) == 0 {
		return 0, fmt.Errorf("empty index")
	}
	return strconv.Atoi(string(digits))
}

func isJSONWhitespace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

func isJSONStartIdentifier(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
}

func isJSONIdentifier(r rune) bool {
	return isJSONStartIdentifier(r) || (r >= '0' && r <= '9')
}
