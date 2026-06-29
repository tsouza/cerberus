package logql

import (
	"fmt"
	"strings"

	logpattern "github.com/tsouza/cerberus/internal/logql/logpattern"

	"github.com/tsouza/cerberus/internal/chplan"
)

// `|>` / `!>` pattern line-filter lowering.
//
// Reference semantics are pkg/logql/log/pattern.Matcher.Test (reached
// via filter.go::newPatternFilterer): the pattern is an alternating
// sequence of literal runs and `<_>` wildcards (named captures and
// consecutive wildcards are parse errors for the FILTER form —
// logpattern.ParseLineFilter enforces both; reference rejects the query
// with a 400, cerberus with a 422). Test walks the line left-to-right:
//
//  1. each literal must be found (bytes.Index) at-or-after the cursor;
//  2. a literal that is NOT the pattern's first node must match
//     STRICTLY after the cursor — a zero-length gap means an empty
//     wildcard, which never matches ("` bar `" does not match
//     "<_> bar <_>"); the FIRST node being a literal is exempt, so a
//     leading literal floats like an implicit wildcard prefix
//     (Test never anchors it at offset 0);
//  3. after the last literal: a pattern ending in a literal requires
//     the cursor to sit exactly at end-of-line; one ending in `<_>`
//     requires a NON-empty remainder ("foo bar baz" matches "<_> baz"
//     but not "<_> bar");
//  4. an empty line only matches an empty pattern (and vice versa: an
//     empty pattern matches only empty lines).
//
// Crucially Test is first-occurrence greedy, NOT a backtracking
// regex: "abab" does NOT match "<_>ab" (the first "ab" occurrence
// sits at the cursor → empty wildcard → fail), while `^.+ab$` would
// match. The lowering therefore renders the cursor walk literally as
// a ClickHouse arrayFold over the literal runs:
//
//	arrayFold((acc, lit, mg) -> multiIf(
//	        acc < 0, acc,
//	        position(<line>, lit, toUInt64(acc) + 1) = 0, toInt64(-1),
//	        (mg = 1) AND (position(<line>, lit, toUInt64(acc) + 1) = toUInt64(acc) + 1), toInt64(-1),
//	        toInt64(position(<line>, lit, toUInt64(acc) + 1) + length(lit)) - 1),
//	    array(<literals>), array(<must-gap flags>), toInt64(0))
//
// acc is the 0-based cursor (bytes consumed); -1 is the fail
// sentinel. position() is byte-based like bytes.Index, and its
// 3-arg form searches from a 1-based offset, so `p = acc + 1` is
// exactly the reference's `j == 0` empty-gap test. The fold result
// feeds the end-of-pattern check from rule 3.
type patternLineFilter struct {
	// Literals are the pattern's literal runs, in order.
	Literals []string
	// FirstIsLiteral reports whether the pattern's first node is a
	// literal (it is then exempt from the must-gap rule).
	FirstIsLiteral bool
	// EndsWithCapture reports whether the pattern's last node is a
	// `<_>` wildcard (the remainder must then be non-empty).
	EndsWithCapture bool
	// Empty reports an empty pattern — matches only empty lines.
	Empty bool
}

// parsePatternLineFilter validates `match` with the upstream pattern
// parser (so cerberus rejects exactly the shapes reference Loki
// rejects: named captures, consecutive wildcards, malformed exprs)
// and derives the literal-run structure.
//
// Structure derivation: after ParseLineFilter validation the only
// capture form left in the pattern is the literal token `<_>` —
// upstream's grammar merges adjacent literal runes into one run and
// alternates them with captures — so logpattern.ParseLiterals supplies
// the ordered literal runs and a prefix / suffix probe against the
// original string classifies the first / last node.
func parsePatternLineFilter(match string) (patternLineFilter, error) {
	if _, err := logpattern.ParseLineFilter([]byte(match)); err != nil {
		return patternLineFilter{}, fmt.Errorf("logql: invalid pattern line filter %q: %w", match, err)
	}
	if match == "" {
		return patternLineFilter{Empty: true}, nil
	}
	rawLits, err := logpattern.ParseLiterals(match)
	if err != nil {
		return patternLineFilter{}, fmt.Errorf("logql: invalid pattern line filter %q: %w", match, err)
	}
	lits := make([]string, len(rawLits))
	for i, l := range rawLits {
		lits[i] = string(l)
	}
	out := patternLineFilter{Literals: lits}
	if len(lits) == 0 {
		// Validation passed and there are no literals: the pattern is a
		// single `<_>` (consecutive captures are rejected) — matches
		// every non-empty line.
		out.EndsWithCapture = true
		return out, nil
	}
	out.FirstIsLiteral = strings.HasPrefix(match, lits[0])
	out.EndsWithCapture = !strings.HasSuffix(match, lits[len(lits)-1])
	return out, nil
}

// patternLineFilterExpr lowers one `|>` (negated=false) / `!>`
// (negated=true) line filter to a predicate over `body`.
func patternLineFilterExpr(match string, negated bool, body chplan.Expr) (chplan.Expr, error) {
	p, err := parsePatternLineFilter(match)
	if err != nil {
		return nil, err
	}
	pred := p.expr(body)
	if negated {
		pred = notExpr(pred)
	}
	return pred, nil
}

// expr renders the Test() walk for one parsed pattern over `line`.
func (p patternLineFilter) expr(line chplan.Expr) chplan.Expr {
	lineLen := &chplan.FuncCall{Name: "length", Args: []chplan.Expr{line}}
	if p.Empty {
		// Empty pattern ⇔ empty line.
		return &chplan.Binary{Op: chplan.OpEq, Left: lineLen, Right: &chplan.LitInt{V: 0}}
	}
	if len(p.Literals) == 0 {
		// Pure `<_>`: the cursor never moves off 0 and the pattern ends
		// on a wildcard — matches exactly the non-empty lines. Rendered
		// directly (an arrayFold over an empty `array()` has no element
		// type for CH to bind the lambda against).
		return &chplan.Binary{Op: chplan.OpNe, Left: lineLen, Right: &chplan.LitInt{V: 0}}
	}

	cursor := p.foldExpr(line)
	ok := &chplan.Binary{Op: chplan.OpGe, Left: cursor, Right: &chplan.LitInt{V: 0}}

	endOp := chplan.OpEq // ends on a literal: cursor must sit at end-of-line
	if p.EndsWithCapture {
		endOp = chplan.OpNe // ends on `<_>`: remainder must be non-empty
	}
	endCheck := &chplan.Binary{Op: endOp, Left: p.foldExpr(line), Right: lineLen}

	return &chplan.Binary{Op: chplan.OpAnd, Left: ok, Right: endCheck}
}

// foldExpr renders the arrayFold cursor walk. Returns the final
// 0-based cursor as Int64, or -1 when any literal failed to match
// under the gap rules. With no literals the fold returns its init (0)
// — the pure-`<_>` pattern — and the end check alone decides.
func (p patternLineFilter) foldExpr(line chplan.Expr) chplan.Expr {
	const (
		accParam = "_cerb_acc"
		litParam = "_cerb_lit"
		gapParam = "_cerb_mg"
	)
	acc := func() chplan.Expr { return &chplan.BareIdent{Name: accParam} }
	lit := func() chplan.Expr { return &chplan.BareIdent{Name: litParam} }
	mustGap := func() chplan.Expr { return &chplan.BareIdent{Name: gapParam} }
	fail := func() chplan.Expr {
		return &chplan.FuncCall{Name: "toInt64", Args: []chplan.Expr{&chplan.LitInt{V: -1}}}
	}
	// 1-based search start: cursor + 1. The greatest() clamp keeps the
	// UInt64 cast in-range on the failed-sentinel branch even when CH
	// evaluates multiIf arms eagerly (short_circuit_function_evaluation
	// is a setting, not a guarantee; toUInt64(-1)+1 would wrap to 0,
	// which position() rejects as a start offset).
	searchFrom := func() chplan.Expr {
		return &chplan.Binary{
			Op: chplan.OpAdd,
			Left: &chplan.FuncCall{Name: "toUInt64", Args: []chplan.Expr{
				&chplan.FuncCall{Name: "greatest", Args: []chplan.Expr{acc(), &chplan.LitInt{V: 0}}},
			}},
			Right: &chplan.LitInt{V: 1},
		}
	}
	// position(line, lit, cursor + 1) — byte-based, 0 when not found.
	pos := func() chplan.Expr {
		return &chplan.FuncCall{Name: "position", Args: []chplan.Expr{line, lit(), searchFrom()}}
	}

	litArgs := make([]chplan.Expr, 0, len(p.Literals))
	gapArgs := make([]chplan.Expr, 0, len(p.Literals))
	for i, l := range p.Literals {
		litArgs = append(litArgs, &chplan.LitString{V: l})
		mg := int64(1)
		if i == 0 && p.FirstIsLiteral {
			// The pattern's first node: exempt from the empty-gap rule
			// (reference Test only applies `j == 0 → fail` for i != 0).
			mg = 0
		}
		gapArgs = append(gapArgs, &chplan.LitInt{V: mg})
	}

	body := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			// Already failed: sticky.
			&chplan.Binary{Op: chplan.OpLt, Left: acc(), Right: &chplan.LitInt{V: 0}},
			acc(),
			// Literal not found.
			&chplan.Binary{Op: chplan.OpEq, Left: pos(), Right: &chplan.LitInt{V: 0}},
			fail(),
			// Found at the cursor with a mandatory gap: empty wildcard.
			&chplan.Binary{
				Op:   chplan.OpAnd,
				Left: &chplan.Binary{Op: chplan.OpEq, Left: mustGap(), Right: &chplan.LitInt{V: 1}},
				Right: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  pos(),
					Right: searchFrom(),
				},
			},
			fail(),
			// Advance: cursor = (position - 1) + length(lit), 0-based.
			&chplan.Binary{
				Op: chplan.OpSub,
				Left: &chplan.FuncCall{Name: "toInt64", Args: []chplan.Expr{
					&chplan.Binary{
						Op:    chplan.OpAdd,
						Left:  pos(),
						Right: &chplan.FuncCall{Name: "length", Args: []chplan.Expr{lit()}},
					},
				}},
				Right: &chplan.LitInt{V: 1},
			},
		},
	}

	return &chplan.FuncCall{
		Name: "arrayFold",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{accParam, litParam, gapParam}, Body: body},
			&chplan.FuncCall{Name: "array", Args: litArgs},
			&chplan.FuncCall{Name: "array", Args: gapArgs},
			&chplan.FuncCall{Name: "toInt64", Args: []chplan.Expr{&chplan.LitInt{V: 0}}},
		},
	}
}
