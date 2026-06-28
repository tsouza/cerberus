package ast

import "fmt"

// ParseError is returned when a query cannot be parsed.
type ParseError struct {
	msg  string
	line int
	col  int
}

func (e *ParseError) Error() string {
	if e.line == 0 && e.col == 0 {
		return fmt.Sprintf("parse error : %s", e.msg)
	}
	return fmt.Sprintf("parse error at line %d, col %d: %s", e.line, e.col, e.msg)
}

func newParseError(msg string, line, col int) *ParseError {
	return &ParseError{msg: msg, line: line, col: col}
}

// Parse parses a TraceQL query string into the native AST. It is the entry
// point the TraceQL head and the Tempo HTTP handlers call.
func Parse(s string) (expr *RootExpr, err error) {
	toks, lexErr := tokenize(s)
	if lexErr != nil {
		return nil, newParseError(lexErr.Error(), 0, 0)
	}

	p := &parser{toks: toks}
	c := &cursor{p: p}

	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(parseErr); ok {
				expr = nil
				err = newParseError(pe.msg, pe.line, pe.col)
				return
			}
			panic(r)
		}
	}()

	return applyRewrites(c.parseRoot()), nil
}

// ParseIdentifier parses a single attribute/intrinsic reference (e.g.
// `.service.name`, `span.http.status_code`, `duration`) by wrapping it in a
// spanset filter and extracting the lone attribute. It rejects anything that
// is not a bare attribute reference.
func ParseIdentifier(s string) (Attribute, error) {
	expr, err := Parse("{" + s + "}")
	if err != nil {
		return Attribute{}, fmt.Errorf("failed to parse identifier %s: %w", s, err)
	}
	if expr == nil || len(expr.Pipeline.Elements) == 0 {
		return Attribute{}, fmt.Errorf("failed to parse identifier %s: no pipeline elements found", s)
	}
	filter, ok := expr.Pipeline.Elements[0].(*SpansetFilter)
	if !ok {
		return Attribute{}, fmt.Errorf("failed to parse identifier %s: expected SpansetFilter but got %T", s, expr.Pipeline.Elements[0])
	}
	attr, ok := filter.Expression.(Attribute)
	if !ok {
		return Attribute{}, fmt.Errorf("failed to parse identifier %s: expected Attribute but got %T", s, filter.Expression)
	}
	return attr, nil
}

// MustParseIdentifier is the panicking form of ParseIdentifier.
func MustParseIdentifier(s string) Attribute {
	a, err := ParseIdentifier(s)
	if err != nil {
		panic(err)
	}
	return a
}
