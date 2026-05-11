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
}

func (*LineContent) exprNode() {}

func (l *LineContent) Equal(other Expr) bool {
	o, ok := other.(*LineContent)
	if !ok {
		return false
	}
	return l.Pattern == o.Pattern && l.IsRegex == o.IsRegex &&
		l.Negated == o.Negated && l.Source.Equal(o.Source)
}
