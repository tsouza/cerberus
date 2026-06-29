package ast

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/scanner"
	"time"
	"unicode"

	"github.com/prometheus/common/model"
)

// tokenKind enumerates the lexical token classes produced by the
// hand-written TraceQL tokenizer. The set mirrors the surface grammar's
// terminal alphabet (one kind per keyword / operator / literal class); the
// concrete integer values are private to this package.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokError

	// punctuation / grouping
	tokComma
	tokOpenBrace
	tokCloseBrace
	tokOpenParen
	tokCloseParen
	tokPipe

	// comparison / arithmetic / logical operators
	tokEq
	tokNeq
	tokRe
	tokNre
	tokGt
	tokGte
	tokLt
	tokLte
	tokAdd
	tokSub
	tokDiv
	tokMod
	tokMul
	tokPow
	tokAnd
	tokOr
	tokNot

	// structural span operators
	tokDesc       // >>
	tokAnce       // <<
	tokSibl       // ~
	tokNotChild   // !>
	tokNotParent  // !<
	tokNotDesc    // !>>
	tokNotAnce    // !<<
	tokUnionSibl  // &~
	tokUnionChild // &>
	tokUnionPar   // &<
	tokUnionDesc  // &>>
	tokUnionAnce  // &<<

	// literal value tokens
	tokString
	tokInteger
	tokFloat
	tokDuration
	tokTrue
	tokFalse
	tokNil

	// status / kind keyword literals
	tokStatusOk
	tokStatusError
	tokStatusUnset
	tokKindUnspecified
	tokKindInternal
	tokKindServer
	tokKindClient
	tokKindProducer
	tokKindConsumer

	// intrinsic keywords
	tokIDuration
	tokChildCount
	tokName
	tokStatus
	tokStatusMessage
	tokKind
	tokRootName
	tokRootServiceName
	tokRootService
	tokTraceDuration
	tokNestedSetLeft
	tokNestedSetRight
	tokNestedSetParent
	tokID
	tokTraceID
	tokSpanID
	tokParentID
	tokTimeSinceStart
	tokVersion
	tokParent

	// scope-prefix tokens
	tokParentDot
	tokResourceDot
	tokSpanDot
	tokTraceColon
	tokSpanColon
	tokEventColon
	tokLinkColon
	tokInstrColon
	tokEventDot
	tokLinkDot
	tokInstrDot
	tokDot

	// attribute identifier + its terminator
	tokIdentifier
	tokEndAttribute

	// aggregate / pipeline keywords
	tokCount
	tokAvg
	tokMax
	tokMin
	tokSum
	tokBy
	tokCoalesce
	tokSelect

	// metrics keywords
	tokRate
	tokCountOverTime
	tokMinOverTime
	tokMaxOverTime
	tokAvgOverTime
	tokSumOverTime
	tokQuantileOverTime
	tokHistogramOverTime
	tokCompare
	tokTopK
	tokBottomK
	tokWith
)

// keywordTokens maps the fixed surface spellings to their token kinds. It is
// the single source of truth for both the tokenizer (forward lookup) and the
// error renderer (reverse lookup). The spellings are dictated by the TraceQL
// language and carry no implementation expression of their own.
var keywordTokens = map[string]tokenKind{
	",":   tokComma,
	".":   tokDot,
	"{":   tokOpenBrace,
	"}":   tokCloseBrace,
	"(":   tokOpenParen,
	")":   tokCloseParen,
	"=":   tokEq,
	"!=":  tokNeq,
	"=~":  tokRe,
	"!~":  tokNre,
	">":   tokGt,
	">=":  tokGte,
	"<":   tokLt,
	"<=":  tokLte,
	"+":   tokAdd,
	"-":   tokSub,
	"/":   tokDiv,
	"%":   tokMod,
	"*":   tokMul,
	"^":   tokPow,
	"&&":  tokAnd,
	"||":  tokOr,
	"!":   tokNot,
	"|":   tokPipe,
	">>":  tokDesc,
	"<<":  tokAnce,
	"~":   tokSibl,
	"!>":  tokNotChild,
	"!<":  tokNotParent,
	"!>>": tokNotDesc,
	"!<<": tokNotAnce,
	"&~":  tokUnionSibl,
	"&>":  tokUnionChild,
	"&<":  tokUnionPar,
	"&>>": tokUnionDesc,
	"&<<": tokUnionAnce,

	"true":  tokTrue,
	"false": tokFalse,
	"nil":   tokNil,

	"ok":          tokStatusOk,
	"error":       tokStatusError,
	"unset":       tokStatusUnset,
	"unspecified": tokKindUnspecified,
	"internal":    tokKindInternal,
	"server":      tokKindServer,
	"client":      tokKindClient,
	"producer":    tokKindProducer,
	"consumer":    tokKindConsumer,

	"duration":        tokIDuration,
	"childCount":      tokChildCount,
	"name":            tokName,
	"status":          tokStatus,
	"statusMessage":   tokStatusMessage,
	"kind":            tokKind,
	"rootName":        tokRootName,
	"rootServiceName": tokRootServiceName,
	"rootService":     tokRootService,
	"traceDuration":   tokTraceDuration,
	"nestedSetLeft":   tokNestedSetLeft,
	"nestedSetRight":  tokNestedSetRight,
	"nestedSetParent": tokNestedSetParent,
	"id":              tokID,
	"traceID":         tokTraceID,
	"spanID":          tokSpanID,
	"parentID":        tokParentID,
	"timeSinceStart":  tokTimeSinceStart,
	"version":         tokVersion,
	"parent":          tokParent,

	"parent.":          tokParentDot,
	"resource.":        tokResourceDot,
	"span.":            tokSpanDot,
	"trace:":           tokTraceColon,
	"span:":            tokSpanColon,
	"event:":           tokEventColon,
	"link:":            tokLinkColon,
	"instrumentation:": tokInstrColon,
	"event.":           tokEventDot,
	"link.":            tokLinkDot,
	"instrumentation.": tokInstrDot,

	"count":               tokCount,
	"avg":                 tokAvg,
	"max":                 tokMax,
	"min":                 tokMin,
	"sum":                 tokSum,
	"by":                  tokBy,
	"coalesce":            tokCoalesce,
	"select":              tokSelect,
	"rate":                tokRate,
	"count_over_time":     tokCountOverTime,
	"min_over_time":       tokMinOverTime,
	"max_over_time":       tokMaxOverTime,
	"avg_over_time":       tokAvgOverTime,
	"sum_over_time":       tokSumOverTime,
	"quantile_over_time":  tokQuantileOverTime,
	"histogram_over_time": tokHistogramOverTime,
	"compare":             tokCompare,
	"topk":                tokTopK,
	"bottomk":             tokBottomK,
	"with":                tokWith,
}

// tokenSpelling is the reverse of keywordTokens, used to render expected-token
// names in parse errors ("expected }" rather than "expected token 4").
var tokenSpelling = func() map[tokenKind]string {
	m := make(map[tokenKind]string, len(keywordTokens))
	for s, k := range keywordTokens {
		// keep the first spelling we see; aliases (e.g. operators) are unique
		if _, ok := m[k]; !ok {
			m[k] = s
		}
	}
	m[tokEOF] = "$end"
	m[tokString] = "string"
	m[tokInteger] = "integer"
	m[tokFloat] = "float"
	m[tokDuration] = "duration"
	m[tokIdentifier] = "identifier"
	m[tokEndAttribute] = "end of attribute"
	return m
}()

func (k tokenKind) String() string {
	if s, ok := tokenSpelling[k]; ok {
		return s
	}
	return fmt.Sprintf("token(%d)", int(k))
}

// token is one lexical unit. The value carriers are mutually exclusive: only
// the one relevant to kind is populated.
type token struct {
	kind tokenKind
	str  string        // string literal, identifier, or raw text
	i    int           // integer literal
	f    float64       // float literal
	d    time.Duration // duration literal
	line int
	col  int
}

const escapeRunes = `\"`

// longestScopePrefix is the length of "resource.", the longest scope prefix
// that tryScopeAttribute must be willing to look ahead across.
const longestScopePrefix = 9

// lexer wraps a text/scanner with the stateful attribute-parsing mode TraceQL
// needs: once a scope-introducing token (`.`, `span.`, `parent.`, …) is seen,
// the following run of attribute runes is consumed verbatim as a single
// identifier rather than re-tokenised.
type lexer struct {
	scanner scanner.Scanner
	errs    []error

	parsingAttribute bool
	currentScope     tokenKind
}

func newLexer(input string) *lexer {
	l := &lexer{}
	l.scanner.Init(strings.NewReader(input))
	l.scanner.Error = func(_ *scanner.Scanner, msg string) {
		l.errs = append(l.errs, errors.New(msg))
	}
	return l
}

// tokenize drains the input into a flat token slice terminated by a single
// tokEOF. A non-nil error is returned on the first lexical failure.
func tokenize(input string) ([]token, error) {
	l := newLexer(input)
	var toks []token
	for {
		t := l.next()
		if t.kind == tokError {
			if len(l.errs) > 0 {
				return nil, l.errs[0]
			}
			return nil, errors.New("lexer error")
		}
		toks = append(toks, t)
		if t.kind == tokEOF {
			break
		}
	}
	if len(l.errs) > 0 {
		return nil, l.errs[0]
	}
	return toks, nil
}

func (l *lexer) pos() (int, int) {
	return l.scanner.Line, l.scanner.Column
}

func (l *lexer) errf(format string, args ...any) token {
	line, col := l.pos()
	l.errs = append(l.errs, fmt.Errorf(format, args...))
	return token{kind: tokError, line: line, col: col}
}

// next returns the next token, advancing the underlying scanner. It mirrors
// the TraceQL lexing rules: attribute-mode capture, multi-rune operator
// disambiguation, and duration-aware number scanning.
func (l *lexer) next() token {
	line, col := l.pos()

	// Attribute mode ends as soon as the next rune can't belong to an
	// attribute name; emit the synthetic terminator the grammar expects.
	if l.parsingAttribute && !isAttributeRune(l.scanner.Peek()) {
		l.parsingAttribute = false
		return token{kind: tokEndAttribute, line: line, col: col}
	}

	if l.parsingAttribute {
		if scopeTok, ok := l.tryScopeAttribute(); ok {
			l.currentScope = scopeTok
			return token{kind: scopeTok, line: line, col: col}
		}
		name, err := l.parseAttribute()
		if err != nil {
			return l.errf("%s", err.Error())
		}
		return token{kind: tokIdentifier, str: name, line: line, col: col}
	}

	r := l.scanner.Scan()
	line, col = l.pos()
	switch r {
	case scanner.EOF:
		return token{kind: tokEOF, line: line, col: col}

	case scanner.String, scanner.RawString:
		unquoted, err := strconv.Unquote(l.scanner.TokenText())
		if err != nil {
			return l.errf("%s", err.Error())
		}
		return token{kind: tokString, str: unquoted, line: line, col: col}

	case scanner.Int:
		text := l.scanner.TokenText()
		if d, ok := l.tryScanDuration(text); ok {
			return token{kind: tokDuration, d: d, line: line, col: col}
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			return l.errf("%s", err.Error())
		}
		return token{kind: tokInteger, i: n, line: line, col: col}

	case scanner.Float:
		text := l.scanner.TokenText()
		if d, ok := l.tryScanDuration(text); ok {
			return token{kind: tokDuration, d: d, line: line, col: col}
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return l.errf("%s", err.Error())
		}
		return token{kind: tokFloat, f: f, line: line, col: col}
	}

	// Greedily extend the current text with following runes as long as the
	// accumulated string is a known multi-rune token; this disambiguates
	// prefixes such as `&` < `&>` < `&>>` and `span` < `span.`.
	multi := tokenKind(0)
	haveMulti := false
	text := l.scanner.TokenText()
	for {
		text += string(l.scanner.Peek())
		k, ok := keywordTokens[text]
		if !ok {
			break
		}
		multi, haveMulti = k, true
		l.scanner.Next()
	}

	if haveMulti {
		if isScopeToken(multi) {
			l.currentScope = multi
		}
		l.parsingAttribute = startsAttribute(multi)
		return token{kind: multi, line: line, col: col}
	}

	if k, ok := keywordTokens[l.scanner.TokenText()]; ok {
		l.parsingAttribute = startsAttribute(k)
		return token{kind: k, line: line, col: col}
	}

	// Anything else is a bare identifier (resolved later against the
	// intrinsic table, or rejected as unknown).
	return token{kind: tokIdentifier, str: l.scanner.TokenText(), line: line, col: col}
}

// parseAttribute consumes a run of attribute runes — including quoted
// segments — into a single name string.
func (l *lexer) parseAttribute() (string, error) {
	var sb strings.Builder
	for {
		r := l.scanner.Peek()
		switch {
		case r == '"':
			s, err := l.parseQuotedAttribute()
			if err != nil {
				return "", err
			}
			sb.WriteString(s)
		case isAttributeRune(r):
			sb.WriteRune(l.scanner.Next())
		default:
			return sb.String(), nil
		}
	}
}

func (l *lexer) parseQuotedAttribute() (string, error) {
	var sb strings.Builder
	l.scanner.Next() // opening quote
	for {
		r := l.scanner.Peek()
		if r == scanner.EOF {
			return "", errors.New(`unexpected EOF, expecting "`)
		}
		if r == '"' {
			l.scanner.Next()
			return sb.String(), nil
		}
		if r == '\\' {
			l.scanner.Next()
			if strings.ContainsRune(escapeRunes, l.scanner.Peek()) {
				sb.WriteRune(l.scanner.Next())
				continue
			}
			return "", errors.New("invalid escape sequence")
		}
		sb.WriteRune(l.scanner.Next())
	}
}

// tryScopeAttribute peeks (without consuming on failure) for an inner scope
// prefix — `span.` or `resource.` — that may follow `parent.`. It returns the
// scope token and consumes it only when the lookahead succeeds.
func (l *lexer) tryScopeAttribute() (tokenKind, bool) {
	probe := l.scanner // value copy; advancing this does not move the real scanner
	var sb strings.Builder
	for probe.Peek() != scanner.EOF {
		r := probe.Peek()
		if r == '.' {
			sb.WriteRune(probe.Next())
			break
		}
		if !isAttributeRune(r) {
			break
		}
		if sb.Len() > longestScopePrefix {
			break
		}
		sb.WriteRune(probe.Next())
	}

	k := keywordTokens[sb.String()]
	if (k == tokSpanDot || k == tokResourceDot) && l.currentScope == tokParentDot {
		for i := 0; i < sb.Len(); i++ {
			l.scanner.Next()
		}
		return k, true
	}
	return 0, false
}

// tryScanDuration attempts to extend an already-scanned numeric literal into a
// duration (e.g. `100` + `ms`). It consumes the suffix only on success.
func (l *lexer) tryScanDuration(number string) (time.Duration, bool) {
	var sb strings.Builder
	sb.WriteString(number)

	probe := l.scanner // value copy
	consumed := 0
	for {
		r := probe.Peek()
		if r == scanner.EOF || unicode.IsSpace(r) {
			break
		}
		if !unicode.IsNumber(r) && !isDurationRune(r) && r != '.' {
			break
		}
		sb.WriteRune(probe.Next())
		consumed++
	}
	if consumed == 0 {
		return 0, false
	}

	d, err := parseDuration(sb.String())
	if err != nil {
		return 0, false
	}
	for i := 0; i < consumed; i++ {
		l.scanner.Next()
	}
	return d, true
}

// parseDuration accepts Prometheus-style durations first (to match the PromQL
// head's unit set) and falls back to Go's time.ParseDuration.
func parseDuration(s string) (time.Duration, error) {
	if pd, err := model.ParseDuration(s); err == nil {
		return time.Duration(pd), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return d, nil
}

func isDurationRune(r rune) bool {
	switch r {
	case 'n', 's', 'u', 'm', 'h', 'µ', 'd', 'w', 'y':
		return true
	default:
		return false
	}
}

// isAttributeRune reports whether r may appear unquoted inside an attribute
// name. The excluded set is exactly the structural punctuation the grammar
// uses to delimit expressions.
func isAttributeRune(r rune) bool {
	if unicode.IsSpace(r) {
		return false
	}
	switch r {
	case scanner.EOF, '{', '}', '(', ')', '=', '~', '!', '<', '>', '&', '|', '^', ',':
		return false
	default:
		return true
	}
}

// startsAttribute reports whether the token, once emitted, switches the lexer
// into attribute-capture mode.
func startsAttribute(k tokenKind) bool {
	switch k {
	case tokDot, tokResourceDot, tokSpanDot, tokParentDot,
		tokEventDot, tokLinkDot, tokInstrDot:
		return true
	default:
		return false
	}
}

// isScopeToken reports whether the token records the current attribute scope
// (so a following `parent.span.` style chain resolves correctly).
func isScopeToken(k tokenKind) bool {
	switch k {
	case tokParentDot, tokSpanDot, tokResourceDot,
		tokSpanColon, tokTraceColon, tokEventColon, tokLinkColon, tokInstrColon,
		tokEventDot, tokLinkDot, tokInstrDot:
		return true
	default:
		return false
	}
}
