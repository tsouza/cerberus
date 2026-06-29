package loki

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// logfmtDecoder is a clean-room reimplementation of the byte-oriented
// logfmt key/value scanner Loki uses in pkg/logql/log/logfmt (itself
// adapted, MIT, from go-logfmt/logfmt). It is reproduced here so the
// cerberus binary stays Apache-only while keeping byte-for-byte parity
// with Loki's detected-fields extraction — including the non-strict
// "skip the malformed pair and continue" behaviour go-logfmt's
// Reader-based decoder cannot reproduce (it aborts the record on the
// first error).
//
// ScanKeyval advances to the next key/value pair, returning false at
// end-of-line or on a syntax error (after skipping the offending pair so
// a caller can continue). Key/Value return slices into the input line.
type logfmtDecoder struct {
	line []byte
	pos  int
	key  []byte
	val  []byte
}

func newLogfmtDecoder(line []byte) *logfmtDecoder {
	return &logfmtDecoder{line: line}
}

func (d *logfmtDecoder) eol() bool { return d.pos >= len(d.line) }

func (d *logfmtDecoder) Key() []byte   { return d.key }
func (d *logfmtDecoder) Value() []byte { return d.val }

// skipValue advances past the rest of a malformed token to the next
// whitespace and reports the failure.
func (d *logfmtDecoder) skipValue() bool {
	for d.pos < len(d.line) {
		if d.line[d.pos] <= ' ' {
			return false
		}
		d.pos++
	}
	return false
}

//nolint:gocyclo // faithful port of the logfmt key/value state machine
func (d *logfmtDecoder) scanKeyval() bool {
	d.key, d.val = nil, nil
	line := d.line

	// Skip garbage (control/space bytes) before the key.
	for d.pos < len(line) && line[d.pos] <= ' ' {
		d.pos++
	}
	if d.pos >= len(line) {
		return false
	}

	// Scan the key: bytes up to '=', whitespace, or the invalid '"'.
	start := d.pos
	for d.pos < len(line) {
		c := line[d.pos]
		switch {
		case c == '=':
			if d.pos > start {
				d.key = line[start:d.pos]
			}
			if d.key == nil {
				// '=' with no key — malformed.
				return d.skipValue()
			}
			return d.scanEqual()
		case c == '"':
			// A quote inside a key is invalid.
			return d.skipValue()
		case c <= ' ':
			// Bare key, no value.
			d.key = line[start:d.pos]
			return true
		}
		d.pos++
	}
	// EOL while scanning the key: bare key, no value.
	if d.pos > start {
		d.key = line[start:d.pos]
	}
	return true
}

// scanEqual consumes the '=' and the value (bare or quoted) that follows.
func (d *logfmtDecoder) scanEqual() bool {
	line := d.line
	d.pos++ // consume '='
	if d.pos >= len(line) {
		return true // key=  with nothing after
	}
	switch c := line[d.pos]; {
	case c <= ' ':
		return true // key= followed by whitespace -> empty value
	case c == '"':
		return d.scanQuoted()
	}

	start := d.pos
	for d.pos < len(line) {
		c := line[d.pos]
		switch {
		case c == '=' || c == '"':
			return d.skipValue()
		case c <= ' ':
			if d.pos > start {
				d.val = line[start:d.pos]
			}
			return true
		}
		d.pos++
	}
	if d.pos > start {
		d.val = line[start:d.pos]
	}
	return true
}

// scanQuoted consumes a "double-quoted" value, honouring backslash
// escapes (JSON-style, matching go-logfmt). dec.pos is at the opening
// quote on entry.
func (d *logfmtDecoder) scanQuoted() bool {
	line := d.line
	start := d.pos
	esc, hasEsc := false, false
	for p := d.pos + 1; p < len(line); p++ {
		c := line[p]
		switch {
		case esc:
			esc = false
		case c == '\\':
			hasEsc, esc = true, true
		case c == '"':
			d.pos = p + 1
			quoted := line[start:d.pos]
			if hasEsc {
				v, ok := unquoteLogfmt(quoted)
				if !ok {
					return false
				}
				d.val = v
				return true
			}
			// Strip the surrounding quotes without copying.
			if len(quoted) > 2 {
				d.val = quoted[1 : len(quoted)-1]
			}
			return true
		}
	}
	// Unterminated quote.
	d.pos = len(line)
	return false
}

// unquoteLogfmt unquotes a JSON-style double-quoted byte slice (matching
// go-logfmt's unquoteBytes, which is itself adapted from encoding/json).
func unquoteLogfmt(b []byte) ([]byte, bool) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false
	}
	return []byte(s), true
}

// removeInvalidUTF maps the UTF-8 replacement rune out of a value, as
// Loki does before storing an extracted value.
func removeInvalidUTF(b []byte) []byte {
	if !bytes.ContainsRune(b, utf8.RuneError) {
		return b
	}
	return bytes.Map(func(r rune) rune {
		if r == utf8.RuneError {
			return -1
		}
		return r
	}, b)
}
