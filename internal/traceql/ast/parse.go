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
	root, err := parseTree(s)
	if err != nil {
		return nil, err
	}

	// Static typing runs on the tree the grammar produced, BEFORE the
	// array-fold rewrites — the order the reference validates in, and the
	// reason both report the same sentence for the same query. The
	// reference's Parse applies no validation at all; its query path
	// validates through ParseNoOptimizations, which skips every AST
	// transformation (the OR-to-IN fold among them), so the message names
	// the comparison the client actually wrote rather than a folded form
	// of it.
	//
	// The order is also what lets the rule stay complete without an array
	// case: a chain can only fold into a mismatched array comparison if
	// one of its scalar comparisons was already mismatched, and that
	// comparison is rejected here, before any fold runs.
	if verr := root.validate(); verr != nil {
		return nil, verr
	}
	return applyRewrites(root), nil
}

// parseTree runs the grammar alone, producing the raw tree with none of
// Parse's static-validation pass or rewrites applied. ParseIdentifier is
// the one caller: it wraps a bare attribute reference in a synthetic
// `{ ... }` to reuse the grammar, and that wrapper is never a real span
// filter, so the reference's boolean-result rule (a genuine `{ duration }`
// query is rejected; `ParseIdentifier("duration")` must not be) does not
// apply to it.
func parseTree(s string) (expr *RootExpr, err error) {
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

	root := c.parseRoot()
	return root, nil
}

// ParseIdentifier parses a single attribute/intrinsic reference (e.g.
// `.service.name`, `span.http.status_code`, `duration`) by wrapping it in a
// spanset filter and extracting the lone attribute. It rejects anything that
// is not a bare attribute reference.
func ParseIdentifier(s string) (Attribute, error) {
	expr, err := parseTree("{" + s + "}")
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
