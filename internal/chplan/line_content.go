package chplan

// LineContent matches a substring or regex against a log body column.
// LogQL `|=` / `!=` / `|~` / `!~` operators lower to this expression.
//
//   - IsRegex=false, Negated=false → `position(<Source>, <Pattern>) > 0`
//   - IsRegex=false, Negated=true  → `position(<Source>, <Pattern>) = 0`
//   - IsRegex=true,  Negated=false → `match(<Source>, <Pattern>)`
//   - IsRegex=true,  Negated=true  → `NOT match(<Source>, <Pattern>)`
//
// The emitter renders it as the CH expression that yields a UInt8
// suitable for a WHERE predicate.
type LineContent struct {
	Source  Expr
	Pattern string
	IsRegex bool
	Negated bool

	// TextIndexPrefilter opts the emitter (internal/chsql's exprLineContent)
	// onto an ANDed per-token `lower(<Source>) LIKE '%tok%'` strict-superset
	// prefilter ahead of the UNCHANGED row predicate above — chopt
	// text_index_line_filter's resolved verdict (cerberus issue #2773),
	// threaded down from lowerCtx.TextIndexLineFilter.
	//
	// The lowerer sets this ONLY when Negated is false: a superset prefilter
	// has no sound dual for a "must NOT contain" predicate (lower(Body) LIKE
	// '%tok%' being false only proves the LITERAL is absent when combined
	// with every other token — negating the prefilter itself would reject
	// rows the row predicate would have kept). A true value is necessary but
	// not sufficient for the emitter to actually rewrite: IsRegex=true
	// additionally needs the Pattern to round-trip through regexp/syntax as
	// a single OpLiteral (a regex only in name — no metacharacters) before
	// the emitter treats it like a plain substring literal; any other regex
	// shape, and a literal pattern with no token long enough to be worth
	// prefiltering, both render byte-identical to TextIndexPrefilter=false.
	TextIndexPrefilter bool
}

func (*LineContent) exprNode() {}

func (l *LineContent) Equal(other Expr) bool {
	o, ok := other.(*LineContent)
	if !ok {
		return false
	}
	return l.Pattern == o.Pattern && l.IsRegex == o.IsRegex &&
		l.Negated == o.Negated && l.TextIndexPrefilter == o.TextIndexPrefilter &&
		l.Source.Equal(o.Source)
}
