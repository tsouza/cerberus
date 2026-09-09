package lsyntax

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// maxInputSize bounds the accepted query length, mirroring the upstream
// LogQL parser's guard against pathological inputs.
const maxInputSize = 131072

// ParseExpr parses a LogQL expression and validates it.
func ParseExpr(input string) (Expr, error) {
	expr, err := ParseExprWithoutValidation(input)
	if err != nil {
		return nil, err
	}
	if err := validateExpr(expr); err != nil {
		return nil, err
	}
	return expr, nil
}

// ParseExprAllowingEmptyCompatibleMatchers parses a LogQL expression and
// runs the SAME validation walk [ParseExpr] runs, with exactly one rule
// short-circuited: [validateMatchers]'s "at least one non-empty matcher"
// rejection.
//
// This is the entry point cerberus's permissive gateway contract
// (logql.ParseExprPermissive) needs. Retrying through
// [ParseExprWithoutValidation] instead dropped the whole walk, so ONE
// intentionally-relaxed rule silently relaxed every other parse-time
// rejection with it: the `err` stashed on BinOpExpr / LiteralExpr /
// VectorExpr / VectorAggregationExpr / LabelReplaceExpr, the
// sort/sort_desc grouping rule, and every rejection reachable only
// through Selector() ("grouping not allowed for %s", "invalid
// aggregation %s with unwrap", ...). Those fields are unexported and the
// lowering never calls Selector(), so nothing downstream re-raised them
// — the query was simply accepted (cerberus issue #3183).
func ParseExprAllowingEmptyCompatibleMatchers(input string) (Expr, error) {
	expr, err := ParseExprWithoutValidation(input)
	if err != nil {
		return nil, err
	}
	if err := (exprValidator{allowEmptyCompatibleMatchers: true}).expr(expr); err != nil {
		return nil, err
	}
	return expr, nil
}

// ParseExprWithoutValidation parses a LogQL expression without running
// ANY of the post-parse validation walk. Parse-time errors stashed on
// AST nodes (e.g. a bad label_replace regex) are populated but NOT
// raised: they live in unexported fields, so a caller that skips the
// walk accepts every shape the walk would have rejected.
//
// Callers wanting only the empty-compatible-matcher rule relaxed want
// [ParseExprAllowingEmptyCompatibleMatchers] instead.
func ParseExprWithoutValidation(input string) (expr Expr, err error) {
	if len(input) >= maxInputSize {
		return nil, NewParseError(fmt.Sprintf("input size too long (%d > %d)", len(input), maxInputSize), 0, 0)
	}
	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(ParseError); ok {
				expr, err = nil, pe
				return
			}
			if e, ok := r.(error); ok {
				expr, err = nil, NewParseError(e.Error(), 0, 0)
				return
			}
			expr, err = nil, NewParseError(fmt.Sprintf("%v", r), 0, 0)
		}
	}()

	toks, lerr := lex(input)
	if lerr != nil {
		return nil, lerr
	}
	p := &parser{toks: toks}
	e := p.parseExpr(0)
	if p.cur().kind != tkEOF {
		return nil, NewParseError(fmt.Sprintf("syntax error: unexpected trailing tokens near %q", p.tokenText(p.cur())), 0, 0)
	}
	return e, nil
}

// ------------------------------------------------------------------
// validation
// ------------------------------------------------------------------

// errEmptyCompatibleMatcherRejected is returned when a selector has no
// matcher that constrains a non-empty value. The error text is
// deliberately matched to upstream Loki for wire compatibility: it is
// returned verbatim to clients, and cerberus's permissive parse path
// (logql.ParseExprPermissive) keys its retry on the "empty-compatible"
// substring, so the wording must not drift.
const errEmptyCompatibleMatcherRejected = "queries require at least one regexp or equality matcher that does not have an empty-compatible value. For instance, app=~\".*\" does not meet this requirement, but app=~\".+\" will"

// exprValidator runs the post-parse validation walk. It carries exactly
// one knob so that the strict and permissive entry points cannot drift:
// both run the same code, and the difference between them is visible as
// a single field rather than as two parallel walks.
type exprValidator struct {
	// allowEmptyCompatibleMatchers short-circuits [validateMatchers]
	// alone. Every other rule in the walk still applies. See
	// [ParseExprAllowingEmptyCompatibleMatchers] for why this is a
	// field on the shared walk and not a second walk.
	allowEmptyCompatibleMatchers bool
}

func validateExpr(expr Expr) error { return exprValidator{}.expr(expr) }

func (v exprValidator) expr(expr Expr) error {
	switch e := expr.(type) {
	case SampleExpr:
		return v.sampleExpr(e)
	case LogSelectorExpr:
		return v.logSelectorExpr(e)
	default:
		return NewParseError(fmt.Sprintf("unexpected expression type: %T", e), 0, 0)
	}
}

func (v exprValidator) sampleExpr(expr SampleExpr) error {
	switch e := expr.(type) {
	case *BinOpExpr:
		if e.err != nil {
			return e.err
		}
		if err := v.sampleExpr(e.SampleExpr); err != nil {
			return err
		}
		return v.sampleExpr(e.RHS)
	case *LiteralExpr:
		return e.err
	case *VectorExpr:
		return e.err
	case *VectorAggregationExpr:
		if e.err != nil {
			return e.err
		}
		if e.Operation == OpTypeSort || e.Operation == OpTypeSortDesc {
			if err := validateSortGrouping(e.Grouping); err != nil {
				return err
			}
		}
		return v.sampleExpr(e.Left)
	case *LabelReplaceExpr:
		if e.err != nil {
			return e.err
		}
		return v.sampleExpr(e.Left)
	default:
		sel, err := e.Selector()
		if err != nil {
			return err
		}
		return v.logSelectorExpr(sel)
	}
}

func (v exprValidator) logSelectorExpr(expr LogSelectorExpr) error {
	switch expr.(type) {
	case *VectorExpr:
		return nil
	default:
		if v.allowEmptyCompatibleMatchers {
			return nil
		}
		return validateMatchers(expr.Matchers())
	}
}

func validateSortGrouping(g *Grouping) error {
	if g != nil && len(g.Groups) > 0 {
		return NewParseError("sort and sort_desc doesn't allow grouping by", 0, 0)
	}
	return nil
}

// validateMatchers rejects a selector whose matchers are all
// empty-compatible (would match every stream).
func validateMatchers(matchers []*labels.Matcher) error {
	for _, m := range matchers {
		if !matcherEmptyCompatible(m) {
			return nil
		}
	}
	return NewParseError(errEmptyCompatibleMatcherRejected, 0, 0)
}

// matcherEmptyCompatible reports whether a matcher also matches the empty
// string — i.e. it does not, on its own, constrain the stream set. This
// mirrors the upstream SplitFiltersAndMatchers heuristic: equality/regexp
// matchers that match "" are treated as line-filter candidates, not
// stream constraints.
func matcherEmptyCompatible(m *labels.Matcher) bool {
	switch m.Type {
	case labels.MatchEqual, labels.MatchRegexp:
		return m.Matches("")
	default:
		// `!=` / `!~` against a value: if it matches "" it doesn't
		// constrain a present label either.
		return m.Matches("")
	}
}

// ------------------------------------------------------------------
// parser core
// ------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
}

func (p *parser) cur() token { return p.toks[p.pos] }
func (p *parser) peek(n int) token {
	i := p.pos + n
	if i >= len(p.toks) {
		return p.toks[len(p.toks)-1] // EOF
	}
	return p.toks[i]
}

func (p *parser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) at(k tokenKind) bool { return p.cur().kind == k }

func (p *parser) expect(k tokenKind, what string) token {
	if p.cur().kind != k {
		p.errf("syntax error: expected %s", what)
	}
	return p.advance()
}

func (p *parser) errf(format string, args ...interface{}) {
	panic(NewParseError(fmt.Sprintf(format, args...), 0, 0))
}

func (p *parser) tokenText(t token) string {
	if t.str != "" {
		return t.str
	}
	return tokenKindName(t.kind)
}

// tokenKindName renders a token kind for error messages.
func tokenKindName(k tokenKind) string {
	switch k {
	case tkEOF:
		return "end of input"
	case tkString:
		return "string"
	case tkNumber:
		return "number"
	case tkDuration:
		return "duration"
	case tkBytes:
		return "bytes"
	case tkRange:
		return "range"
	case tkOpenBrace:
		return "{"
	case tkCloseBrace:
		return "}"
	case tkOpenParen:
		return "("
	case tkCloseParen:
		return ")"
	case tkComma:
		return ","
	case tkPipe:
		return "|"
	default:
		return "token"
	}
}

// ------------------------------------------------------------------
// expression grammar (Pratt for binary operators)
// ------------------------------------------------------------------

func binOpInfo(k tokenKind) (op string, prec int, rightAssoc, ok bool) {
	switch k {
	case tkOr:
		return OpTypeOr, 1, false, true
	case tkAnd:
		return OpTypeAnd, 2, false, true
	case tkUnless:
		return OpTypeUnless, 2, false, true
	case tkCmpEq:
		return OpTypeCmpEQ, 3, false, true
	case tkNeq:
		return OpTypeNEQ, 3, false, true
	case tkLt:
		return OpTypeLT, 3, false, true
	case tkLte:
		return OpTypeLTE, 3, false, true
	case tkGt:
		return OpTypeGT, 3, false, true
	case tkGte:
		return OpTypeGTE, 3, false, true
	case tkAdd:
		return OpTypeAdd, 4, false, true
	case tkSub:
		return OpTypeSub, 4, false, true
	case tkMul:
		return OpTypeMul, 5, false, true
	case tkDiv:
		return OpTypeDiv, 5, false, true
	case tkMod:
		return OpTypeMod, 5, false, true
	case tkPow:
		return OpTypePow, 6, true, true
	}
	return "", 0, false, false
}

func (p *parser) parseExpr(minPrec int) Expr {
	left := p.parseUnary()
	for {
		op, prec, rightAssoc, ok := binOpInfo(p.cur().kind)
		if !ok || prec < minPrec {
			break
		}
		p.advance() // consume operator
		opts := p.parseBinOpModifier()
		nextMin := prec + 1
		if rightAssoc {
			nextMin = prec
		}
		right := p.parseExpr(nextMin)
		left = mustNewBinOpExpr(op, opts, left, right)
	}
	return left
}

// parseBinOpModifier parses the optional `[bool] [on(...)|ignoring(...)]
// [group_left(...)|group_right(...)]` modifier that follows a binary
// operator.
func (p *parser) parseBinOpModifier() *BinOpOptions {
	opts := &BinOpOptions{VectorMatching: &VectorMatching{Card: CardOneToOne}}
	if p.at(tkBool) {
		p.advance()
		opts.ReturnBool = true
	}
	if p.at(tkOn) || p.at(tkIgnoring) {
		on := p.at(tkOn)
		p.advance()
		p.expect(tkOpenParen, "'('")
		opts.VectorMatching.MatchingLabels = p.parseLabelList()
		p.expect(tkCloseParen, "')'")
		opts.VectorMatching.On = on
		p.parseGroupModifier(opts.VectorMatching)
	}
	return opts
}

// parseGroupModifier parses the optional `group_left(...)|group_right(...)`
// clause that may follow an on/ignoring matcher, populating the cardinality
// and the include-label list on vm.
func (p *parser) parseGroupModifier(vm *VectorMatching) {
	if !p.at(tkGroupLeft) && !p.at(tkGroupRight) {
		return
	}
	right := p.at(tkGroupRight)
	p.advance()
	if right {
		vm.Card = CardOneToMany
	} else {
		vm.Card = CardManyToOne
	}
	if p.at(tkOpenParen) {
		p.advance()
		vm.Include = p.parseLabelList()
		p.expect(tkCloseParen, "')'")
	}
}

// parseLabelList parses a possibly-empty comma-separated list of label
// names (used by on/ignoring/group_left/group_right). It leaves the
// closing paren for the caller.
func (p *parser) parseLabelList() []string {
	var out []string
	if p.at(tkCloseParen) {
		return out
	}
	for {
		out = append(out, p.expect(tkIdentifier, "label name").str)
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	return out
}

func (p *parser) parseUnary() Expr {
	// Signed numeric literal: `+5` / `-5`.
	if p.at(tkAdd) || p.at(tkSub) {
		if p.peek(1).kind == tkNumber {
			invert := p.at(tkSub)
			p.advance() // sign
			num := p.advance()
			return mustNewLiteralExpr(num.str, invert)
		}
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() Expr {
	switch p.cur().kind {
	case tkNumber:
		num := p.advance()
		return mustNewLiteralExpr(num.str, false)
	case tkOpenBrace:
		return p.parseLogExpr()
	case tkOpenParen:
		p.advance()
		inner := p.parseExpr(0)
		p.expect(tkCloseParen, "')'")
		return inner
	case tkRangeOp:
		return p.parseRangeAggregation()
	case tkVectorOp:
		return p.parseVectorAggregation()
	case tkLabelReplace:
		return p.parseLabelReplace()
	case tkVector:
		return p.parseVectorExpr()
	case tkVariants:
		return p.parseVariants()
	default:
		p.errf("syntax error: unexpected %s", p.tokenText(p.cur()))
		return nil
	}
}

// parseLogExpr parses a stream selector with an optional pipeline.
func (p *parser) parseLogExpr() LogSelectorExpr {
	matchers := p.parseSelector()
	sel := newMatcherExpr(matchers)
	if p.isPipelineStageStart() {
		stages := p.parsePipeline()
		return newPipelineExpr(sel, stages)
	}
	return sel
}

func (p *parser) parseSelector() []*labels.Matcher {
	p.expect(tkOpenBrace, "'{'")
	var matchers []*labels.Matcher
	if p.at(tkCloseBrace) {
		p.advance()
		return matchers
	}
	for {
		matchers = append(matchers, p.parseMatcher())
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	p.expect(tkCloseBrace, "'}'")
	return matchers
}

func (p *parser) parseMatcher() *labels.Matcher {
	id := p.expect(tkIdentifier, "label name").str
	var mt labels.MatchType
	switch p.cur().kind {
	case tkEq:
		mt = labels.MatchEqual
	case tkNeq:
		mt = labels.MatchNotEqual
	case tkRe:
		mt = labels.MatchRegexp
	case tkNre:
		mt = labels.MatchNotRegexp
	default:
		p.errf("syntax error: expected matcher operator after %q", id)
	}
	p.advance()
	val := p.expect(tkString, "string").str
	return mustNewMatcher(mt, id, val)
}

// ------------------------------------------------------------------
// pipeline stages
// ------------------------------------------------------------------

func (p *parser) isPipelineStageStart() bool {
	if p.isLineFilterStart() {
		return true
	}
	return p.at(tkPipe)
}

func (p *parser) isLineFilterStart() bool {
	switch p.cur().kind {
	case tkPipeExact, tkPipeMatch, tkPipePattern, tkNeq, tkNre, tkNpa:
		return true
	}
	return false
}

func (p *parser) parsePipeline() MultiStageExpr {
	var stages MultiStageExpr
	for {
		switch {
		case p.isLineFilterStart():
			stages = append(stages, p.parseLineFilters())
		case p.at(tkPipe) && p.peek(1).kind != tkUnwrap:
			p.advance() // consume '|'
			stages = append(stages, p.parsePipeStage())
		default:
			return stages
		}
	}
}

// parsePipeStage parses a single `| ...` stage (the leading PIPE already
// consumed) that is not a line filter.
func (p *parser) parsePipeStage() StageExpr {
	switch p.cur().kind {
	case tkLogfmt:
		p.advance()
		flags := p.parseFlags()
		if p.at(tkIdentifier) {
			return newLogfmtExpressionParser(p.parseExtractionList(), flags)
		}
		return newLogfmtParserExpr(flags)
	case tkJSON:
		p.advance()
		if p.at(tkIdentifier) {
			return newJSONExpressionParser(p.parseExtractionList())
		}
		return newLabelParserExpr(OpParserTypeJSON, "")
	case tkRegexp:
		p.advance()
		param := p.expect(tkString, "regexp pattern").str
		return newLabelParserExpr(OpParserTypeRegexp, param)
	case tkUnpack:
		p.advance()
		return newLabelParserExpr(OpParserTypeUnpack, "")
	case tkPattern:
		p.advance()
		param := p.expect(tkString, "pattern").str
		return newLabelParserExpr(OpParserTypePattern, param)
	case tkLineFmt:
		p.advance()
		v := p.expect(tkString, "line_format template").str
		return newLineFmtExpr(v)
	case tkLabelFmt:
		p.advance()
		return newLabelFmtExpr(p.parseLabelsFormat())
	case tkDecolorize:
		p.advance()
		return newDecolorizeExpr()
	case tkDrop:
		p.advance()
		return newDropLabelsExpr(p.parseNamedMatchers())
	case tkKeep:
		p.advance()
		return newKeepLabelsExpr(p.parseNamedMatchers())
	case tkUnwrap:
		p.errf("syntax error: `| unwrap` is only valid inside a range aggregation")
		return nil
	default:
		return newLabelFilterExpr(p.parseLabelFilter())
	}
}

func (p *parser) parseFlags() []string {
	var flags []string
	for p.at(tkFunctionFlag) {
		flags = append(flags, p.advance().str)
	}
	return flags
}

func (p *parser) parseExtractionList() []LabelExtractionExpr {
	var exprs []LabelExtractionExpr
	for {
		id := p.expect(tkIdentifier, "label name").str
		if p.at(tkEq) {
			p.advance()
			val := p.expect(tkString, "extraction expression").str
			exprs = append(exprs, NewLabelExtractionExpr(id, val))
		} else {
			exprs = append(exprs, NewLabelExtractionExpr(id, id))
		}
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	return exprs
}

func (p *parser) parseLabelsFormat() []LabelFmt {
	var fmts []LabelFmt
	for {
		dst := p.expect(tkIdentifier, "label name").str
		p.expect(tkEq, "'='")
		switch p.cur().kind {
		case tkIdentifier:
			src := p.advance().str
			fmts = append(fmts, NewRenameLabelFmt(dst, src))
		case tkString:
			tmpl := p.advance().str
			fmts = append(fmts, NewTemplateLabelFmt(dst, tmpl))
		default:
			p.errf("syntax error: expected identifier or string in label_format")
		}
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	return fmts
}

func (p *parser) parseNamedMatchers() []NamedLabelMatcher {
	var out []NamedLabelMatcher
	for {
		id := p.expect(tkIdentifier, "label name").str
		switch p.cur().kind {
		case tkEq, tkNeq, tkRe, tkNre:
			var mt labels.MatchType
			switch p.cur().kind {
			case tkEq:
				mt = labels.MatchEqual
			case tkNeq:
				mt = labels.MatchNotEqual
			case tkRe:
				mt = labels.MatchRegexp
			case tkNre:
				mt = labels.MatchNotRegexp
			}
			p.advance()
			val := p.expect(tkString, "string").str
			out = append(out, NewNamedLabelMatcher(mustNewMatcher(mt, id, val), ""))
		default:
			out = append(out, NewNamedLabelMatcher(nil, id))
		}
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	return out
}

// ------------------------------------------------------------------
// line filters
// ------------------------------------------------------------------

func lineMatchType(k tokenKind) LineMatchType {
	switch k {
	case tkPipeExact:
		return LineMatchEqual
	case tkNeq:
		return LineMatchNotEqual
	case tkPipeMatch:
		return LineMatchRegexp
	case tkNre:
		return LineMatchNotRegexp
	case tkPipePattern:
		return LineMatchPattern
	case tkNpa:
		return LineMatchNotPattern
	}
	return LineMatchEqual
}

func (p *parser) parseLineFilters() *LineFilterExpr {
	result := p.parseLineFilter()
	for p.isLineFilterStart() {
		next := p.parseLineFilter()
		result = newNestedLineFilterExpr(result, next)
	}
	return result
}

func (p *parser) parseLineFilter() *LineFilterExpr {
	ty := lineMatchType(p.cur().kind)
	p.advance()
	var head *LineFilterExpr
	if p.at(tkIP) {
		p.advance()
		p.expect(tkOpenParen, "'('")
		s := p.expect(tkString, "string").str
		p.expect(tkCloseParen, "')'")
		head = newLineFilterExpr(ty, OpFilterIP, s)
	} else {
		s := p.expect(tkString, "string").str
		head = newLineFilterExpr(ty, "", s)
	}
	for p.at(tkOr) {
		p.advance()
		head = newOrLineFilterExpr(head, p.parseOrFilter())
	}
	return head
}

func (p *parser) parseOrFilter() *LineFilterExpr {
	if p.at(tkIP) {
		p.advance()
		p.expect(tkOpenParen, "'('")
		s := p.expect(tkString, "string").str
		p.expect(tkCloseParen, "')'")
		return newLineFilterExpr(LineMatchEqual, OpFilterIP, s)
	}
	s := p.expect(tkString, "string").str
	node := newLineFilterExpr(LineMatchEqual, "", s)
	if p.at(tkOr) {
		p.advance()
		return newOrLineFilterExpr(node, p.parseOrFilter())
	}
	return node
}

// ------------------------------------------------------------------
// label filters
// ------------------------------------------------------------------

// Label-filter operator binding powers.
//
// Upstream's grammar (pkg/logql/syntax/syntax.y) declares precedence for
// `or` and `and` — `%left OR` below `%left AND UNLESS` — but declares
// NONE for the two conjunction spellings `labelFilter COMMA labelFilter`
// and `labelFilter labelFilter` (juxtaposition). goyacc cannot compare a
// rule with no precedence against the lookahead, so it resolves every
// such conflict by its default: SHIFT.
//
// That default is not expressible as one precedence number, because it
// fires in BOTH directions — the parser shifts a comma arriving after an
// `or`, and shifts an `or` arriving after a comma. What it IS expressible
// as is an asymmetric pair of binding powers: comma and juxtaposition
// bind tighter than anything on their LEFT (so a pending `a or …`
// reduction is always deferred) and looser than anything on their RIGHT
// (so the right operand swallows the rest of the filter). Hence
// [labelFilterLbp] / [labelFilterRbp] rather than a single precedence.
//
// The two spellings therefore come out RIGHT-associative and, from the
// left, LOOSEST — the opposite of the tighter-than-`or`,
// left-associative reading cerberus used to give them. The user-visible
// difference: `| a="1", b="2" or c="3"` is `And(a, Or(b, c))` upstream
// and was `Or(And(a, b), c)` here, which selects a different row set
// (cerberus issue #3183). Upstream pins the shape in
// pkg/logql/syntax/parser_test.go's
// `| foo="bar" buzz!="blip", blop=~"boop" or fuzz==5` case:
// `And(foo, And(buzz, Or(blop, fuzz)))`.
const (
	// labelFilterOrLbp / labelFilterOrRbp: `%left OR`. Left
	// associativity is rbp = lbp + 1.
	labelFilterOrLbp = 1
	labelFilterOrRbp = 2

	// labelFilterAndLbp / labelFilterAndRbp: `%left AND`, one level
	// above OR.
	labelFilterAndLbp = 3
	labelFilterAndRbp = 4

	// labelFilterConjLbp / labelFilterConjRbp: `,` and juxtaposition.
	// The lbp is above every declared operator so the conflict always
	// resolves as a shift; the rbp is below every one of them so the
	// right operand extends maximally.
	labelFilterConjLbp = 5
	labelFilterConjRbp = 0
)

func (p *parser) parseLabelFilter() LabelFilterer {
	return p.parseLabelFilterAt(0)
}

// parseLabelFilterAt is the precedence-climbing loop. minBp is the
// binding power an operator must reach to be consumed here rather than
// returned to the caller.
func (p *parser) parseLabelFilterAt(minBp int) LabelFilterer {
	left := p.parseLabelFilterAtom()
	for {
		lbp, rbp, isOr, ok := p.labelFilterOp()
		if !ok || lbp < minBp {
			return left
		}
		// `,`, `and` and `or` are spelled tokens and are consumed;
		// juxtaposition is not a token at all, so there is nothing to
		// advance past — the next atom starts immediately.
		if p.at(tkOr) || p.at(tkAnd) || p.at(tkComma) {
			p.advance()
		}
		right := p.parseLabelFilterAt(rbp)
		if isOr {
			left = NewOrLabelFilter(left, right)
		} else {
			left = NewAndLabelFilter(left, right)
		}
	}
}

// labelFilterOp classifies the token at the cursor as a label-filter
// infix operator and reports its binding powers. The final bool reports
// whether the filter continues at all.
func (p *parser) labelFilterOp() (lbp, rbp int, isOr, ok bool) {
	switch {
	case p.at(tkOr):
		return labelFilterOrLbp, labelFilterOrRbp, true, true
	case p.at(tkAnd):
		return labelFilterAndLbp, labelFilterAndRbp, false, true
	case p.at(tkComma):
		return labelFilterConjLbp, labelFilterConjRbp, false, true
	case p.at(tkIdentifier) || p.at(tkOpenParen):
		// Juxtaposition: two filters side by side are an implicit `and`,
		// with the same binding powers as the comma spelling.
		return labelFilterConjLbp, labelFilterConjRbp, false, true
	default:
		return 0, 0, false, false
	}
}

func (p *parser) parseLabelFilterAtom() LabelFilterer {
	if p.at(tkOpenParen) {
		p.advance()
		f := p.parseLabelFilter()
		p.expect(tkCloseParen, "')'")
		return f
	}
	id := p.expect(tkIdentifier, "label name").str
	opk := p.cur().kind
	switch opk {
	case tkEq, tkNeq:
		p.advance()
		switch p.cur().kind {
		case tkString:
			val := p.advance().str
			return NewStringLabelFilter(mustNewMatcher(stringMatchType(opk), id, val))
		case tkIP:
			p.advance()
			p.expect(tkOpenParen, "'('")
			s := p.expect(tkString, "string").str
			p.expect(tkCloseParen, "')'")
			return NewIPLabelFilter(s, id, ipFilterType(opk))
		case tkDuration:
			d := p.advance().dur
			return NewDurationLabelFilter(labelFilterType(opk), id, d)
		case tkBytes:
			b := p.advance().bytes
			return NewBytesLabelFilter(labelFilterType(opk), id, b)
		case tkNumber, tkAdd, tkSub:
			return NewNumericLabelFilter(labelFilterType(opk), id, p.parseSignedFloat())
		default:
			p.errf("syntax error: unexpected value in label filter for %q", id)
		}
	case tkRe, tkNre:
		p.advance()
		val := p.expect(tkString, "string").str
		return NewStringLabelFilter(mustNewMatcher(stringMatchType(opk), id, val))
	case tkGt, tkGte, tkLt, tkLte, tkCmpEq:
		p.advance()
		switch p.cur().kind {
		case tkDuration:
			d := p.advance().dur
			return NewDurationLabelFilter(labelFilterType(opk), id, d)
		case tkBytes:
			b := p.advance().bytes
			return NewBytesLabelFilter(labelFilterType(opk), id, b)
		case tkNumber, tkAdd, tkSub:
			return NewNumericLabelFilter(labelFilterType(opk), id, p.parseSignedFloat())
		default:
			p.errf("syntax error: unexpected value in label filter for %q", id)
		}
	default:
		p.errf("syntax error: expected a label-filter operator after %q", id)
	}
	return nil
}

func (p *parser) parseSignedFloat() float64 {
	invert := false
	if p.at(tkAdd) || p.at(tkSub) {
		invert = p.at(tkSub)
		p.advance()
	}
	num := p.expect(tkNumber, "number").str
	lit := mustNewLiteralExpr(num, invert)
	v, err := lit.Value()
	if err != nil {
		panic(err)
	}
	return v
}

func stringMatchType(k tokenKind) labels.MatchType {
	switch k {
	case tkEq:
		return labels.MatchEqual
	case tkNeq:
		return labels.MatchNotEqual
	case tkRe:
		return labels.MatchRegexp
	case tkNre:
		return labels.MatchNotRegexp
	}
	return labels.MatchEqual
}

func ipFilterType(k tokenKind) LabelFilterType {
	if k == tkNeq {
		return LabelFilterNotEqual
	}
	return LabelFilterEqual
}

func labelFilterType(k tokenKind) LabelFilterType {
	switch k {
	case tkGt:
		return LabelFilterGreaterThan
	case tkGte:
		return LabelFilterGreaterThanOrEqual
	case tkLt:
		return LabelFilterLesserThan
	case tkLte:
		return LabelFilterLesserThanOrEqual
	case tkNeq:
		return LabelFilterNotEqual
	case tkEq, tkCmpEq:
		return LabelFilterEqual
	}
	return LabelFilterEqual
}

// ------------------------------------------------------------------
// metric forms
// ------------------------------------------------------------------

func (p *parser) parseRangeAggregation() Expr {
	op := p.advance().str // range op
	p.expect(tkOpenParen, "'('")
	var param *string
	if p.at(tkNumber) && p.peek(1).kind == tkComma {
		n := p.advance().str
		p.advance() // comma
		param = &n
	}
	lr := p.parseLogRange()
	p.expect(tkCloseParen, "')'")
	var grouping *Grouping
	if p.isGroupingStart() {
		grouping = p.parseGrouping()
	}
	return newRangeAggregationExpr(lr, op, grouping, param)
}

func (p *parser) parseVectorAggregation() Expr {
	op := p.advance().str // vector op
	var grouping *Grouping
	if p.isGroupingStart() {
		grouping = p.parseGrouping()
	}
	p.expect(tkOpenParen, "'('")
	var param *string
	if p.at(tkNumber) && p.peek(1).kind == tkComma {
		n := p.advance().str
		p.advance() // comma
		param = &n
	}
	inner := p.parseExpr(0)
	p.expect(tkCloseParen, "')'")
	if grouping == nil && p.isGroupingStart() {
		grouping = p.parseGrouping()
	}
	left, ok := inner.(SampleExpr)
	if !ok {
		p.errf("syntax error: %s() expects a metric expression", op)
	}
	return mustNewVectorAggregationExpr(left, op, grouping, param)
}

func (p *parser) parseLabelReplace() Expr {
	p.advance() // label_replace
	p.expect(tkOpenParen, "'('")
	inner := p.parseExpr(0)
	p.expect(tkComma, "','")
	dst := p.expect(tkString, "string").str
	p.expect(tkComma, "','")
	repl := p.expect(tkString, "string").str
	p.expect(tkComma, "','")
	src := p.expect(tkString, "string").str
	p.expect(tkComma, "','")
	regex := p.expect(tkString, "string").str
	p.expect(tkCloseParen, "')'")
	left, ok := inner.(SampleExpr)
	if !ok {
		p.errf("syntax error: label_replace() expects a metric expression")
	}
	return mustNewLabelReplaceExpr(left, dst, repl, src, regex)
}

func (p *parser) parseVectorExpr() Expr {
	p.advance() // vector
	p.expect(tkOpenParen, "'('")
	n := p.expect(tkNumber, "number").str
	p.expect(tkCloseParen, "')'")
	return NewVectorExpr(n)
}

func (p *parser) parseVariants() Expr {
	p.advance() // variants
	p.expect(tkOpenParen, "'('")
	variants := p.parseMetricExprs()
	p.expect(tkCloseParen, "')'")
	p.expect(tkOf, "'of'")
	p.expect(tkOpenParen, "'('")
	lr := p.parseLogRange()
	p.expect(tkCloseParen, "')'")
	return newVariantsExpr(variants, lr)
}

func (p *parser) parseMetricExprs() []SampleExpr {
	var out []SampleExpr
	for {
		inner := p.parseExpr(0)
		se, ok := inner.(SampleExpr)
		if !ok {
			p.errf("syntax error: variants() expects metric expressions")
		}
		out = append(out, se)
		if p.at(tkComma) {
			p.advance()
			continue
		}
		break
	}
	return out
}

func (p *parser) isGroupingStart() bool { return p.at(tkBy) || p.at(tkWithout) }

func (p *parser) parseGrouping() *Grouping {
	without := p.at(tkWithout)
	p.advance() // by/without
	p.expect(tkOpenParen, "'('")
	var groups []string
	if !p.at(tkCloseParen) {
		for {
			groups = append(groups, p.expect(tkIdentifier, "label name").str)
			if p.at(tkComma) {
				p.advance()
				continue
			}
			break
		}
	}
	p.expect(tkCloseParen, "')'")
	return &Grouping{Groups: groups, Without: without}
}

// ------------------------------------------------------------------
// log range
// ------------------------------------------------------------------

func (p *parser) parseLogRange() *LogRangeExpr {
	leadingParen := false
	if p.at(tkOpenParen) {
		leadingParen = true
		p.advance()
	}
	matchers := p.parseSelector()
	var left LogSelectorExpr = newMatcherExpr(matchers)

	var stages MultiStageExpr
	var unwrap *UnwrapExpr
	var interval time.Duration
	haveInterval := false
	var offset *OffsetExpr

loop:
	for {
		switch {
		case p.isLineFilterStart():
			stages = append(stages, p.parseLineFilters())
		case p.at(tkPipe) && p.peek(1).kind == tkUnwrap:
			unwrap = p.parseUnwrap()
		case p.at(tkPipe):
			p.advance()
			stages = append(stages, p.parsePipeStage())
		case p.at(tkRange):
			interval = p.advance().dur
			haveInterval = true
		case p.at(tkOffset):
			offset = p.parseOffset()
		case leadingParen && p.at(tkCloseParen):
			p.advance()
			leadingParen = false
		default:
			break loop
		}
	}

	if !haveInterval {
		p.errf("syntax error: missing range `[...]` in range aggregation")
	}
	if len(stages) > 0 {
		left = newPipelineExpr(left.(*MatchersExpr), stages)
	}
	return newLogRange(left, interval, unwrap, offset)
}

func (p *parser) parseUnwrap() *UnwrapExpr {
	p.expect(tkPipe, "'|'")
	p.expect(tkUnwrap, "'unwrap'")
	var u *UnwrapExpr
	if p.at(tkConvOp) {
		op := p.advance().str
		p.expect(tkOpenParen, "'('")
		id := p.expect(tkIdentifier, "label name").str
		p.expect(tkCloseParen, "')'")
		u = newUnwrapExpr(id, op)
	} else {
		id := p.expect(tkIdentifier, "label name").str
		u = newUnwrapExpr(id, "")
	}
	// Trailing `| <labelFilter>` post-filters.
	for p.at(tkPipe) && (p.peek(1).kind == tkIdentifier || p.peek(1).kind == tkOpenParen) {
		p.advance() // '|'
		u.addPostFilter(p.parseLabelFilter())
	}
	return u
}

func (p *parser) parseOffset() *OffsetExpr {
	p.expect(tkOffset, "'offset'")
	d := p.expect(tkDuration, "duration").dur
	return newOffsetExpr(d)
}
