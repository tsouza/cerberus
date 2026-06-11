package chsql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Builder accumulates a parameterised ClickHouse SQL fragment plus the
// positional `?` argument slice that the chclient driver binds.
//
// Builder is the public, named version of the private emitter struct in
// emit.go. It exposes the same `strings.Builder` + `[]any` args
// primitives the emitter uses, plus a handful of CH-specific helpers
// (MapAt, MapKeys, MapFilterExcept, Now64, SubtractNanos,
// DateTime64Lit, Lambda, ParamAgg) and a QueryBuilder with first-class
// PREWHERE, JOIN, and WITH RECURSIVE slots so the optimizer rules can
// compose SQL fragments without re-parsing rendered strings.
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

// writeSQL appends raw SQL text. Unexported — external packages must
// use the typed surface (QueryBuilder slots + Frag constructors like
// Eq / And / Paren / Cast). In-package callers
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
// quoteIdent in range_window.go.
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

// writeInlineNonFinite emits ±Inf / NaN inline as a CH-portable
// arithmetic literal and returns true; finite floats return false and
// nothing is written. PromQL's `quantile()` helper returns ±Inf for phi
// outside [0, 1] (see prometheus/promql/quantile.go) and the lowerer
// post-Projects the Value column with such a literal — but clickhouse-go
// and chdb-go both render Go's `math.Inf(±1)` / `math.NaN()` as the
// mixed-case strings `+Inf` / `-Inf` / `NaN` when binding `?`, and real
// CH 24.x parses only the lowercase forms (`inf` / `-inf` / `nan`),
// surfacing on the wire as `Unknown identifier 'Inf'` → 502. The
// division forms `1.0/0` / `-1.0/0` / `0.0/0` fold to the same IEEE
// special values on the CH side and don't depend on the lexer's
// case-sensitivity for the identifier path.
func writeInlineNonFinite(b *Builder, v float64) bool {
	switch {
	case math.IsNaN(v):
		b.sb.WriteString("(0.0/0)")
		return true
	case math.IsInf(v, +1):
		b.sb.WriteString("(1.0/0)")
		return true
	case math.IsInf(v, -1):
		b.sb.WriteString("(-1.0/0)")
		return true
	}
	return false
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
// modifier) and vector_join.go's mapFilter for the same purpose.
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
// Builder helpers (Ident / Arg / etc.). It is used by the ported
// emitFilter / emitProject to emit predicates and projection expressions
// without reaching into the private emitter.
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
		// LitFloat values are passed to b.Arg as `?` placeholders,
		// and callers that project the bound value back as a
		// `Float64` column wrap the placeholder in `toFloat64(?)`
		// at the emission site (see internal/chsql/absent_over_time.go,
		// internal/chsql/exemplars.go, internal/chsql/vector_join.go,
		// and the `group(...)` / `count(...)` / `absent_over_time(...)`
		// PromQL lowerings — #634, #644, #646). The wrap exists
		// because clickhouse-go/v2's bind.go::format() has no
		// `case float64:` branch: integer-valued `float64` values
		// (e.g. `float64(1.0)`) fall through to `fmt.Sprint(v)`,
		// which renders the bare SQL literal `1` (no decimal).
		// ClickHouse then narrows the parameter to `UInt8`, and the
		// chclient cursor's typed `*float64` scan path errors with
		// `converting UInt8 to *float64 is unsupported`. Tracked
		// upstream at https://github.com/ClickHouse/clickhouse-go/issues/1862;
		// drop the toFloat64 wraps once that lands and we bump past
		// the fixed clickhouse-go version.
		//
		// Non-finite float64 values (±Inf / NaN) cannot ride the
		// positional `?` parameter slot: clickhouse-go and chdb-go
		// both render them via fmt.Sprint / strconv.AppendFloat,
		// which emit the literal strings `+Inf` / `-Inf` / `NaN`
		// (mixed case). Real CH 24.x parses the lowercase forms
		// (`inf` / `-inf` / `nan`) but rejects mixed-case `Inf` /
		// `NaN` as an unknown identifier, surfacing as the wire
		// 502 cerberus sees on `quantile_over_time(-0.5, ...)`
		// post-#322. Emit the value inline as a CH-portable
		// division form (`1.0/0`, `-1.0/0`, `0.0/0`) so the
		// driver never sees a non-finite arg and CH's parser
		// never sees the case-sensitive identifier path.
		if writeInlineNonFinite(b, v.V) {
			return nil
		}
		// Finite floats are wrapped in `toFloat64(?)` unconditionally.
		// The clickhouse-go/v2 driver renders Go `float64(N.0)` as the
		// bare SQL literal `N` (no decimal: its `bind.go::format()` has
		// no `case float64` and falls through to `fmt.Sprint`, which
		// uses Go's `%v` for float64 and prints `1` for whole numbers).
		// CH then narrows to `UInt8`, `UInt8 OP UInt8` promotes to
		// `UInt16`, and `chclient.Sample.Value` (declared `float64`)
		// fails the `UInt8/UInt16 -> *float64` Scan conversion with
		// `converting UInt8 to *float64 is unsupported. try using
		// *uint8` — surfaced as the 502 Grafana sees on
		// `vector(1)+vector(1)` health-probes, `absent(<empty>)`,
		// `group(...)`, the LogQL `1+1` reduce path, etc. Wrapping
		// every LitFloat emission in `toFloat64(?)` pins the wire
		// shape to Float64 from the start so no downstream cast or
		// narrow can chip it down. `toFloat64(toFloat64(x))` is a
		// CH-side no-op so the legacy per-callsite wraps that
		// predated this central fix remain semantically harmless
		// (they just emit `toFloat64(toFloat64(?))`).
		b.sb.WriteString("toFloat64(")
		b.Arg(v.V)
		b.sb.WriteByte(')')
		return nil
	case *chplan.LitBool:
		b.Arg(v.V)
		return nil
	case *chplan.Binary:
		return b.exprBinary(v)
	case *chplan.InList:
		return b.exprInList(v)
	case *chplan.FuncCall:
		return b.exprFunc(v)
	case *chplan.MapAccess:
		return b.exprMapAccess(v)
	case *chplan.MapWithoutKeys:
		return b.exprMapWithoutKeys(v)
	case *chplan.MapWithoutEmptyValues:
		return b.exprMapWithoutEmptyValues(v)
	case *chplan.LabelReplace:
		return b.exprLabelReplace(v)
	case *chplan.LabelJoin:
		return b.exprLabelJoin(v)
	case *chplan.LineContent:
		return b.exprLineContent(v)
	case *chplan.FieldAccess:
		return b.exprFieldAccess(v)
	case *chplan.NestedArrayExists:
		return b.exprNestedArrayExists(v)
	case *chplan.Lambda:
		return b.exprLambda(v)
	case *chplan.BareIdent:
		b.sb.WriteString(v.Name)
		return nil
	case *chplan.Subscript:
		return b.exprSubscript(v)
	case *chplan.ScalarSubquery:
		return b.exprScalarSubquery(v)
	default:
		return fmt.Errorf("%w: expr %T", ErrUnsupported, x)
	}
}

// exprScalarSubquery renders chplan.ScalarSubquery as `(<SELECT ...>)`
// — ClickHouse's scalar-subquery position. The embedded plan is emitted
// through a fresh in-package emitter and its SQL + args spliced into
// this Builder's stream, so positional `?` ordering follows the SQL
// text exactly like every other Expr.
//
// The one-row / one-column contract lives on the chplan.ScalarSubquery
// doc; the Builder only enforces the non-nil invariant.
func (b *Builder) exprScalarSubquery(s *chplan.ScalarSubquery) error {
	if s.Input == nil {
		return fmt.Errorf("%w: chplan.ScalarSubquery has nil Input", ErrUnsupported)
	}
	e := &emitter{}
	if err := e.emitSubquery(s.Input); err != nil {
		return err
	}
	b.sb.WriteString(e.b.String())
	b.args = append(b.args, e.args...)
	return nil
}

// exprLambda renders chplan.Lambda. Single-parameter shapes render as
// `p -> body` (no parens); multi-parameter shapes render as
// `(p1, p2, …) -> body` (with parens) to match CH's conventional
// lambda forms across the array-function family.
func (b *Builder) exprLambda(l *chplan.Lambda) error {
	if len(l.Params) == 0 {
		return fmt.Errorf("%w: chplan.Lambda requires at least one parameter", ErrUnsupported)
	}
	if len(l.Params) == 1 {
		b.sb.WriteString(l.Params[0])
	} else {
		b.sb.WriteByte('(')
		for i, p := range l.Params {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			b.sb.WriteString(p)
		}
		b.sb.WriteByte(')')
	}
	b.sb.WriteString(" -> ")
	if l.Body == nil {
		return fmt.Errorf("%w: chplan.Lambda has nil Body", ErrUnsupported)
	}
	return b.Expr(l.Body)
}

// exprSubscript renders `<container>[<key>]`. No surrounding whitespace.
// Used by the exp-histogram aggregate-merge path to index into
// groupArray-collected per-row arrays.
func (b *Builder) exprSubscript(s *chplan.Subscript) error {
	if s.Container == nil {
		return fmt.Errorf("%w: chplan.Subscript has nil Container", ErrUnsupported)
	}
	if err := b.Expr(s.Container); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	if s.Key == nil {
		return fmt.Errorf("%w: chplan.Subscript has nil Key", ErrUnsupported)
	}
	if err := b.Expr(s.Key); err != nil {
		return err
	}
	b.sb.WriteByte(']')
	return nil
}

// exprNestedArrayExists renders
//
//	arrayExists(x -> x[?] <op> ?, `<Column>`.`<SubField>`)
//
// against the public Builder helpers. Two refinements over the naive
// form:
//
//   - Key == "" means the Nested subfield itself is the comparison
//     subject (e.g. `event:name` → Events.Name, an Array(String)):
//     the lambda compares the bare element — `x <op> ?` — instead of
//     a map lookup.
//   - OpMatch / OpNotMatch render as `match(<elem>, ?)` / `NOT
//     match(<elem>, ?)`: ClickHouse has no `=~` operator, so the raw
//     infix spelling the generic branch writes is a server-side
//     syntax error (the bug TraceQL `{ event.foo =~ "..." }` hit
//     before the showcase pinned it).
//   - Presence != PresenceCompare renders the existence probes for
//     TraceQL nil comparisons: `arrayExists(x -> mapContains(x, ?),
//     …)` (HasKey), `arrayExists(x -> not(mapContains(x, ?)), …)`
//     (LacksKey), and `notEmpty(…)` for the empty-Key HasKey form
//     (nested intrinsics — any element at all).
func (b *Builder) exprNestedArrayExists(n *chplan.NestedArrayExists) error {
	switch n.Presence {
	case chplan.PresenceHasKey, chplan.PresenceLacksKey:
		if n.Key == "" {
			// Any-element probe (event:name != nil and friends): the
			// sub-field is a required column of every Nested element,
			// so presence of any element answers the probe.
			if n.Presence == chplan.PresenceLacksKey {
				b.sb.WriteString("empty(")
			} else {
				b.sb.WriteString("notEmpty(")
			}
			b.QualIdent(n.Column, n.SubField)
			b.sb.WriteByte(')')
			return nil
		}
		b.sb.WriteString("arrayExists(x -> ")
		if n.Presence == chplan.PresenceLacksKey {
			b.sb.WriteString("not(mapContains(x, ")
			b.Arg(n.Key)
			b.sb.WriteString("))")
		} else {
			b.sb.WriteString("mapContains(x, ")
			b.Arg(n.Key)
			b.sb.WriteByte(')')
		}
		b.sb.WriteString(", ")
		b.QualIdent(n.Column, n.SubField)
		b.sb.WriteByte(')')
		return nil
	}
	b.sb.WriteString("arrayExists(x -> ")
	elem := func() {
		b.sb.WriteByte('x')
		if n.Key != "" {
			b.sb.WriteByte('[')
			b.Arg(n.Key)
			b.sb.WriteByte(']')
		}
	}
	switch n.Op {
	case chplan.OpMatch, chplan.OpNotMatch:
		if n.Op == chplan.OpNotMatch {
			b.sb.WriteString("NOT ")
		}
		b.sb.WriteString("match(")
		elem()
		b.sb.WriteString(", ")
		if err := b.Expr(n.Value); err != nil {
			return err
		}
		b.sb.WriteByte(')')
	default:
		elem()
		b.sb.WriteByte(' ')
		b.sb.WriteString(string(n.Op))
		b.sb.WriteByte(' ')
		if err := b.Expr(n.Value); err != nil {
			return err
		}
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
	case chplan.OpAtan2:
		// PromQL `l atan2 r` is Go's math.Atan2(l, r); ClickHouse's
		// atan2(y, x) takes the same argument order, so left/right map
		// positionally. Function-call rendering mirrors OpPow — CH has
		// no infix atan2.
		b.sb.WriteString("atan2(")
		if err := b.Expr(bx.Left); err != nil {
			return err
		}
		b.sb.WriteString(", ")
		if err := b.Expr(bx.Right); err != nil {
			return err
		}
		b.sb.WriteByte(')')
		return nil
	case chplan.OpMod:
		return b.emitGoModulo(bx.Left, bx.Right)
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

// exprInList renders chplan.InList as `(<left> IN (<e0>, <e1>, ...))`
// — a single flat tuple membership test. The flatness is the point:
// the equivalent nested OR-chain of equality Binary nodes deepens
// ClickHouse's parser AST by one level per element and trips
// `max_parser_depth` (default 1000, error code 306) around 1000
// elements — the /api/search root-span lookup hit exactly that on
// >1000-trace result sets. The IN tuple's elements are siblings in
// the AST, so parse depth stays constant no matter how long List is.
//
// Literal elements ride the usual positional `?` bound-arg path via
// b.Expr. The outer parens keep the rendered fragment self-delimiting
// when composed into a larger predicate (same posture as exprBinary's
// default arm).
func (b *Builder) exprInList(v *chplan.InList) error {
	if v.Left == nil {
		return fmt.Errorf("%w: chplan.InList requires a left operand", ErrUnsupported)
	}
	if len(v.List) == 0 {
		// CH rejects `x IN ()` with "Function 'in' is supported only if
		// the second argument is non-empty"; surface the misuse here
		// rather than shipping unparseable SQL.
		return fmt.Errorf("%w: chplan.InList requires a non-empty list", ErrUnsupported)
	}
	b.sb.WriteByte('(')
	if err := b.Expr(v.Left); err != nil {
		return err
	}
	b.sb.WriteString(" IN (")
	for i, e := range v.List {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		if err := b.Expr(e); err != nil {
			return err
		}
	}
	b.sb.WriteString("))")
	return nil
}

// emitGoModulo emits a ClickHouse expression that computes
// `math.Mod(left, right)` bit-exact to Go's `math.Mod` (the function
// Prometheus uses to evaluate `%`). The naive CH path (`left % right`,
// equivalently `left - right*trunc(left/right)`) loses precision at
// the subtraction step relative to Go, which uses Plauger's iterative
// algorithm (Frexp/Ldexp + repeated subtraction) to preserve the
// mantissa. The visible compat-lane symptom is Bucket 2 of #400 — the
// `metric % -7.333…` cases where CH returns exactly 0 while Prom
// returns the float64 residual (~7.33 in magnitude).
//
// Algorithm (matches src/math/mod.go::mod):
//
//   - Special cases:
//     `y == 0` → NaN
//     `x is ±Inf or NaN` → NaN
//     `y is NaN` → NaN
//     `y is ±Inf` → x
//   - Otherwise: let y' = |y|, r = |x|. While r >= y', subtract
//     y' * 2^(rexp - yexp + sign_correction) from r. Result is
//     sign(x) * r.
//
// CH-side encoding: the lambda body uses a triply-nested arrayMap to
// bind each operand exactly once (no re-emission of `left` / `right`
// Frags, so positional `?` placeholders stay aligned). The Plauger
// iteration is unrolled via `arrayFold` over a 64-element index array
// — enough headroom for any finite Float64 ratio (worst case is
// ~log2(MaxFloat64) = 1024, but the loop short-circuits via the
// `acc >= y_abs` guard once r < y'; in practice 30-50 iterations
// suffice for any pair the seed corpus produces). Each iteration
// computes `rexp - yexp` (with the sign-correction for the case
// `y * 2^(rexp-yexp) > r`) and subtracts `y_abs * 2^(...)`.
//
// Bit-exact correspondence against Go's `math.Mod` was verified
// across the audit's failing pair plus 50 random (x, y) pairs in
// `[-2^30, 2^30]` (probe in internal/chsql/builder_test.go).
//
// Cost: ~64 float ops + ~64 comparisons + array materialisation per
// row, all per-chunk-vectorised by CH. For typical compat queries
// (modulo is rare in PromQL workloads — Bucket 2 of #400 covers the
// only two compliance fixtures that use it) the overhead is
// negligible relative to the rest of the query plan.
func (b *Builder) emitGoModulo(left, right chplan.Expr) error {
	// Outer arrayMap binds (x_var, y_var) from singleton arrays so each
	// operand emits exactly once. Inner nested arrayMaps then bind
	// y_abs_var and y_exp_var so abs(y) and frexp(|y|).exp are not
	// recomputed per fold iteration.
	b.sb.WriteString("arrayMap((__mx, __my) -> " +
		"arrayMap(__myabs -> " +
		"arrayMap(__myexp -> " +
		"if(isNaN(__mx) OR isNaN(__my) OR isInfinite(__mx) OR __myabs = 0, " +
		"CAST(0 AS Float64) / 0, " + // NaN
		"if(isInfinite(__myabs), " +
		"__mx, " +
		"if(__mx < 0, CAST(-1 AS Float64), CAST(1 AS Float64)) * arrayFold(" +
		"(__macc, __mi) -> " +
		"if(__macc >= __myabs, " +
		"__macc - exp2(" +
		"if(__myabs * exp2(if(__macc = 0, CAST(0 AS Float64), floor(log2(__macc))) + 1 - __myexp) > __macc, -1, 0) " +
		"+ if(__macc = 0, CAST(0 AS Float64), floor(log2(__macc))) + 1 - __myexp" +
		") * __myabs, " +
		"__macc), " +
		"CAST(range(64) AS Array(UInt8)), " +
		"CAST(abs(__mx) AS Float64))" +
		")), " +
		"[if(__myabs = 0, CAST(0 AS Float64), floor(log2(__myabs)) + 1)])[1], " +
		"[abs(__my)])[1], " +
		"[CAST(ifNull(")
	if err := b.Expr(left); err != nil {
		return err
	}
	// ifNull(<operand>, nan): the operands may be Nullable — the TraceQL
	// numeric-attribute coercion emits toFloat64OrNull(...) so rows
	// without the attribute produce NULL — and CAST(NULL AS Float64)
	// aborts the query (CH error 349). Folding NULL to NaN keeps the
	// modulo emulation's existing contract: NaN operands yield NaN, and
	// IEEE comparisons against NaN are false, so the row simply doesn't
	// match.
	b.sb.WriteString(", nan) AS Float64)], [CAST(ifNull(")
	if err := b.Expr(right); err != nil {
		return err
	}
	b.sb.WriteString(", nan) AS Float64)])[1]")
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
	// Zero keys is the identity: emit the map directly. The degenerate
	// `mapFilter((k, v) -> NOT (k IN ()), m)` is invalid ClickHouse —
	// CH rejects an empty IN list with "Function 'in' is supported only
	// if second argument is constant or table expression". LogQL
	// `max without () (...)` / PromQL `sum without() (...)` reach this
	// shape with an empty exclusion set, which by upstream semantics
	// groups by the full label set.
	if len(m.Keys) == 0 {
		return b.Expr(m.Map)
	}
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

// exprMapWithoutEmptyValues renders
//
//	mapFilter((k, v) -> v != '', <map>)
//
// — the CH expression that strips Map entries whose value is the
// empty string. The empty-string literal is emitted inline (no `?`
// placeholder) because it is part of the query shape, not user data.
//
// PromQL `by(...)` aggregation lowering wraps the per-group-key
// `map('label1', gkey_0, ...)` literal with this so series whose
// grouped-by label was absent in the OTel-CH Attributes Map don't
// surface as `{label1=""}` on the wire — Prom canonicalises an
// empty-valued label to "no label", and so do we.
func (b *Builder) exprMapWithoutEmptyValues(m *chplan.MapWithoutEmptyValues) error {
	b.sb.WriteString("mapFilter((k, v) -> v != '', ")
	if err := b.Expr(m.Map); err != nil {
		return err
	}
	b.sb.WriteByte(')')
	return nil
}

// exprLabelReplace renders PromQL `label_replace(v, dst, replacement, src, regex)`
// over a CH Map(String, String). The PromQL semantics are:
//
//   - if `regex` matches the FULL value of `src` (Prom anchors with `^…$`),
//     bind `dst` to the regex-substituted `replacement`;
//   - otherwise leave `dst` unchanged.
//
// Lowers to:
//
//	mapFilter((k, v) -> v != '',
//	    if(match(<map>[?src], ?anchoredRegex),
//	       mapUpdate(<map>, map(?dst,
//	          if(empty(<map>[?src]),
//	             ?emptyReplacement,
//	             replaceRegexpOne(<map>[?src], ?anchoredRegex, ?replacement)))),
//	       <map>))
//
// `anchoredRegex` is `^<regex>$` so the match is full-string, matching
// Prometheus's `RE2 ^…$` anchoring rule. The outer mapFilter drops the
// dst label when the substituted replacement is the empty string —
// Prom's "labels set to empty values are dropped" rule.
//
// The inner `if(empty(src), emptyReplacement, replaceRegexpOne(…))`
// short-circuit patches CH ≤ 24.8's divergent behaviour where
// `replaceRegexpOne(”, '^(.*)$', 'value-\1')` returns the empty
// string (the input is silently passed through) instead of the
// spec-correct `"value-"`. The build-time pre-computed
// `emptyReplacement` substitutes every capture group with "" — the
// value Go's `ExpandString` (Prom's reference impl) produces against
// an empty match. CH ≥ 25.8 honours `replaceRegexpOne` on empty
// inputs natively; the conditional collapses harmlessly in that
// regime (both branches produce the same string). The compose
// harness's reference Prom is on CH 24.8 so the short-circuit is
// load-bearing on the compatibility lane.
func (b *Builder) exprLabelReplace(l *chplan.LabelReplace) error {
	anchored := "^" + l.Regex + "$"
	b.sb.WriteString("mapFilter((k, v) -> v != '', if(match(")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	b.Arg(l.Src)
	b.sb.WriteString("], ")
	b.Arg(anchored)
	b.sb.WriteString("), mapUpdate(")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteString(", map(")
	b.Arg(l.Dst)
	b.sb.WriteString(", if(empty(")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	b.Arg(l.Src)
	b.sb.WriteString("]), ")
	b.Arg(l.EmptyReplacement)
	b.sb.WriteString(", replaceRegexpOne(")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	b.Arg(l.Src)
	b.sb.WriteString("], ")
	b.Arg(anchored)
	b.sb.WriteString(", ")
	b.Arg(l.Replacement)
	b.sb.WriteString(")))), ")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteString("))")
	return nil
}

// exprLabelJoin renders PromQL `label_join(v, dst, separator, src1, src2, ...)`
// over a CH Map(String, String):
//
//	mapFilter((k, v) -> v != '',
//	    mapUpdate(<map>, map(?dst, arrayStringConcat([<map>[?src1], <map>[?src2], ...], ?separator))))
//
// Missing source labels read as the empty string from CH's Map default;
// the empty-value mapFilter wrapper then drops `dst` if the joined
// result is entirely empty (e.g. join of all-absent labels with an
// empty separator). The match to Prom semantics is: Prom canonicalises
// empty-valued labels to "absent", same as our drop.
func (b *Builder) exprLabelJoin(l *chplan.LabelJoin) error {
	b.sb.WriteString("mapFilter((k, v) -> v != '', mapUpdate(")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteString(", map(")
	b.Arg(l.Dst)
	b.sb.WriteString(", arrayStringConcat([")
	for i, src := range l.Srcs {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		if err := b.Expr(l.Map); err != nil {
			return err
		}
		b.sb.WriteByte('[')
		b.Arg(src)
		b.sb.WriteByte(']')
	}
	b.sb.WriteString("], ")
	b.Arg(l.Separator)
	b.sb.WriteString("))))")
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

// verbatim is the in-package escape for synthetic emitter-chosen
// tokens that don't fit a typed Frag constructor — local CTE / alias
// names pinned by golden fixtures (`_struct_closure`, `_seed`, the
// `_depth` alias), qualified-bare references like `c._depth` /
// `t.<col>` the recursive CTE walks, and the bare `anchor_ts` / `ts`
// references the range-window emitter uses inside arrayFilter / WHERE
// clauses. None of these take user input; the surrounding emitter
// shape pins their lexical form.
//
// Package-private: external packages can't call it; in-package
// callers reach for it sparingly and only for emitter-controlled
// synthetic tokens. The public typed Frag surface (Call, BareIdent,
// InlineLit, Subscript, Array, If, Lambda1, Subquery, …) covers the
// general case.
func verbatim(sql string) Frag {
	return func(b *Builder) { b.sb.WriteString(sql) }
}

// BareIdent returns a Frag that emits name literally — no backtick
// quoting. The narrow trust contract: name MUST be a CH-safe bare
// identifier (the CH grammar requires it to match
// `[a-zA-Z_][a-zA-Z0-9_]*`). Used for lambda parameter names
// (`mapFilter((k, v) -> k IN (?), col)` — `k` is not a column) and
// other emitter-controlled bare tokens.
//
// Prefer Col / Qual for genuine column references — they apply the
// backtick quoting CH expects. BareIdent is for parameter / synthetic
// alias references the emitter pins.
func BareIdent(name string) Frag {
	return func(b *Builder) { b.sb.WriteString(name) }
}

// InlineLit returns a Frag emitting v as an inline literal (no `?`
// placeholder, no positional binding). Supports int64, int, float64,
// and string (single-quoted with CH-style escaping for embedded `'`
// and `\`). Used for values that are part of the query *shape* rather
// than data:
//
//   - array literals `[0, 1, 2]` — the elements are CH-syntax constants;
//   - default sentinel arguments like `toFloat64(0)` where the 0 is the
//     CH expression's shape, not user input;
//   - constants inside lambda predicates the optimizer needs visible
//     (CH's planner can't push a `?`-bound bound through some expression
//     shapes).
//
// Prefer Lit (which uses `?` binding) when the value is user / plan
// data. InlineLit panics for unsupported types so a mis-typed callsite
// surfaces at test time rather than producing wrong SQL.
func InlineLit(v any) Frag {
	return func(b *Builder) {
		switch x := v.(type) {
		case int64:
			b.sb.WriteString(strconv.FormatInt(x, 10))
		case int:
			b.sb.WriteString(strconv.FormatInt(int64(x), 10))
		case float64:
			// Mirror the LitFloat path in Builder.Expr: emit ±Inf
			// / NaN as a CH-portable division form so the SQL the
			// driver assembles never carries the mixed-case
			// identifier strings real CH 24.x rejects.
			if writeInlineNonFinite(b, x) {
				break
			}
			b.sb.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
		case string:
			b.sb.WriteByte('\'')
			for i := 0; i < len(x); i++ {
				c := x[i]
				if c == '\'' || c == '\\' {
					b.sb.WriteByte('\\')
				}
				b.sb.WriteByte(c)
			}
			b.sb.WriteByte('\'')
		default:
			panic(fmt.Sprintf("chsql: InlineLit unsupported type %T", v))
		}
	}
}

// Render materialises a standalone Frag into (sql, args) by rendering it
// against a fresh Builder. Use it when a Frag is itself a complete
// top-level statement — e.g. a UnionAll of SELECT arms run directly as a
// query rather than wrapped in an outer SELECT … FROM (…). Wrapping a
// Map-typed projection in a redundant `SELECT * FROM (…)` boundary makes
// some ClickHouse drivers (chdb) refuse to cast the column back to MAP, so
// the bare-Frag render keeps the union as the top-level SELECT.
func Render(f Frag) (string, []any) {
	b := NewBuilder()
	f(b)
	return b.Build()
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

// Array returns a Frag rendering a CH array literal "[<e0>, <e1>, …]".
// An empty elems list renders as "[]" (CH accepts the empty-array
// literal; its element type is inferred from the surrounding context
// or, if standalone, defaults to `Array(Nothing)`).
//
// Element Frags emit their own `?` placeholders if present; bound args
// land in element order.
func Array(elems ...Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteByte('[')
		writeFragList(b, elems)
		b.sb.WriteByte(']')
	}
}

// Subscript returns a Frag rendering "<container>[<key>]" — CH's Map /
// Array subscript shape (`col[?]`, `arr[idx]`). Both operands are
// rendered through their Frag callbacks so any `?` placeholders bind
// in container-then-key order.
//
// Companion to Builder.MapAt (which is the same shape but with a
// hard-coded bare column name + `?`-bound key); Subscript is the typed
// Frag form for the general case where container and key are arbitrary
// expressions.
func Subscript(container, key Frag) Frag {
	return func(b *Builder) {
		container(b)
		b.sb.WriteByte('[')
		key(b)
		b.sb.WriteByte(']')
	}
}

// If returns a Frag rendering "if(<cond>, <then>, <else>)" — CH's
// ternary `if` function. The fixed-arity wrapper around Call("if", …)
// makes the structural intent grep-able and rejects ill-arity uses at
// compile time.
func If(cond, thenF, elseF Frag) Frag {
	return Call("if", cond, thenF, elseF)
}

// Lambda1 returns a Frag rendering "<param> -> <body>" — a CH
// single-parameter lambda (no parens around the parameter, matching
// CH's conventional shape for `arrayMap(x -> ..., arr)`). For multi-
// parameter lambdas use Lambda2 (or Builder.Lambda for the general
// N-arity case — it wraps params in parens).
//
// param is emitted via BareIdent's trust contract: must be a CH-safe
// bare identifier (`[a-zA-Z_][a-zA-Z0-9_]*`); the caller is responsible.
func Lambda1(param string, body Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteString(param)
		b.sb.WriteString(" -> ")
		body(b)
	}
}

// Lambda2 returns a Frag rendering "(<p1>, <p2>) -> <body>" — a CH
// two-parameter lambda, the shape `arrayMap` / `arrayFilter` /
// `arrayFold` use for paired-array operations like
// `arrayMap((p, c) -> if(c < p, c, c - p), prev, curr)`. Both
// parameter names follow BareIdent's trust contract.
func Lambda2(p1, p2 string, body Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteByte('(')
		b.sb.WriteString(p1)
		b.sb.WriteString(", ")
		b.sb.WriteString(p2)
		b.sb.WriteString(") -> ")
		body(b)
	}
}

// RangeWindowFilter renders
//
//	arrayFilter(p -> tupleElement(p, 1) >  <start>
//	              AND tupleElement(p, 1) <= <end>,
//	            <series>)
//
// — the per-series clamp to the (start, end] window used by every
// range-window emitter. The interval is left-open / right-closed to
// match PromQL range vector selector semantics: a sample at exactly
// t = end - range is *not* part of the window, while a sample at
// exactly t = end is. series is a CH array of (Timestamp, Value)
// tuples (typically the `series_array` alias projected by the
// innermost groupArray + arraySort layer). The lambda parameter `p`
// binds each tuple; `tupleElement(p, 1)` extracts the timestamp.
//
// Composed entirely from typed primitives — no raw SQL writes — so
// the audit grep for clause-keyword cosplay stays clean. The
// start / end / series Frags emit their own `?` placeholders if
// present; bound args land in start → end → series order.
func RangeWindowFilter(start, end, series Frag) Frag {
	tsElem := Call("tupleElement", BareIdent("p"), InlineLit(int64(1)))
	body := And(Gt(tsElem, start), Lte(tsElem, end))
	return Call("arrayFilter", Lambda1("p", body), series)
}

// CounterDelta renders
//
//	arrayMap((p, c) -> if(c < p, c, c - p),
//	         arrayPopBack(arrayMap(x -> tupleElement(x, 2), <seriesArr>)),
//	         arrayPopFront(arrayMap(x -> tupleElement(x, 2), <seriesArr>)))
//
// — the counter-reset-aware pair-wise delta over the values of a CH
// array of (Timestamp, Value) tuples. arrayPopBack drops the last
// element to yield the `prev` sample list; arrayPopFront drops the
// first to yield the `curr` sample list; the lambda pairs them and
// emits `curr - prev` for monotonic moves or `curr` itself when a
// counter reset (curr < prev) is detected.
//
// The result is an Array(Float64); callers typically wrap it in
// `arraySum(...)` to reduce to the scalar delta over the window.
// CounterDelta is intentionally not pre-wrapped so the typed surface
// stays compositional (an emitter that wants the array form — e.g.
// for cumulative-delta debugging — can drop the arraySum).
//
// seriesArr is rendered twice (once into each arrayPopBack /
// arrayPopFront branch). For callers passing a Frag with `?`
// bindings this would double-bind; in practice the emitter always
// passes a bare alias reference (`BareIdent("window_pairs")`) which
// has no args.
func CounterDelta(seriesArr Frag) Frag {
	valsArr := func() Frag {
		return Call(
			"arrayMap",
			Lambda1("x", Call("tupleElement", BareIdent("x"), InlineLit(int64(2)))),
			seriesArr,
		)
	}
	lambdaBody := If(
		Lt(BareIdent("c"), BareIdent("p")),
		BareIdent("c"),
		Sub(BareIdent("c"), BareIdent("p")),
	)
	return Call(
		"arrayMap",
		Lambda2("p", "c", lambdaBody),
		Call("arrayPopBack", valsArr()),
		Call("arrayPopFront", valsArr()),
	)
}

// IfNonZero renders
//
//	if(length(window_vals) > 0, <num> / <denom>, 0.0)
//
// — the divide-by-zero guard used by the LogQL log-rate window
// reducer (and any future *_over_time / *_rate reducer that maps an
// empty window to 0.0 rather than NaN).
//
// The predicate is hard-wired to `length(window_vals) > 0` because
// every callsite operates against the synthetic `window_vals` alias
// the windowed-array emitter projects in its middle layer; threading
// the predicate as a third Frag would just push that constant up to
// every callsite for no structural gain.
func IfNonZero(num, denom Frag) Frag {
	return If(
		Gt(Call("length", BareIdent("window_vals")), InlineLit(int64(0))),
		Div(num, denom),
		// `0.0` is the existing emitter's literal for the empty-window
		// fallback; InlineLit(0.0) would render as `0` (FormatFloat's
		// canonical form) and drift goldens. verbatim is the in-package
		// escape for emitter-pinned synthetic tokens.
		verbatim("0.0"),
	)
}

// Subqueryable is anything that renders as a parameterised SQL
// statement. *QueryBuilder satisfies it; PreRenderedSQL adapts a
// (sql, args) pair from the legacy emitter so its output can flow
// through Subquery without raw-string composition.
type Subqueryable interface {
	Build() (string, []any)
}

// Subquery returns a Frag rendering "(<rendered s>)" — wraps a
// Subqueryable's rendered SQL in parentheses and splices its args at
// the position the Frag emits. Use this to plug a SELECT into another
// QueryBuilder's From slot without flattening to a string first; args
// stay tied to the position they're written at.
//
// Both *QueryBuilder and the chsql-public PreRenderedSQL adapter
// satisfy Subqueryable. The latter is the one documented escape for
// SQL produced by the legacy string emitter (chsql.Emit) — a future
// port can collapse that emitter into the QueryBuilder surface.
func Subquery(s Subqueryable) Frag {
	return func(b *Builder) {
		sql, args := s.Build()
		b.sb.WriteByte('(')
		b.sb.WriteString(sql)
		b.sb.WriteByte(')')
		b.args = append(b.args, args...)
	}
}

// PreRenderedSQL adapts an already-rendered (sql, args) pair into a
// Subqueryable so it can flow through Subquery without raw-string
// composition. Holds an opaque CH SQL string plus its positional args;
// the adapter exists for legacy chsql.Emit output that pre-dates the
// QueryBuilder migration.
//
// Don't reach for this for newly written code — compose with
// QueryBuilder + typed Frags instead.
type PreRenderedSQL struct {
	SQL  string
	Args []any
}

// Build satisfies Subqueryable.
func (p PreRenderedSQL) Build() (string, []any) { return p.SQL, p.Args }

// writeFragList emits Frags comma-separated (with ", " between
// subsequent parts) into the builder. Shared helper for the function-
// call shapes below — keeps the loop pattern in one place rather than
// duplicating it across Call, Parametric, etc.
func writeFragList(b *Builder, parts []Frag) {
	for i, p := range parts {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		p(b)
	}
}

// Call returns a Frag rendering "<name>(<a0>, <a1>, ...)" — a CH
// function call. name is emitted verbatim and is treated as a trusted
// literal (same trust contract as Cast's type-name); callers must
// ensure it's a safe CH function identifier. An empty args list
// renders as "<name>()", which is valid for nullary CH functions like
// now() or today().
func Call(name string, args ...Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteString(name)
		b.sb.WriteByte('(')
		writeFragList(b, args)
		b.sb.WriteByte(')')
	}
}

// Parametric returns a Frag rendering a CH parametric aggregate
// "<name>(<p0>, <p1>, ...)(<a0>, <a1>, ...)" — e.g. quantile(0.5)(col).
// name is a trusted literal (same trust contract as Call / Cast).
// params MUST be non-empty: a parametric aggregate with zero params is
// indistinguishable from a plain Call and the API rejects it to keep
// the typed surface unambiguous. args may be empty if the CH function
// permits it.
//
// See https://clickhouse.com/docs/en/sql-reference/aggregate-functions/parametric-aggregate-functions
// for the CH-side semantics.
func Parametric(name string, params []Frag, args ...Frag) Frag {
	if len(params) == 0 {
		panic("chsql: Parametric requires at least one param; use Call for non-parametric functions")
	}
	return func(b *Builder) {
		b.sb.WriteString(name)
		b.sb.WriteByte('(')
		writeFragList(b, params)
		b.sb.WriteString(")(")
		writeFragList(b, args)
		b.sb.WriteByte(')')
	}
}

// OrderKey pairs a sort expression with its direction for inline use
// in window specifications and similar contexts where the QueryBuilder's
// OrderBy slot isn't a fit. Desc=true renders "<expr> DESC"; false
// renders the bare expression (ASC is the CH default and is left implicit
// to match the existing emitter's render of ORDER BY clauses).
type OrderKey struct {
	Expr Frag
	Desc bool
}

// Window returns a Frag rendering "<fn> OVER (PARTITION BY <p1>, <p2>, ...
// ORDER BY <o1> [DESC], <o2> [DESC], ...)" — a CH window-function
// expression. `partitionBy` empty omits the PARTITION BY clause (the
// window runs over the whole result). `orderBy` empty omits the ORDER BY
// clause. `fn` is rendered before "OVER (...)" — typically a
// `Call("row_number")` or `Call("rank")`.
//
// Used by chplan.TopK's computed-K path (`topk(scalar(...), v)`) where
// the K value comes from a scalar subquery and CH's LIMIT clause can't
// accept that shape. The emitter wraps the topk as
// `row_number() OVER (PARTITION BY <by> ORDER BY <sort> [DESC]) <= K`
// so the per-partition top-K survives without a constant LIMIT.
func Window(fn Frag, partitionBy []Frag, orderBy []OrderKey) Frag {
	return func(b *Builder) {
		fn(b)
		b.sb.WriteString(" OVER (")
		first := true
		if len(partitionBy) > 0 {
			b.sb.WriteString("PARTITION BY ")
			writeFragList(b, partitionBy)
			first = false
		}
		if len(orderBy) > 0 {
			if !first {
				b.sb.WriteByte(' ')
			}
			b.sb.WriteString("ORDER BY ")
			for i, k := range orderBy {
				if i > 0 {
					b.sb.WriteString(", ")
				}
				k.Expr(b)
				if k.Desc {
					b.sb.WriteString(" DESC")
				}
			}
		}
		b.sb.WriteByte(')')
	}
}

// Star returns a Frag rendering "*" — the unqualified wildcard for
// SELECT *. Use QualStar for the qualified "<table>.*" form.
func Star() Frag {
	return func(b *Builder) { b.sb.WriteByte('*') }
}

// QualStar returns a Frag rendering "<table>.*" with the table
// identifier flowing through Ident's backtick quoting (so embedded
// backticks are doubled).
func QualStar(table string) Frag {
	return func(b *Builder) {
		b.Ident(table)
		b.sb.WriteString(".*")
	}
}

// Distinct returns a Frag rendering "DISTINCT <f>". Typically used
// inside the SELECT projection slot to deduplicate the result rows
// on the given expression.
func Distinct(f Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteString("DISTINCT ")
		f(b)
	}
}

// IsNull returns a Frag rendering "<f> IS NULL".
func IsNull(f Frag) Frag {
	return func(b *Builder) {
		f(b)
		b.sb.WriteString(" IS NULL")
	}
}

// IsNotNull returns a Frag rendering "<f> IS NOT NULL".
func IsNotNull(f Frag) Frag {
	return func(b *Builder) {
		f(b)
		b.sb.WriteString(" IS NOT NULL")
	}
}

// Between returns a Frag rendering "<f> BETWEEN <lo> AND <hi>". The
// CH semantics match SQL standard: inclusive on both bounds.
func Between(f, lo, hi Frag) Frag {
	return func(b *Builder) {
		f(b)
		b.sb.WriteString(" BETWEEN ")
		lo(b)
		b.sb.WriteString(" AND ")
		hi(b)
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
	// LeftAntiJoin renders as "LEFT ANTI JOIN" — ClickHouse-specific
	// flavour that returns rows from the left side whose ON predicate
	// matches *no* row on the right. Used by structural_join.go for
	// the negated TraceQL operators (`!>`, `!<`, `!~`, `!>>`, `!<<`).
	LeftAntiJoin JoinKind = "LEFT ANTI JOIN"
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
// recursive CTEs are wired up; the non-recursive shape is unused.
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
// pruning rows before the full row read; the optimizer's PREWHERE
// promotion rule moves predicates from WHERE → PREWHERE when the
// predicate only references sort-key columns. Modelling PREWHERE separately
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
	limitBy    []Frag
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
// Only structural_join.go uses one CTE per emit; the multi-CTE shape
// is unused.
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

// LimitBy appends a partition expression to the CH-specific
// `LIMIT N BY <expr1>, <expr2>, ...` clause, which restricts the
// LIMIT to the first N rows per distinct combination of the BY
// expressions. Calling LimitBy without first calling Limit is a
// no-op (CH requires the LIMIT count).
//
// Used by chplan.TopK to render `topk(K, v) by (g)` as the canonical
// CH idiom — preserves all input columns and only K rows survive
// per group, matching PromQL's topk/bottomk semantics. Empty BY
// renders no `BY` suffix (bare `LIMIT N`).
func (s *QueryBuilder) LimitBy(exprs ...Frag) *QueryBuilder {
	s.limitBy = append(s.limitBy, exprs...)
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
		if len(s.limitBy) > 0 {
			b.sb.WriteString(" BY ")
			for i, f := range s.limitBy {
				if i > 0 {
					b.sb.WriteString(", ")
				}
				f(b)
			}
		}
	}
}
