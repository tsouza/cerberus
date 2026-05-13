package chsql

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Builder accumulates a parameterised ClickHouse SQL fragment plus the
// positional `?` argument slice that the chclient driver binds.
//
// Builder is the public, named version of the private emitter struct in
// emit.go. As of RC6 R6.1 it is pure scaffolding — no emit_*.go function
// uses it yet; R6.2–R6.10 port each emit function in turn. The
// architectural intent (per docs/sql-builder-evaluation.md) is to expose
// the same `strings.Builder` + `[]any` args primitives the emitter
// already uses, plus a handful of CH-specific helpers (MapAt, MapKeys,
// MapFilterExcept, Now64, SubtractNanos, DateTime64Lit, Lambda,
// ParamAgg) and a QueryBuilder with first-class PREWHERE, JOIN, and
// WITH RECURSIVE slots so the RC3 optimizer rules can compose SQL
// fragments without re-parsing rendered strings.
//
// The zero value is ready to use.
type Builder struct {
	sb   strings.Builder
	args []any
}

// NewBuilder returns an empty Builder. Equivalent to &Builder{}.
func NewBuilder() *Builder { return &Builder{} }

// String returns the accumulated SQL.
func (b *Builder) String() string { return b.sb.String() }

// Args returns the positional argument slice, in the order `?`
// placeholders were emitted. The slice is owned by the Builder; callers
// should not mutate it.
func (b *Builder) Args() []any { return b.args }

// Build is the conventional terminator: returns the rendered SQL and
// its positional argument slice.
func (b *Builder) Build() (string, []any) { return b.sb.String(), b.args }

// writeSQL appends raw SQL text. Unexported as of R6.11e — external
// packages must use the typed surface (QueryBuilder slots + Frag
// constructors like Eq / And / Paren / Cast). In-package callers
// (histogram_quantile.go, vector_join.go, structural_join.go,
// set_op.go) use this for operator-token-style glue inside Frag
// callbacks; clause keywords still go through QueryBuilder slots.
//
// (There is intentionally no writeByte method on Builder: io.ByteWriter
// expects WriteByte(byte) error, and offering a non-error variant
// confuses both govet and callers. Single-byte writes go through
// writeSQL with a one-character string.)
func (b *Builder) writeSQL(s string) { b.sb.WriteString(s) }

// Ident appends a ClickHouse identifier with backtick quoting, doubling
// any embedded backticks. Mirrors writeIdent in emit_node.go and
// quoteIdent in range_window.go; R6.2+ replaces both call sites with
// this method.
func (b *Builder) Ident(name string) {
	b.sb.WriteByte('`')
	b.sb.WriteString(strings.ReplaceAll(name, "`", "``"))
	b.sb.WriteByte('`')
}

// QualIdent appends "<qualifier>.<name>" with both parts backtick-quoted.
// Used by VectorJoin output where columns are qualified as L.<col> /
// R.<col>.
func (b *Builder) QualIdent(qualifier, name string) {
	b.Ident(qualifier)
	b.sb.WriteByte('.')
	b.Ident(name)
}

// Arg appends a `?` placeholder and records v in the args slice.
// Every dynamic value (literals, regex patterns, map keys) flows
// through Arg so the driver parameterises them rather than splicing
// them into the SQL.
func (b *Builder) Arg(v any) {
	b.sb.WriteByte('?')
	b.args = append(b.args, v)
}

// MapAt appends "<col>[?]" and binds key as a positional argument —
// CH's Map column access. col is a single bare column name; for nested
// or qualified references, write the prefix via writeSQL / QualIdent
// before the bracket form lands.
func (b *Builder) MapAt(col, key string) {
	b.Ident(col)
	b.sb.WriteByte('[')
	b.Arg(key)
	b.sb.WriteByte(']')
}

// MapKeys appends "mapKeys(<col>)" — CH's built-in for extracting the
// key set of a Map column. Used by the metadata SQL stack to derive the
// list of attribute names known for a metric.
func (b *Builder) MapKeys(col string) {
	b.sb.WriteString("mapKeys(")
	b.Ident(col)
	b.sb.WriteByte(')')
}

// MapFilterExcept appends
//
//	mapFilter((k, v) -> NOT (k IN (?, ?, ...)), <col>)
//
// binding each key as a positional `?` argument. The shape mirrors
// emit_expr.go's emitMapWithoutKeys (used by PromQL's ignoring(…)
// modifier) and vector_join.go's mapFilter for the same purpose;
// R6.4 / R6.6 collapse both call sites onto this helper.
//
// Empty keys is a programmer error and panics: the resulting CH SQL
// would always pass the filter, which is never the caller's intent
// (an empty `ignoring()` round-trips through the parser as no
// ignoring clause at all).
func (b *Builder) MapFilterExcept(col string, keys ...string) {
	if len(keys) == 0 {
		panic("chsql: MapFilterExcept requires at least one key")
	}
	b.sb.WriteString("mapFilter((k, v) -> NOT (k IN (")
	for i, k := range keys {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		b.Arg(k)
	}
	b.sb.WriteString(")), ")
	b.Ident(col)
	b.sb.WriteByte(')')
}

// Now64 appends "now64(9)" — ClickHouse's current-time-at-nanosecond
// precision builtin. The range-window stack falls back to this when
// the lowering hasn't populated an explicit End time (typically only
// in the M0–M1 transition fixtures).
func (b *Builder) Now64() { b.sb.WriteString("now64(9)") }

// SubtractNanos appends "(<lhs> - toIntervalNanosecond(<ns>))". lhs
// writes the left-hand expression at the right SQL position so callers
// can compose with any expression-emitting helper (DateTime64Lit,
// Now64, or another SubtractNanos).
//
// ns is rendered as a literal integer, not parameterised. Duration
// constants are part of the query *shape* — CH sort-key pruning needs
// them visible to the planner, and parameterising them would force
// CH to recompute the bound per request.
func (b *Builder) SubtractNanos(lhs func(b *Builder), ns int64) {
	b.sb.WriteByte('(')
	lhs(b)
	b.sb.WriteString(" - toIntervalNanosecond(")
	b.sb.WriteString(strconv.FormatInt(ns, 10))
	b.sb.WriteString("))")
}

// DateTime64Lit appends a CH DateTime64(9) literal in the form
//
//	toDateTime64('YYYY-MM-DD HH:MM:SS.NNNNNNNNN', 9)
//
// The format mirrors timeOrNow in range_window.go. The time is
// rendered in UTC; the 9-digit fractional second covers nanosecond
// precision exactly.
func (b *Builder) DateTime64Lit(t time.Time) {
	b.sb.WriteString("toDateTime64('")
	b.sb.WriteString(t.UTC().Format("2006-01-02 15:04:05.000000000"))
	b.sb.WriteString("', 9)")
}

// Lambda appends "(<p1>, <p2>, ...) -> " and runs body() to write the
// lambda body. CH lambdas are bare (no `function` keyword); used by
// mapFilter, arrayMap, arrayFilter, etc. Args bound inside body land
// at the position body emits them, so positional `?` ordering follows
// the SQL stream.
func (b *Builder) Lambda(params []string, body func(b *Builder)) {
	b.sb.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		b.sb.WriteString(p)
	}
	b.sb.WriteString(") -> ")
	body(b)
}

// ParamAgg appends "<name>(<param1>, ...)(<arg1>, ...)" — the CH
// parameterised-aggregate shape used by quantile / quantiles /
// topK / etc. If params is empty, the leading parens are omitted,
// matching the non-parameterised shape "<name>(<arg1>, ...)".
//
// params and args are each rendered via callback so callers can use
// any expression-emitting helper (Arg, Ident, ParamAgg-of-ParamAgg,
// …). Bound args land in the order the callbacks emit them.
func (b *Builder) ParamAgg(name string, params, args []func(b *Builder)) {
	b.sb.WriteString(name)
	if len(params) > 0 {
		b.sb.WriteByte('(')
		for i, p := range params {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			p(b)
		}
		b.sb.WriteByte(')')
	}
	b.sb.WriteByte('(')
	for i, a := range args {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		a(b)
	}
	b.sb.WriteByte(')')
}

// Expr renders a chplan.Expr through the Builder using the public
// Builder helpers (Ident / Arg / etc.). It mirrors the legacy
// emitter.emitExpr in emit_expr.go; RC6 R6.2 introduces this method so
// the ported emitFilter / emitProject can emit predicates and
// projection expressions without reaching into the private emitter.
//
// The legacy emitter.emitExpr is intentionally retained — it is the
// canonical implementation until RC6 R6.4 ports the expression tree.
// Both paths produce byte-identical SQL for every fixture; once the
// rest of the emitter migrates, emitExpr collapses into this method.
func (b *Builder) Expr(x chplan.Expr) error {
	switch v := x.(type) {
	case *chplan.ColumnRef:
		if v.Qualifier != "" {
			b.QualIdent(v.Qualifier, v.Name)
			return nil
		}
		b.Ident(v.Name)
		return nil
	case *chplan.LitString:
		b.Arg(v.V)
		return nil
	case *chplan.LitInt:
		b.Arg(v.V)
		return nil
	case *chplan.LitFloat:
		b.Arg(v.V)
		return nil
	case *chplan.LitBool:
		b.Arg(v.V)
		return nil
	case *chplan.Binary:
		return b.exprBinary(v)
	case *chplan.FuncCall:
		return b.exprFunc(v)
	case *chplan.MapAccess:
		return b.exprMapAccess(v)
	case *chplan.MapWithoutKeys:
		return b.exprMapWithoutKeys(v)
	case *chplan.LineContent:
		return b.exprLineContent(v)
	case *chplan.FieldAccess:
		return b.exprFieldAccess(v)
	case *chplan.NestedArrayExists:
		return b.exprNestedArrayExists(v)
	default:
		return fmt.Errorf("%w: expr %T", ErrUnsupported, x)
	}
}

// exprNestedArrayExists renders
//
//	arrayExists(x -> x[?] <op> ?, `<Column>`.`<SubField>`)
//
// against the public Builder helpers. Mirrors the legacy
// emitter.emitNestedArrayExists so both paths produce byte-identical SQL.
func (b *Builder) exprNestedArrayExists(n *chplan.NestedArrayExists) error {
	b.sb.WriteString("arrayExists(x -> x[")
	b.Arg(n.Key)
	b.sb.WriteString("] ")
	b.sb.WriteString(string(n.Op))
	b.sb.WriteByte(' ')
	if err := b.Expr(n.Value); err != nil {
		return err
	}
	b.sb.WriteString(", ")
	b.QualIdent(n.Column, n.SubField)
	b.sb.WriteByte(')')
	return nil
}

func (b *Builder) exprBinary(bx *chplan.Binary) error {
	switch bx.Op {
	case chplan.OpMatch, chplan.OpNotMatch:
		if bx.Op == chplan.OpNotMatch {
			b.sb.WriteString("NOT ")
		}
		b.sb.WriteString("match(")
		if err := b.Expr(bx.Left); err != nil {
			return err
		}
		b.sb.WriteString(", ")
		if err := b.Expr(bx.Right); err != nil {
			return err
		}
		b.sb.WriteByte(')')
		return nil
	case chplan.OpPow:
		b.sb.WriteString("pow(")
		if err := b.Expr(bx.Left); err != nil {
			return err
		}
		b.sb.WriteString(", ")
		if err := b.Expr(bx.Right); err != nil {
			return err
		}
		b.sb.WriteByte(')')
		return nil
	}
	b.sb.WriteByte('(')
	if err := b.Expr(bx.Left); err != nil {
		return err
	}
	b.sb.WriteByte(' ')
	b.sb.WriteString(string(bx.Op))
	b.sb.WriteByte(' ')
	if err := b.Expr(bx.Right); err != nil {
		return err
	}
	b.sb.WriteByte(')')
	return nil
}

func (b *Builder) exprFunc(f *chplan.FuncCall) error {
	b.sb.WriteString(f.Name)
	b.sb.WriteByte('(')
	for i, a := range f.Args {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		if err := b.Expr(a); err != nil {
			return err
		}
	}
	b.sb.WriteByte(')')
	return nil
}

func (b *Builder) exprMapAccess(m *chplan.MapAccess) error {
	if err := b.Expr(m.Map); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	if err := b.Expr(m.Key); err != nil {
		return err
	}
	b.sb.WriteByte(']')
	return nil
}

func (b *Builder) exprMapWithoutKeys(m *chplan.MapWithoutKeys) error {
	b.sb.WriteString("mapFilter((k, v) -> NOT (k IN (")
	for i, k := range m.Keys {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		b.Arg(k)
	}
	b.sb.WriteString(")), ")
	if err := b.Expr(m.Map); err != nil {
		return err
	}
	b.sb.WriteByte(')')
	return nil
}

func (b *Builder) exprLineContent(l *chplan.LineContent) error {
	if l.IsRegex {
		if l.Negated {
			b.sb.WriteString("NOT ")
		}
		b.sb.WriteString("match(")
		if err := b.Expr(l.Source); err != nil {
			return err
		}
		b.sb.WriteString(", ")
		b.Arg(l.Pattern)
		b.sb.WriteByte(')')
		return nil
	}
	op := " > 0"
	if l.Negated {
		op = " = 0"
	}
	b.sb.WriteString("(position(")
	if err := b.Expr(l.Source); err != nil {
		return err
	}
	b.sb.WriteString(", ")
	b.Arg(l.Pattern)
	b.sb.WriteByte(')')
	b.sb.WriteString(op)
	b.sb.WriteByte(')')
	return nil
}

func (b *Builder) exprFieldAccess(f *chplan.FieldAccess) error {
	if err := b.Expr(f.Source); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	b.Arg(f.Path)
	b.sb.WriteByte(']')
	return nil
}

// Frag is the unit of composition: anything that knows how to write
// itself into a Builder. QueryBuilder's slots hold Frag values
// rather than rendered strings so positional `?` arguments stay
// tied to the position they're written at — a fragment passed to
// Where renders into the WHERE clause with its args at the WHERE
// position in the args slice.
type Frag func(b *Builder)

// Col returns a Frag that emits a backtick-quoted column identifier.
// Equivalent to b.Ident(name) but usable as a QueryBuilder slot.
func Col(name string) Frag {
	return func(b *Builder) { b.Ident(name) }
}

// Qual returns a Frag that emits "<qualifier>.<name>" with both
// parts backtick-quoted.
func Qual(qualifier, name string) Frag {
	return func(b *Builder) { b.QualIdent(qualifier, name) }
}

// Lit returns a Frag that emits a `?` placeholder and binds v.
func Lit(v any) Frag {
	return func(b *Builder) { b.Arg(v) }
}

// Raw returns a Frag that emits sql verbatim — the escape hatch for
// shapes not (yet) covered by a typed helper. R6.9's lint rule will
// flag direct fmt.Sprintf-on-SQL but Raw is the sanctioned bypass
// for one-off CH idioms; reach for it sparingly.
func Raw(sql string) Frag {
	return func(b *Builder) { b.sb.WriteString(sql) }
}

// UnionAll joins one or more Frags with " UNION ALL " between them. It
// is the typed alternative to `strings.Join(parts, " UNION ALL ")` —
// keeping the keyword inside the typed surface so the audit grep for
// clause-keyword cosplay stays clean. Each part is rendered in order
// and its `?` args bind at the position they're emitted.
//
// UNION is a SELECT-level binary operator (mirrors the SetUnion path
// in set_op.go), not a clause inside a single SELECT, so it lives as
// a standalone Frag constructor rather than a QueryBuilder slot.
//
// Typical use: pass QueryBuilder.Frag() values as parts so each arm
// renders as a parenthesised (SELECT …) and the whole UnionAll Frag
// is plugged into the outer QueryBuilder.From slot.
//
// Zero parts is a programmer error and panics; one part is rendered
// unchanged (no UNION keyword emitted).
func UnionAll(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: UnionAll requires at least one part")
	}
	return func(b *Builder) {
		for i, p := range parts {
			if i > 0 {
				b.sb.WriteString(" UNION ALL ")
			}
			p(b)
		}
	}
}

// UnionDistinct renders `<p1> UNION DISTINCT <p2> UNION DISTINCT …`.
// CH's UNION DISTINCT dedupes on the full row tuple. Same composition
// shape as UnionAll; see its godoc.
func UnionDistinct(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: UnionDistinct requires at least one part")
	}
	return func(b *Builder) {
		for i, p := range parts {
			if i > 0 {
				b.sb.WriteString(" UNION DISTINCT ")
			}
			p(b)
		}
	}
}

// As wraps expr in "<expr> AS <alias>" with the alias backtick-quoted.
// The typed alternative to `b.writeSQL(" AS "); b.Ident(alias)`; using
// As keeps the AS keyword inside the typed surface so the audit grep
// for clause-keyword cosplay stays clean. If alias is empty the inner
// expression is emitted unchanged (no AS clause).
func As(expr Frag, alias string) Frag {
	if alias == "" {
		return expr
	}
	return func(b *Builder) {
		expr(b)
		b.sb.WriteString(" AS ")
		b.Ident(alias)
	}
}

// binOp returns a Frag that renders "<l> <op> <r>" with single spaces
// around op. Shared shape for the comparison + arithmetic operator
// constructors below — each typed wrapper just supplies its op token.
func binOp(op string, l, r Frag) Frag {
	return func(b *Builder) {
		l(b)
		b.sb.WriteByte(' ')
		b.sb.WriteString(op)
		b.sb.WriteByte(' ')
		r(b)
	}
}

// Eq returns a Frag rendering "<l> = <r>".
func Eq(l, r Frag) Frag { return binOp("=", l, r) }

// Neq returns a Frag rendering "<l> != <r>".
func Neq(l, r Frag) Frag { return binOp("!=", l, r) }

// Gt returns a Frag rendering "<l> > <r>".
func Gt(l, r Frag) Frag { return binOp(">", l, r) }

// Gte returns a Frag rendering "<l> >= <r>".
func Gte(l, r Frag) Frag { return binOp(">=", l, r) }

// Lt returns a Frag rendering "<l> < <r>".
func Lt(l, r Frag) Frag { return binOp("<", l, r) }

// Lte returns a Frag rendering "<l> <= <r>".
func Lte(l, r Frag) Frag { return binOp("<=", l, r) }

// Like returns a Frag rendering "<l> LIKE <r>".
func Like(l, r Frag) Frag { return binOp("LIKE", l, r) }

// NotLike returns a Frag rendering "<l> NOT LIKE <r>".
func NotLike(l, r Frag) Frag { return binOp("NOT LIKE", l, r) }

// And returns a Frag joining parts with " AND ". Panics if parts is empty.
func And(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: And requires at least one part")
	}
	return func(b *Builder) {
		for i, p := range parts {
			if i > 0 {
				b.sb.WriteString(" AND ")
			}
			p(b)
		}
	}
}

// Or returns a Frag joining parts with " OR ". Panics if parts is empty.
func Or(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: Or requires at least one part")
	}
	return func(b *Builder) {
		for i, p := range parts {
			if i > 0 {
				b.sb.WriteString(" OR ")
			}
			p(b)
		}
	}
}

// Not returns a Frag rendering "NOT <f>". No parens are added — the
// caller wraps with Paren if precedence requires it.
func Not(f Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteString("NOT ")
		f(b)
	}
}

// Add returns a Frag rendering "<l> + <r>".
func Add(l, r Frag) Frag { return binOp("+", l, r) }

// Sub returns a Frag rendering "<l> - <r>".
func Sub(l, r Frag) Frag { return binOp("-", l, r) }

// Mul returns a Frag rendering "<l> * <r>".
func Mul(l, r Frag) Frag { return binOp("*", l, r) }

// Div returns a Frag rendering "<l> / <r>".
func Div(l, r Frag) Frag { return binOp("/", l, r) }

// Mod returns a Frag rendering "<l> % <r>".
func Mod(l, r Frag) Frag { return binOp("%", l, r) }

// Neg returns a Frag rendering "-<f>" (no space between the minus and
// the operand).
func Neg(f Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteByte('-')
		f(b)
	}
}

// Paren returns a Frag rendering "(<f>)" with no inner whitespace.
func Paren(f Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteByte('(')
		f(b)
		b.sb.WriteByte(')')
	}
}

// Tuple returns a Frag rendering "(<p0>, <p1>, ...)". Panics if parts
// is empty (an empty tuple is a CH syntax error).
func Tuple(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: Tuple requires at least one part")
	}
	return func(b *Builder) {
		b.sb.WriteByte('(')
		for i, p := range parts {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			p(b)
		}
		b.sb.WriteByte(')')
	}
}

// Cast returns a Frag rendering "CAST(<f> AS <typ>)". typ is a CH type
// name (e.g. "Float64") and is emitted verbatim — same trust contract
// as Raw, the caller is responsible for ensuring it is a safe literal.
func Cast(f Frag, typ string) Frag {
	return func(b *Builder) {
		b.sb.WriteString("CAST(")
		f(b)
		b.sb.WriteString(" AS ")
		b.sb.WriteString(typ)
		b.sb.WriteByte(')')
	}
}

// Concat returns a Frag rendering parts back-to-back with no separator.
// For arbitrary glue when callers compose Frags whose internal
// whitespace they already control. Panics if parts is empty.
func Concat(parts ...Frag) Frag {
	if len(parts) == 0 {
		panic("chsql: Concat requires at least one part")
	}
	return func(b *Builder) {
		for _, p := range parts {
			p(b)
		}
	}
}

// In returns a Frag rendering "<left> IN (<r0>, <r1>, ...)". Panics if
// right is empty (an empty IN list is a CH syntax error).
func In(left Frag, right ...Frag) Frag {
	if len(right) == 0 {
		panic("chsql: In requires at least one right-hand part")
	}
	return func(b *Builder) {
		left(b)
		b.sb.WriteString(" IN (")
		for i, r := range right {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			r(b)
		}
		b.sb.WriteByte(')')
	}
}

// JoinKind identifies a SQL JOIN flavour. The constants render as
// their literal SQL keywords (e.g. "INNER JOIN") and flow through
// QueryBuilder.Join's typed slot so callers never compose the join
// keyword by hand.
type JoinKind string

const (
	// InnerJoin renders as "INNER JOIN" — rows from both sides that
	// satisfy the ON predicate.
	InnerJoin JoinKind = "INNER JOIN"
	// LeftJoin renders as "LEFT JOIN".
	LeftJoin JoinKind = "LEFT JOIN"
	// RightJoin renders as "RIGHT JOIN".
	RightJoin JoinKind = "RIGHT JOIN"
	// CrossJoin renders as "CROSS JOIN"; the ON Frag is ignored.
	CrossJoin JoinKind = "CROSS JOIN"
	// FullJoin renders as "FULL JOIN".
	FullJoin JoinKind = "FULL JOIN"
)

// joinClause is one entry in a QueryBuilder's join chain. Rendered
// as ` <kind> <src> ON <on>` (single leading space) — or, for
// CrossJoin, ` CROSS JOIN <src>` with the ON Frag suppressed.
type joinClause struct {
	Kind JoinKind
	Src  Frag
	On   Frag
}

// cteClause is one entry in a QueryBuilder's WITH chain. The
// recursive flag flips on the WITH RECURSIVE shape:
//
//	WITH RECURSIVE <name> AS (<anchor> UNION ALL <recursive>)
//
// Non-recursive CTEs render the anchor alone (no UNION ALL). Only
// recursive CTEs are wired up at R6.6 — the non-recursive case is
// reserved for a future R6.x port.
type cteClause struct {
	Name      string
	Anchor    *QueryBuilder
	Recursive *QueryBuilder
}

// QueryBuilder accumulates a SELECT statement's parts. Slots are
// appended to in order; rendering walks each slot, emitting the
// canonical clause prefix (SELECT, FROM, WHERE, …) and joining
// per-slot Frags with the right separator.
//
// PREWHERE is a first-class slot, distinct from WHERE. ClickHouse
// evaluates PREWHERE before WHERE on the primary-key columns,
// pruning rows before the full row read; RC3's optimizer rules
// promote predicates from WHERE → PREWHERE when the predicate
// only references sort-key columns. Modelling PREWHERE separately
// here means those rewrites are slot-level operations rather than
// string rewrites on rendered SQL.
//
// JOIN clauses live in the joins slot, rendered in order between
// FROM and PREWHERE. Each entry holds a JoinKind, a source Frag (the
// right-hand table / subquery, typically already aliased via the
// caller's Frag), and an ON predicate Frag. The shape is the same
// flavour as a typed Where clause — the JOIN keyword + ON keyword
// stay inside writeInto.
//
// CTEs live in the ctes slot. Currently only WITH RECURSIVE form is
// emitted (vector_join.go has no CTE; structural_join.go's >> / <<
// emitter uses the recursive shape). Each entry renders as
// `WITH RECURSIVE <name> AS (<anchor> UNION ALL <recursive>)` ahead
// of the SELECT keyword.
//
// The zero value is ready to use; NewQuery is provided for clarity.
type QueryBuilder struct {
	ctes       []cteClause
	selectList []Frag
	from       Frag
	joins      []joinClause
	where      []Frag
	prewhere   []Frag
	groupBy    []Frag
	orderBy    []orderKey
	limit      int64
	hasLimit   bool
}

type orderKey struct {
	Expr Frag
	Desc bool
}

// NewQuery returns an empty QueryBuilder.
func NewQuery() *QueryBuilder { return &QueryBuilder{} }

// Select appends one or more expressions to the SELECT list. If the
// list is left empty at Build time the rendered SQL emits `SELECT *`.
func (s *QueryBuilder) Select(exprs ...Frag) *QueryBuilder {
	s.selectList = append(s.selectList, exprs...)
	return s
}

// SelectAs appends "<expr> AS <alias>" to the SELECT list. If alias is
// empty the expression is appended bare (equivalent to Select(expr)).
// Convenience wrapper over As + Select; lets projection callers express
// "this expression renames to this column" without composing the AS
// keyword by hand.
func (s *QueryBuilder) SelectAs(expr Frag, alias string) *QueryBuilder {
	s.selectList = append(s.selectList, As(expr, alias))
	return s
}

// From sets the FROM source. Accepts any Frag — Col(table), Raw for
// subquery escape hatches, or another QueryBuilder via its Frag()
// method (which wraps the nested SELECT in parens).
func (s *QueryBuilder) From(src Frag) *QueryBuilder {
	s.from = src
	return s
}

// Join appends a JOIN clause. kind selects the JOIN flavour (the
// keyword stays inside writeInto), src is the right-hand source —
// typically a subquery Frag already wrapped in parens + an unquoted
// alias suffix (vector_join / structural_join use bare `L` / `R`
// aliases) — and on is the ON predicate Frag. on may be nil for
// CrossJoin (the only kind that omits ON); a nil on with any other
// kind panics at render time.
//
// Multiple Join calls chain in order, rendered after FROM and before
// PREWHERE / WHERE.
func (s *QueryBuilder) Join(kind JoinKind, src, on Frag) *QueryBuilder {
	s.joins = append(s.joins, joinClause{Kind: kind, Src: src, On: on})
	return s
}

// WithRecursive registers a `WITH RECURSIVE <name> AS (<anchor>
// UNION ALL <recursive>)` CTE in front of the SELECT. The anchor and
// recursive children are QueryBuilders so their args land in
// emission order: anchor first, recursive second, then the outer
// SELECT.
//
// Multiple WithRecursive calls chain — rendered as a single
// `WITH RECURSIVE <n1> AS (...), <n2> AS (...)` head per CH syntax.
// At R6.6 only structural_join.go uses one CTE per emit; the
// multi-CTE shape is reserved for future ports.
//
// Passing a nil anchor or recursive panics at render time.
func (s *QueryBuilder) WithRecursive(name string, anchor, recursive *QueryBuilder) *QueryBuilder {
	s.ctes = append(s.ctes, cteClause{Name: name, Anchor: anchor, Recursive: recursive})
	return s
}

// Where appends predicates to the WHERE clause. Multiple predicates
// are joined with " AND " when rendered.
func (s *QueryBuilder) Where(conds ...Frag) *QueryBuilder {
	s.where = append(s.where, conds...)
	return s
}

// Prewhere appends predicates to the PREWHERE clause. Multiple
// predicates are joined with " AND " when rendered. PREWHERE is
// emitted before WHERE in the SQL.
func (s *QueryBuilder) Prewhere(conds ...Frag) *QueryBuilder {
	s.prewhere = append(s.prewhere, conds...)
	return s
}

// GroupBy appends grouping expressions.
func (s *QueryBuilder) GroupBy(keys ...Frag) *QueryBuilder {
	s.groupBy = append(s.groupBy, keys...)
	return s
}

// OrderBy appends a sort key. desc selects DESC; default is ASC
// (implicit, ClickHouse default).
func (s *QueryBuilder) OrderBy(expr Frag, desc bool) *QueryBuilder {
	s.orderBy = append(s.orderBy, orderKey{Expr: expr, Desc: desc})
	return s
}

// Limit sets the LIMIT count. n <= 0 emits no LIMIT clause; positive
// n is rendered as a literal integer (CH's LIMIT does not accept
// `?` placeholders in all driver paths and the value is part of the
// query shape, not user data). int64 accommodates chplan.Limit.Count
// without a lossy downcast.
func (s *QueryBuilder) Limit(n int64) *QueryBuilder {
	s.limit = n
	s.hasLimit = n > 0
	return s
}

// Frag returns a Frag that emits the rendered SELECT wrapped in
// parentheses. Used to plug a QueryBuilder into another's From
// without flattening to a string: args bound inside the nested
// SELECT stay tied to their position in the outer args slice.
func (s *QueryBuilder) Frag() Frag {
	return func(b *Builder) {
		b.sb.WriteByte('(')
		s.writeInto(b)
		b.sb.WriteByte(')')
	}
}

// Build renders the SELECT statement to (sql, args). Equivalent to
// running Frag() into a fresh Builder, minus the surrounding parens.
func (s *QueryBuilder) Build() (string, []any) {
	b := NewBuilder()
	s.writeInto(b)
	return b.Build()
}

func (s *QueryBuilder) writeInto(b *Builder) {
	if len(s.ctes) > 0 {
		b.sb.WriteString("WITH RECURSIVE ")
		for i, c := range s.ctes {
			if c.Anchor == nil || c.Recursive == nil {
				panic("chsql: WithRecursive requires non-nil anchor and recursive")
			}
			if i > 0 {
				b.sb.WriteString(", ")
			}
			// CTE names render bare — CH accepts unquoted identifiers
			// for CTE aliases, and the existing structural_join fixture
			// pins `_struct_closure` (no backticks). The caller is
			// responsible for passing a CH-identifier-safe token.
			b.sb.WriteString(c.Name)
			b.sb.WriteString(" AS (")
			c.Anchor.writeInto(b)
			b.sb.WriteString(" UNION ALL ")
			c.Recursive.writeInto(b)
			b.sb.WriteByte(')')
		}
		b.sb.WriteByte(' ')
	}
	b.sb.WriteString("SELECT ")
	if len(s.selectList) == 0 {
		b.sb.WriteByte('*')
	} else {
		for i, f := range s.selectList {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			f(b)
		}
	}
	if s.from != nil {
		b.sb.WriteString(" FROM ")
		s.from(b)
	}
	for _, j := range s.joins {
		b.sb.WriteByte(' ')
		b.sb.WriteString(string(j.Kind))
		b.sb.WriteByte(' ')
		j.Src(b)
		if j.Kind != CrossJoin {
			if j.On == nil {
				panic("chsql: Join requires a non-nil ON Frag (except for CrossJoin)")
			}
			b.sb.WriteString(" ON ")
			j.On(b)
		}
	}
	if len(s.prewhere) > 0 {
		b.sb.WriteString(" PREWHERE ")
		for i, f := range s.prewhere {
			if i > 0 {
				b.sb.WriteString(" AND ")
			}
			f(b)
		}
	}
	if len(s.where) > 0 {
		b.sb.WriteString(" WHERE ")
		for i, f := range s.where {
			if i > 0 {
				b.sb.WriteString(" AND ")
			}
			f(b)
		}
	}
	if len(s.groupBy) > 0 {
		b.sb.WriteString(" GROUP BY ")
		for i, f := range s.groupBy {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			f(b)
		}
	}
	if len(s.orderBy) > 0 {
		b.sb.WriteString(" ORDER BY ")
		for i, k := range s.orderBy {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			k.Expr(b)
			if k.Desc {
				b.sb.WriteString(" DESC")
			}
		}
	}
	if s.hasLimit {
		b.sb.WriteString(" LIMIT ")
		b.sb.WriteString(strconv.FormatInt(s.limit, 10))
	}
}
