package lsyntax

import (
	"errors"
	"fmt"
)

// ErrParse is the sentinel that every parse failure wraps, so callers
// can branch on errors.Is(err, ErrParse). The message text mirrors the
// upstream LogQL parser so error strings stay wire-compatible.
var ErrParse = errors.New("failed to parse the log query")

// ParseError is returned when a query fails to parse. The rendered
// message format ("parse error ...") mirrors the upstream LogQL parser
// so any caller asserting on error text — including cerberus's
// permissive-retry substring check on "empty-compatible" — keeps
// working unchanged.
type ParseError struct {
	msg       string
	line, col int
}

func (p ParseError) Error() string {
	if p.col == 0 && p.line == 0 {
		return fmt.Sprintf("parse error : %s", p.msg)
	}
	return fmt.Sprintf("parse error at line %d, col %d: %s", p.line, p.col, p.msg)
}

// Is lets errors.Is(err, ErrParse) match any ParseError.
func (p ParseError) Is(target error) bool { return target == ErrParse }

// NewParseError builds a ParseError at the given source position. A
// zero line/col renders without the positional prefix.
func NewParseError(msg string, line, col int) ParseError {
	return ParseError{msg: msg, line: line, col: col}
}
