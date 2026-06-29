package lsyntax

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/util/strutil"
)

// tokenKind enumerates the LogQL lexical tokens. The set mirrors the
// terminals of the LogQL grammar.
type tokenKind int

const (
	tkEOF tokenKind = iota
	tkError

	// literals / names
	tkIdentifier
	tkString
	tkNumber       // numeric literal (str holds the text)
	tkDuration     // duration literal (dur)
	tkBytes        // byte-size literal (bytes)
	tkRange        // `[5m]` range (dur)
	tkFunctionFlag // `--strict` / `--keep-empty` (str)

	// punctuation
	tkComma
	tkDot
	tkOpenBrace
	tkCloseBrace
	tkOpenParen
	tkCloseParen
	tkOpenBracket
	tkCloseBracket
	tkPipe

	// match / comparison operators
	tkEq          // =
	tkNeq         // !=
	tkRe          // =~
	tkNre         // !~
	tkNpa         // !>
	tkCmpEq       // ==
	tkGt          // >
	tkGte         // >=
	tkLt          // <
	tkLte         // <=
	tkPipeExact   // |=
	tkPipeMatch   // |~
	tkPipePattern // |>

	// arithmetic / logical operators
	tkAdd
	tkSub
	tkMul
	tkDiv
	tkMod
	tkPow
	tkOr
	tkAnd
	tkUnless

	// modifier keywords
	tkBy
	tkWithout
	tkBool
	tkOffset
	tkOn
	tkIgnoring
	tkGroupLeft
	tkGroupRight

	// pipeline keywords
	tkJSON
	tkRegexp
	tkLogfmt
	tkUnpack
	tkPattern
	tkLabelFmt
	tkLineFmt
	tkDecolorize
	tkDrop
	tkKeep
	tkUnwrap

	// variants
	tkVariants
	tkOf

	// function-call tokens (str holds the canonical op string)
	tkRangeOp
	tkVectorOp
	tkConvOp
	tkVector
	tkLabelReplace
	tkIP
)

type token struct {
	kind  tokenKind
	str   string
	dur   time.Duration
	bytes uint64
	pos   int // byte offset, for error reporting
}

// keywordKinds maps the unconditional keyword identifiers to their
// token kinds. These words are always keywords and never label names.
var keywordKinds = map[string]tokenKind{
	"by":                tkBy,
	"without":           tkWithout,
	"bool":              tkBool,
	OpOffset:            tkOffset,
	OpOn:                tkOn,
	OpIgnoring:          tkIgnoring,
	OpGroupLeft:         tkGroupLeft,
	OpGroupRight:        tkGroupRight,
	OpTypeOr:            tkOr,
	OpTypeAnd:           tkAnd,
	OpTypeUnless:        tkUnless,
	OpParserTypeJSON:    tkJSON,
	OpParserTypeRegexp:  tkRegexp,
	OpParserTypeLogfmt:  tkLogfmt,
	OpParserTypeUnpack:  tkUnpack,
	OpParserTypePattern: tkPattern,
	OpFmtLabel:          tkLabelFmt,
	OpFmtLine:           tkLineFmt,
	OpDecolorize:        tkDecolorize,
	OpDrop:              tkDrop,
	OpKeep:              tkKeep,
	OpUnwrap:            tkUnwrap,
	OpVariants:          tkVariants,
	VariantsOf:          tkOf,
}

// rangeOps / vectorOps / convOps classify the function-call identifiers
// that are only treated as functions when followed by `(` (or
// `by/without (`). Otherwise they are ordinary label identifiers.
var rangeOps = map[string]struct{}{
	OpRangeTypeCount: {}, OpRangeTypeRate: {}, OpRangeTypeRateCounter: {},
	OpRangeTypeBytes: {}, OpRangeTypeBytesRate: {}, OpRangeTypeAvg: {},
	OpRangeTypeSum: {}, OpRangeTypeMin: {}, OpRangeTypeMax: {},
	OpRangeTypeStdvar: {}, OpRangeTypeStddev: {}, OpRangeTypeQuantile: {},
	OpRangeTypeFirst: {}, OpRangeTypeLast: {}, OpRangeTypeAbsent: {},
}

var vectorOps = map[string]struct{}{
	OpTypeSum: {}, OpTypeAvg: {}, OpTypeMax: {}, OpTypeMin: {},
	OpTypeCount: {}, OpTypeStddev: {}, OpTypeStdvar: {}, OpTypeBottomK: {},
	OpTypeTopK: {}, OpTypeSort: {}, OpTypeSortDesc: {}, OpTypeApproxTopK: {},
}

var convOps = map[string]tokenKind{
	OpConvBytes: tkConvOp, OpConvDuration: tkConvOp, OpConvDurationSeconds: tkConvOp,
}

// durationRune reports whether r can appear in a duration unit.
func durationRune(r rune) bool {
	switch r {
	case 'n', 'u', 'µ', 'm', 's', 'h', 'd', 'w', 'y':
		return true
	}
	return false
}

// bytesRune reports whether r can appear in a byte-size unit.
func bytesRune(r rune) bool {
	switch r {
	case 'B', 'i', 'k', 'K', 'M', 'G', 'T', 'P':
		return true
	}
	return false
}

// parseDurationText parses a LogQL duration, accepting both Prometheus
// model durations (e.g. `1d`, `2w`) and Go durations (e.g. `1h30m`).
func parseDurationText(d string) (time.Duration, bool) {
	if pd, err := model.ParseDuration(d); err == nil {
		return time.Duration(pd), true
	}
	if gd, err := time.ParseDuration(d); err == nil {
		return gd, true
	}
	return 0, false
}

// lexer tokenizes a LogQL query string into a flat token slice.
type lexer struct {
	src  string
	pos  int
	toks []token
	err  *ParseError
}

func lex(input string) ([]token, error) {
	l := &lexer{src: input}
	l.run()
	if l.err != nil {
		return nil, *l.err
	}
	return l.toks, nil
}

func (l *lexer) fail(msg string) {
	if l.err == nil {
		line, col := l.lineCol()
		pe := NewParseError(msg, line, col)
		l.err = &pe
	}
}

func (l *lexer) lineCol() (int, int) {
	line, col := 1, 1
	for i := 0; i < l.pos && i < len(l.src); i++ {
		if l.src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func (l *lexer) peekByte() byte {
	if l.pos < len(l.src) {
		return l.src[l.pos]
	}
	return 0
}

func (l *lexer) peekByteAt(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

// singleCharTokens maps punctuation bytes that always stand alone (no
// multi-byte operator can start with them) to their token kind.
var singleCharTokens = map[byte]tokenKind{
	'{': tkOpenBrace,
	'}': tkCloseBrace,
	'(': tkOpenParen,
	')': tkCloseParen,
	']': tkCloseBracket,
	',': tkComma,
	'+': tkAdd,
	'*': tkMul,
	'/': tkDiv,
	'%': tkMod,
	'^': tkPow,
}

func (l *lexer) run() {
	for l.pos < len(l.src) && l.err == nil {
		c := l.src[l.pos]
		if k, ok := singleCharTokens[c]; ok {
			l.toks = append(l.toks, token{kind: k, pos: l.pos})
			l.pos++
			continue
		}
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '.' && !isDigit(l.peekByteAt(1)):
			l.toks = append(l.toks, token{kind: tkDot, pos: l.pos})
			l.pos++
		case c == '[':
			l.scanRange()
		case c == '"' || c == '`':
			l.scanString()
		case c == '=':
			l.scanFrom2('~', tkRe, '=', tkCmpEq, tkEq)
		case c == '!':
			l.scanBang()
		case c == '|':
			l.scanPipe()
		case c == '>':
			l.scanFrom1('=', tkGte, tkGt)
		case c == '<':
			l.scanFrom1('=', tkLte, tkLt)
		case c == '-':
			l.scanMinus()
		case isDigit(c) || (c == '.' && isDigit(l.peekByteAt(1))):
			l.scanNumber()
		case isIdentStart(c):
			l.scanIdent()
		default:
			// Non-ASCII identifier start (e.g. UTF-8 label names).
			r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
			if unicode.IsLetter(r) {
				l.scanIdent()
				continue
			}
			l.fail("syntax error: unexpected character " + string(rune(c)))
			return
		}
	}
	l.toks = append(l.toks, token{kind: tkEOF, pos: l.pos})
}

func (l *lexer) scanFrom2(b1 byte, k1 tokenKind, b2 byte, k2, fallback tokenKind) {
	start := l.pos
	l.pos++
	switch l.peekByte() {
	case b1:
		l.pos++
		l.toks = append(l.toks, token{kind: k1, pos: start})
	case b2:
		l.pos++
		l.toks = append(l.toks, token{kind: k2, pos: start})
	default:
		l.toks = append(l.toks, token{kind: fallback, pos: start})
	}
}

func (l *lexer) scanFrom1(b1 byte, k1, fallback tokenKind) {
	start := l.pos
	l.pos++
	if l.peekByte() == b1 {
		l.pos++
		l.toks = append(l.toks, token{kind: k1, pos: start})
		return
	}
	l.toks = append(l.toks, token{kind: fallback, pos: start})
}

func (l *lexer) scanBang() {
	start := l.pos
	l.pos++
	switch l.peekByte() {
	case '=':
		l.pos++
		l.toks = append(l.toks, token{kind: tkNeq, pos: start})
	case '~':
		l.pos++
		l.toks = append(l.toks, token{kind: tkNre, pos: start})
	case '>':
		l.pos++
		l.toks = append(l.toks, token{kind: tkNpa, pos: start})
	default:
		l.fail("syntax error: unexpected '!'")
	}
}

func (l *lexer) scanPipe() {
	start := l.pos
	l.pos++
	switch l.peekByte() {
	case '=':
		l.pos++
		l.toks = append(l.toks, token{kind: tkPipeExact, pos: start})
	case '~':
		l.pos++
		l.toks = append(l.toks, token{kind: tkPipeMatch, pos: start})
	case '>':
		l.pos++
		l.toks = append(l.toks, token{kind: tkPipePattern, pos: start})
	default:
		l.toks = append(l.toks, token{kind: tkPipe, pos: start})
	}
}

func (l *lexer) scanMinus() {
	start := l.pos
	// Parser flag: `--strict` / `--keep-empty`.
	if l.peekByteAt(1) == '-' {
		j := l.pos
		for j < len(l.src) && (isLetter(l.src[j]) || l.src[j] == '-') {
			j++
		}
		flag := l.src[l.pos:j]
		if flag == OpStrict || flag == OpKeepEmpty {
			l.pos = j
			l.toks = append(l.toks, token{kind: tkFunctionFlag, str: flag, pos: start})
			return
		}
	}
	// Negative duration: `-5m`, `-1h30m`. Try to read a duration run
	// starting with the sign; if it parses, emit a DURATION token.
	if isDigit(l.peekByteAt(1)) {
		j := l.pos + 1
		for j < len(l.src) {
			r := l.src[j]
			if isDigit(r) || durationRune(rune(r)) || r == '.' {
				j++
				continue
			}
			break
		}
		if d, ok := parseDurationText(l.src[l.pos:j]); ok {
			l.pos = j
			l.toks = append(l.toks, token{kind: tkDuration, dur: d, pos: start})
			return
		}
	}
	l.pos++
	l.toks = append(l.toks, token{kind: tkSub, pos: start})
}

func (l *lexer) scanRange() {
	start := l.pos
	l.pos++ // consume '['
	j := l.pos
	for j < len(l.src) && l.src[j] != ']' {
		j++
	}
	if j >= len(l.src) {
		l.fail("missing closing ']' in duration")
		return
	}
	inner := l.src[l.pos:j]
	d, err := model.ParseDuration(inner)
	if err != nil {
		l.pos = j
		l.fail(err.Error())
		return
	}
	l.pos = j + 1 // consume ']'
	l.toks = append(l.toks, token{kind: tkRange, dur: time.Duration(d), pos: start})
}

func (l *lexer) scanString() {
	start := l.pos
	quote := l.src[l.pos]
	j := l.pos + 1
	if quote == '`' {
		for j < len(l.src) && l.src[j] != '`' {
			j++
		}
	} else {
		for j < len(l.src) {
			if l.src[j] == '\\' {
				j += 2
				continue
			}
			if l.src[j] == '"' {
				break
			}
			j++
		}
	}
	if j >= len(l.src) {
		l.fail("literal not terminated")
		return
	}
	raw := l.src[l.pos : j+1]
	l.pos = j + 1
	if !utf8.ValidString(raw) {
		l.fail("invalid UTF-8 rune")
		return
	}
	v, err := strutil.Unquote(raw)
	if err != nil {
		l.fail(err.Error())
		return
	}
	l.toks = append(l.toks, token{kind: tkString, str: v, pos: start})
}

// scanHexRun consumes a `0x`/`0X`-prefixed run of hex digits starting at j,
// returning the index just past the last consumed byte.
func (l *lexer) scanHexRun(j int) int {
	j += 2
	for j < len(l.src) && isHexDigit(l.src[j]) {
		j++
	}
	return j
}

// scanExponent consumes an `e`/`E` exponent (with optional sign) starting at
// j, returning the index just past it. If no valid exponent follows, j is
// returned unchanged.
func (l *lexer) scanExponent(j int) int {
	k := j + 1
	if k < len(l.src) && (l.src[k] == '+' || l.src[k] == '-') {
		k++
	}
	if k < len(l.src) && isDigit(l.src[k]) {
		j = k
		for j < len(l.src) && isDigit(l.src[j]) {
			j++
		}
	}
	return j
}

// scanDecimalRun consumes a decimal mantissa (integer part, optional
// fraction, optional exponent) starting at j.
func (l *lexer) scanDecimalRun(j int) int {
	for j < len(l.src) && isDigit(l.src[j]) {
		j++
	}
	if j < len(l.src) && l.src[j] == '.' {
		j++
		for j < len(l.src) && isDigit(l.src[j]) {
			j++
		}
	}
	if j < len(l.src) && (l.src[j] == 'e' || l.src[j] == 'E') {
		j = l.scanExponent(j)
	}
	return j
}

func (l *lexer) scanNumber() {
	start := l.pos
	j := l.pos
	if l.src[j] == '0' && j+1 < len(l.src) && (l.src[j+1] == 'x' || l.src[j+1] == 'X') {
		j = l.scanHexRun(j)
	} else {
		j = l.scanDecimalRun(j)
	}
	numText := l.src[start:j]
	l.pos = j

	// Duration suffix (`5m`, `1h30m`).
	if d, consumed, ok := l.trySuffix(numText, durationRune, func(s string) (time.Duration, bool) {
		return parseDurationText(s)
	}); ok {
		l.pos = consumed
		l.toks = append(l.toks, token{kind: tkDuration, dur: d, pos: start})
		return
	}
	// Byte-size suffix (`1KB`, `1.5GiB`).
	if b, consumed, ok := l.tryBytesSuffix(numText); ok {
		l.pos = consumed
		l.toks = append(l.toks, token{kind: tkBytes, bytes: b, pos: start})
		return
	}
	l.toks = append(l.toks, token{kind: tkNumber, str: numText, pos: start})
}

// trySuffix attempts to read a unit suffix immediately following a
// number and parse number+suffix via parse. Returns the parsed value,
// the new position, and whether it succeeded (without committing l.pos).
func (l *lexer) trySuffix(num string, runeOK func(rune) bool, parse func(string) (time.Duration, bool)) (time.Duration, int, bool) {
	j := l.pos
	for j < len(l.src) {
		b := l.src[j]
		if b >= utf8.RuneSelf {
			rr, sz := utf8.DecodeRuneInString(l.src[j:])
			if runeOK(rr) {
				j += sz
				continue
			}
			break
		}
		if isDigit(b) || runeOK(rune(b)) || b == '.' {
			j++
			continue
		}
		break
	}
	if j == l.pos {
		return 0, 0, false
	}
	d, ok := parse(num + l.src[l.pos:j])
	if !ok {
		return 0, 0, false
	}
	return d, j, true
}

func (l *lexer) tryBytesSuffix(num string) (uint64, int, bool) {
	j := l.pos
	for j < len(l.src) {
		b := l.src[j]
		if isDigit(b) || bytesRune(rune(b)) || b == '.' {
			j++
			continue
		}
		break
	}
	if j == l.pos {
		return 0, 0, false
	}
	v, err := humanize.ParseBytes(num + l.src[l.pos:j])
	if err != nil {
		return 0, 0, false
	}
	return v, j, true
}

func (l *lexer) scanIdent() {
	start := l.pos
	j := l.pos
	for j < len(l.src) {
		r, sz := utf8.DecodeRuneInString(l.src[j:])
		if isIdentPart(r) {
			j += sz
			continue
		}
		break
	}
	id := l.src[start:j]
	l.pos = j
	lower := strings.ToLower(id)

	// Function-call identifiers are only operators when followed by `(`
	// (or `by/without (`); otherwise they are ordinary label names.
	if _, ok := rangeOps[lower]; ok {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkRangeOp, str: lower, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if _, ok := vectorOps[lower]; ok {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkVectorOp, str: lower, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if _, ok := convOps[lower]; ok {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkConvOp, str: lower, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if lower == OpTypeVector {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkVector, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if lower == OpLabelReplace {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkLabelReplace, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if lower == OpFilterIP {
		if l.isFunction() {
			l.toks = append(l.toks, token{kind: tkIP, pos: start})
			return
		}
		l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
		return
	}
	if k, ok := keywordKinds[lower]; ok {
		l.toks = append(l.toks, token{kind: k, str: lower, pos: start})
		return
	}
	l.toks = append(l.toks, token{kind: tkIdentifier, str: id, pos: start})
}

// isFunction reports whether the runes after the current position (skipping
// whitespace) begin a function-call: an open parenthesis directly, or a
// `by` / `without` grouping keyword followed by `(`.
func (l *lexer) isFunction() bool {
	i := l.pos
	skip := func() {
		for i < len(l.src) {
			c := l.src[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				i++
				continue
			}
			break
		}
	}
	skip()
	if i < len(l.src) && l.src[i] == '(' {
		return true
	}
	// `by` / `without`
	for _, kw := range []string{"by", "without"} {
		if strings.HasPrefix(strings.ToLower(l.src[i:]), kw) {
			end := i + len(kw)
			// Ensure it's a whole word.
			if end < len(l.src) {
				r, _ := utf8.DecodeRuneInString(l.src[end:])
				if isIdentPart(r) {
					continue
				}
			}
			j := end
			for j < len(l.src) {
				c := l.src[j]
				if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					j++
					continue
				}
				break
			}
			return j < len(l.src) && l.src[j] == '('
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isHexDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
func isIdentStart(b byte) bool { return isLetter(b) || b == '_' }
func isIdentPart(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		return true
	case r < utf8.RuneSelf:
		return false
	default:
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
}
