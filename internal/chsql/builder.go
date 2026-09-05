package chsql

import (
	"fmt"
	"math"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Builder accumulates a parameterised ClickHouse SQL fragment plus the
// positional `?` argument slice that the chclient driver binds.
//
// Builder is the public, named version of the private emitter struct in
// emit.go. It exposes the same `strings.Builder` + `[]any` args
// primitives the emitter uses, plus a handful of CH-specific helpers
// (MapAt, MapKeys, MapFilterExcept, Now64, SubtractNanos,
// DateTime64Lit, Lambda, ParamAgg) and a QueryBuilder with first-class
// PREWHERE, JOIN, and WITH RECURSIVE slots so emitters and endpoint-specific
// callers can compose SQL fragments without re-parsing rendered strings.
//
// The zero value is ready to use.
type Builder struct {
	sb   strings.Builder
	args []any

	// attrStrategies resolves how an attribute-map column that appears as
	// a bare chplan.ColumnRef renders a per-key access (exprMapAccess,
	// the FnMapContainsKey render hook) — see AttrStrategies's doc
	// (attr_strategy.go). nil (the zero value, set by every existing
	// NewBuilder() call) means AttrStrategyMap for every column, so this
	// field is purely additive: nothing that predates cerberus issue
	// #2777 sets it, and every one of those call sites keeps emitting
	// byte-identical SQL.
	attrStrategies AttrStrategies

	// err is the first error Expr encountered while rendering into this
	// Builder, first-error-wins. It exists because Frag has no error
	// return (`func(b *Builder)`), so an expression embedded via a
	// closure — `func(b *Builder) { _ = b.Expr(expr) }`, the shape every
	// QueryBuilder clause slot (Select/Where/Having/OrderBy/...) takes —
	// cannot propagate a chplan rendering error through its own return
	// value. Before this field existed, every such call site paid for a
	// throwaway `(&Builder{}).Expr(expr)` pre-flight render just to catch
	// that error synchronously, ahead of the real render — see #1449.
	// Expr now records the error here as a side effect, so the real
	// render IS the check: Build (both Builder.Build and
	// QueryBuilder.Build) surfaces err, and Subquery/Spliced propagate a
	// nested QueryBuilder's err into the outer Builder they splice into.
	err error
}

// NewBuilder returns an empty Builder. Equivalent to &Builder{}.
func NewBuilder() *Builder { return &Builder{} }

// NewBuilderWithAttrStrategies returns an empty Builder that renders
// attribute-map accesses against s (see AttrStrategies). QueryBuilder's
// internal render path (subquerySQL) is the only caller — external code
// opts in via QueryBuilder.WithAttrStrategies, not this constructor
// directly.
func NewBuilderWithAttrStrategies(s AttrStrategies) *Builder { return &Builder{attrStrategies: s} }

// String returns the accumulated SQL.
func (b *Builder) String() string { return b.sb.String() }

// Args returns the positional argument slice, in the order `?`
// placeholders were emitted. The slice is owned by the Builder; callers
// should not mutate it.
func (b *Builder) Args() []any { return b.args }

// Build is the conventional terminator: returns the rendered SQL, its
// positional argument slice, and the first Expr render error (if any) —
// see Builder.err.
func (b *Builder) Build() (string, []any, error) { return b.sb.String(), b.args, b.err }

// writeSQL appends raw SQL text. Unexported — external packages must
// use the typed surface (QueryBuilder slots + Frag constructors like
// Eq / And / Paren / Cast).
//
// IN-PACKAGE ESCAPE HATCH, NOT A SHORTCUT. No domain emitter calls it:
// every operator-token-style glue site in `internal/chsql` composes typed
// Frags instead, so the only callers are this package's own tests
// (builder_test.go, frag_goldens_test.go). Those tests splice literal
// glue — a `, ` separator, an ` = ` operator, an ` AS L` alias — into
// hand-built token streams that exercise Builder / QueryBuilder plumbing
// (positional-arg binding order, clause placement) directly. Several of
// those streams do have exact typed equivalents: `writeSQL("max(") …
// writeSQL(")")` is Call("max", …), `Ident(x); writeSQL(" AS L")` is
// As(…), `writeSQL("c._depth < "); Arg(5)` is Lt(…). In non-test code
// those equivalents are mandatory — writeSQL must NEVER be reached to
// build a query/expression SHAPE that a Frag constructor already covers:
// any CH function is Call("fn", args…), arithmetic is Mul/Add/Sub/Div,
// and so on. If a shape genuinely has no constructor, add the
// constructor — reaching for writeSQL puts raw SQL back in an emitter
// and the typed surface stops being closed by construction.
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
// post-Projects the Value column with such a literal.
//
// A bound `?` cannot carry the value on every substrate cerberus emits
// for. chdb-go interpolates args through huandu/go-sqlbuilder, whose
// float path is `strconv.AppendFloat(…, 'g', …)`: it renders Go's
// `math.Inf(±1)` / `math.NaN()` as the mixed-case strings `+Inf` /
// `-Inf` / `NaN`, and CH parses only the lowercase forms (`inf` /
// `-inf` / `nan`), surfacing on the wire as `Unknown identifier 'Inf'`
// → 502. clickhouse-go/v2 binds all three correctly as of v2.48.0,
// which quotes them lowercase inside a `cast(…, 'Float64')` — but one
// emitted SQL text serves both binders, so the emitter holds to the
// form that is safe under either. The division forms `1.0/0` /
// `-1.0/0` / `0.0/0` fold to the same IEEE special values on the CH
// side and never reach the lexer's case-sensitive identifier path.
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
// CH's Map column access. col is a single bare column name; for a
// qualified or otherwise composite container, use the typed Frag form
// Subscript(container, key) instead — e.g.
// Subscript(Qual("L", "Attributes"), Lit(key)) for `L`.`Attributes`[?].
//
// cerberus issue #3063 point 2: this is the ad-hoc-query-builder
// equivalent of chplan.MapAccess (exprMapAccess) — internal/api/loki's
// /loki/api/v1/label/<name>/values (label_values.go's mapAtFrag) and
// internal/api/prom's metadata endpoints build their per-key lookup
// through THIS method directly rather than through a chplan tree, so
// wiring exprMapAccess's JSON branch alone left them reading a
// JSON-strategy column with plain bracket-subscript syntax — the exact
// "bypasses chplan entirely" class of gap #3063 names. When col resolves
// to AttrStrategyJSON, this renders the same
// `coalesce(<col>.<key>.:String, the empty string)` shape exprMapAccess's doc explains
// in full (dot-nesting, and the missing-vs-empty normalisation) — with
// key inlined as a backtick-quoted identifier (b.Ident, not b.Arg)
// because ClickHouse's JSON dynamic-subcolumn path is a compile-time
// syntax token, not a bound parameter; safe here because key is always a
// caller-supplied Go string already fixed at SQL-build time (a URL path
// segment / metric label name), never row data. PromQL's metadata.go
// calls into this too, but preflight never resolves AttrStrategyJSON for
// a metrics attribute column (chplan.AttrStrategy's own doc), so this
// branch is a no-op there.
func (b *Builder) MapAt(col, key string) {
	if b.attrStrategies.Lookup(col) == AttrStrategyJSON {
		b.sb.WriteString("coalesce(")
		b.Ident(col)
		b.sb.WriteByte('.')
		b.Ident(key)
		b.sb.WriteString(".:String, '')")
		return
	}
	b.Ident(col)
	b.sb.WriteByte('[')
	b.Arg(key)
	b.sb.WriteByte(']')
}

// MapContains appends "mapContains(<col>, ?)" with key bound as a
// positional argument — CH's Map key-existence check.
//
// cerberus issue #3065 point 2: this is the ad-hoc-query-builder
// equivalent of chplan.FnMapContainsKey (exprMapContainsKey) — MapAt's
// own doc explains why internal/api/loki's ad-hoc query builders need
// their own JSON branch rather than relying on the chplan-level fix
// alone; internal/api/tempo's /api/search/tag/{name}/values
// (search_tag_values.go's mapContainsFrag) has the identical shape: it
// builds its per-key existence pre-filter through THIS method directly
// rather than through a chplan tree. When col resolves to
// AttrStrategyJSON, this renders the same
//
//	has(JSONAllPaths(<col>), ?)
//
// shape exprMapContainsKey's doc explains in full — unlike MapAt's JSON
// branch, key flows through b.Arg (a bound `?` placeholder) exactly like
// the Map branch, since has() takes a runtime argument rather than a
// compile-time dynamic-subcolumn path token.
func (b *Builder) MapContains(col, key string) {
	if b.attrStrategies.Lookup(col) == AttrStrategyJSON {
		b.sb.WriteString("has(JSONAllPaths(")
		b.Ident(col)
		b.sb.WriteString("), ")
		b.Arg(key)
		b.sb.WriteByte(')')
		return
	}
	b.sb.WriteString("mapContains(")
	b.Ident(col)
	b.sb.WriteString(", ")
	b.Arg(key)
	b.sb.WriteByte(')')
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

// nanoScale is the DateTime64 sub-second scale every cerberus timestamp
// fragment uses: 9 fractional digits = nanosecond precision, matching
// the OTel-CH schema's `DateTime64(9)` columns and chplan.NanoScale on
// the IR side.
const nanoScale = 9

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
//
// Besides returning the error to a caller that checks it directly, Expr
// records the first one on b.err (first-error-wins) so a caller that
// embeds it in a Frag closure — which cannot itself return an error —
// still surfaces it once the enclosing Build runs. See Builder.err.
func (b *Builder) Expr(x chplan.Expr) (err error) {
	defer func() {
		if err != nil && b.err == nil {
			b.err = err
		}
	}()
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
	case *chplan.InlineString:
		// Inline single-quoted literal (no `?` binding). Same escaping
		// as InlineLit's string case — backslash-escape embedded `'`
		// and `\`. Used where a `?`-bound string would leave a type
		// indeterminate at CH analysis (e.g. a map-literal key feeding
		// concat); see chplan.InlineString.
		InlineLit(v.V)(b)
		return nil
	case *chplan.LitInt:
		b.Arg(v.V)
		return nil
	case *chplan.LitFloat:
		// LitFloat values ride the positional `?` slot via b.Arg, and
		// the placeholder is wrapped in `toFloat64(?)` here, centrally,
		// so every LitFloat emission is wire-safe by construction. That
		// replaced the per-callsite wraps each emission site used to
		// carry (#634, #644, #646); `toFloat64(toFloat64(x))` is a
		// CH-side no-op, so any that survive elsewhere stay harmless.
		//
		// The wrap exists because a client-side binder that renders an
		// integer-valued `float64` as the bare literal `1` (no decimal)
		// lets CH narrow the parameter to `UInt8`; `UInt8 OP UInt8`
		// then promotes to `UInt16`, and `chclient.Sample.Value`
		// (declared `float64`) fails the Scan conversion with
		// `converting UInt8 to *float64 is unsupported. try using
		// *uint8` — the 502 Grafana shows on `vector(1)+vector(1)`
		// health probes, `absent(<empty>)`, `group(...)`, the LogQL
		// `1+1` reduce path, and friends. Wrapping pins the wire shape
		// to Float64 from the start, so no downstream cast or narrow
		// can chip it down.
		//
		// clickhouse-go/v2 fixed its half of that in v2.48.0
		// (https://github.com/ClickHouse/clickhouse-go/issues/1862):
		// its bind.go renders a bound float as `cast(1, 'Float64')`.
		// The wrap stays regardless, because chsql emits ONE SQL text
		// for both of cerberus's substrates and the other binder is
		// unfixed — chdb-go interpolates through huandu/go-sqlbuilder,
		// whose float path is `strconv.AppendFloat(…, 'g', …)` and
		// still yields the bare `1`. Dropping the wrap would type the
		// same query Float64 on production CH and UInt8 on chDB, which
		// is the substrate divergence the spec lane exists to rule out.
		//
		// Non-finite values (±Inf / NaN) cannot ride the `?` slot at
		// all under the same constraint — writeInlineNonFinite emits
		// them as a CH-portable division form and explains why there.
		if writeInlineNonFinite(b, v.V) {
			return nil
		}
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
	case *chplan.InSubquery:
		return b.exprInSubquery(v)
	case *chplan.BoundedTraceScope:
		return b.exprBoundedTraceScope(v)
	case *chplan.WindowExpr:
		return b.exprWindow(v)
	default:
		return fmt.Errorf("%w: expr %T", ErrUnsupported, x)
	}
}

// exprBoundedTraceScope renders `<TraceId> IN (<top-N newest root traces>)` by
// reusing boundedRootScopeFrag — the SAME self-contained top-N subquery the
// NestedSetAnnotate numbering anchor uses (nested_set_annotate.go), so a
// leaf-scan gate and the numbering scope see a byte-identical trace set. The
// subquery self-parenthesises (QueryBuilder.Frag) and InSubquery adds none, so
// the result is the CH-idiomatic `<TraceId> IN (SELECT …)` with one paren pair.
// When the emit chokepoint has bound the top-N set once
// (chplan.BindBoundedTraceScope stamps BindingAlias), the gate renders
// instead as `has(<alias>, <TraceId>)` against that binding — the same
// trace set, derived once for the whole statement rather than re-scanned at
// every gate. `IN <alias>` is not an option: ClickHouse only accepts a
// constant or a table expression on the right of IN, and a scalar array is
// neither; `has` against the bound array still drives the TraceId
// bloom-filter skip index, so granule pruning is unchanged.
func (b *Builder) exprBoundedTraceScope(s *chplan.BoundedTraceScope) error {
	if s.BindingAlias != "" {
		Call("has", BareIdent(s.BindingAlias), Col(s.TraceIDColumn))(b)
		return nil
	}
	InSubquery(
		Col(s.TraceIDColumn),
		boundedRootScopeFrag(s.SpansTable, s.TraceIDColumn, s.ParentSpanIDColumn, s.TimestampColumn, s.TraceLimit, s.WindowStartNano, s.WindowEndNano),
	)(b)
	return nil
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

// exprInSubquery renders chplan.InSubquery as `<Left> IN (<SELECT ...>)`.
// The embedded plan subtree is rendered through a fresh in-package emitter
// exactly like exprScalarSubquery above, so its args splice into this
// Builder's stream at the position the IN predicate is written; the operator
// itself reuses the top-level InSubquery Frag constructor so the shape
// matches every other `<col> IN (<SELECT ...>)` predicate cerberus emits
// (chplan.BoundedTraceScope, the nested-set anchor/step scope, …).
func (b *Builder) exprInSubquery(v *chplan.InSubquery) error {
	if v.Left == nil {
		return fmt.Errorf("%w: chplan.InSubquery has nil Left", ErrUnsupported)
	}
	if v.Subquery == nil {
		return fmt.Errorf("%w: chplan.InSubquery has nil Subquery", ErrUnsupported)
	}
	e := &emitter{}
	if err := e.emitSubquery(v.Subquery); err != nil {
		return err
	}
	sql, args := e.b.String(), e.args

	var leftErr error
	InSubquery(
		func(lb *Builder) {
			if err := lb.Expr(v.Left); err != nil {
				leftErr = err
			}
		},
		func(sb *Builder) {
			sb.sb.WriteString(sql)
			sb.args = append(sb.args, args...)
		},
	)(b)
	return leftErr
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
//     before the showcase pinned it). The pattern is anchored via
//     [anchoredRegexPattern] — see its doc comment.
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
		if err := b.Expr(anchoredRegexPattern(n.Value)); err != nil {
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

// anchoredRegexPattern wraps a chplan.OpMatch / chplan.OpNotMatch
// pattern expression so ClickHouse's match() — a SUBSTRING search —
// reproduces Prometheus/Loki/Tempo's regex matcher semantics, which
// are ALWAYS fully anchored: `job=~"ap"` must match ONLY `job="ap"`,
// never `job="api"`. Every current chplan.OpMatch/OpNotMatch producer
// (PromQL label + `__name__` matchers, LogQL stream-selector and
// label-filter-stage matchers, TraceQL `=~`/`!~` attribute and nested
// event/link comparisons) borrows this exact anchored-FastRegexMatcher
// semantics from upstream — PromQL and LogQL both compile matchers
// through prometheus/model/labels, and Tempo's own regex evaluator
// (grafana/tempo/pkg/regexp) is built on labels.NewFastRegexMatcher
// too — so anchoring centrally here, at the one SQL emission site both
// render paths share, covers every head without touching any of the
// three lowering packages. See issue #1741.
//
// The non-capturing group is required: `^1|2.5$` alternates between
// `^1` and `2.5$`, not the intended "the whole string is 1 or 2.5" —
// `^(?:1|2.5)$` is the only correct wrap for alternation patterns.
//
// A user-supplied pattern that already carries its own `^`/`$`
// (`job=~"^api$"`) nests safely: `^(?:^api$)$` still matches only
// "api" in RE2 — `^`/`$` are zero-width assertions, so the nested pair
// composes rather than conflicting. Verified against chDB directly
// (see the PR that introduced this function for the empirical
// before/after `match()` calls).
//
// v's shape is almost always *chplan.LitString — every STATIC matcher
// lowerer binds the pattern as a plain string literal — in which case the
// anchors are folded into the Go string before it becomes the bound
// `?` parameter, so the emitted SQL is byte-identical in shape to the
// unanchored form, just a longer parameter value. Any other Expr shape is
// wrapped with CH's concat() instead, composed via the typed FuncCall Frag
// rather than string concatenation — this is a REAL, reachable shape, not
// defensive: TraceQL's dynamic-attribute regex match (`{ .x =~ .y }`) has
// no compile-time pattern to validate (internal/traceql/ast's
// validateRegexPattern explicitly skips a non-Static RHS) and lowers its
// RHS the same way any other operand reaches this function, so v can be a
// bare ColumnRef/MapAccess resolved per span at query time. Pinned by
// builder_test.go's "binary_match_dynamic_pattern" case.
func anchoredRegexPattern(v chplan.Expr) chplan.Expr {
	const anchorPrefix = "^(?:"
	const anchorSuffix = ")$"
	if lit, ok := v.(*chplan.LitString); ok {
		return &chplan.LitString{V: anchorPrefix + lit.V + anchorSuffix}
	}
	return &chplan.FuncCall{
		Fn: chplan.FnConcat,
		Args: []chplan.Expr{
			&chplan.LitString{V: anchorPrefix},
			v,
			&chplan.LitString{V: anchorSuffix},
		},
	}
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
		if err := b.Expr(anchoredRegexPattern(bx.Right)); err != nil {
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
	if v.Negated {
		b.sb.WriteString(" NOT IN (")
	} else {
		b.sb.WriteString(" IN (")
	}
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

// exprFunc renders a FuncCall by resolving its sealed Fn through
// fnResolutions (internal/chsql/fnresolution.go).
//
// chplan.FnMapContainsKey is special-cased ahead of that lookup (see
// exprMapContainsKey) rather than via fnResolution's Render hook: a Render
// function stored in the fnResolutions map that itself calls b.Expr
// transitively reaches resolveFn again (through the generic exprFunc /
// resolveAggFuncName paths), which reads that very map — Go's
// initialization-dependency analysis treats that as a package-init cycle
// even though it is runtime-safe, so the branch has to live here instead.
func (b *Builder) exprFunc(f *chplan.FuncCall) error {
	if f.Fn == chplan.FnMapContainsKey {
		return b.exprMapContainsKey(f.Args)
	}
	if jsonFullMapFns[f.Fn] {
		f = b.substituteJSONFullMapArgs(f)
	}
	name, render, err := resolveFn(f.Fn)
	if err != nil {
		return err
	}
	if render != nil {
		return render(b, f.Args)
	}
	b.sb.WriteString(name)
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

// exprWindow renders chplan.WindowExpr as `<fn>(<Args>) OVER (PARTITION BY
// <PartitionBy>)` via the Window Frag builder — the same window-clause
// idiom chplan.TopK's computed-K path and the vector-set-op / range-window
// emitters already use, generalised to an arbitrary Expr position. An
// empty PartitionBy renders `OVER ()`, ClickHouse's whole-result-set
// partition.
//
// Fn is resolved through the same fnResolutions table exprFunc uses; a Fn
// resolving to a fnRender hook is rejected, mirroring resolveAggFuncName's
// identical restriction for chplan.AggFunc — no current Fn sets one (see
// fnresolution.go's fnRender doc), so this is a forward guard, not a live
// case.
func (b *Builder) exprWindow(w *chplan.WindowExpr) error {
	name, render, err := resolveFn(w.Fn)
	if err != nil {
		return err
	}
	if render != nil {
		return fmt.Errorf("%w: chplan.WindowExpr Fn %q resolves to a chsql render hook, "+
			"not a plain aggregate name — window position cannot use it", ErrUnsupported, w.Fn)
	}

	var firstErr error
	captureErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	args := w.Args
	fnFrag := func(fb *Builder) {
		fb.sb.WriteString(name)
		fb.sb.WriteByte('(')
		for i, a := range args {
			if i > 0 {
				fb.sb.WriteString(", ")
			}
			captureErr(fb.Expr(a))
		}
		fb.sb.WriteByte(')')
	}

	partitionBy := make([]Frag, 0, len(w.PartitionBy))
	for _, p := range w.PartitionBy {
		expr := p
		partitionBy = append(partitionBy, func(fb *Builder) { captureErr(fb.Expr(expr)) })
	}

	Window(fnFrag, partitionBy, nil)(b)
	return firstErr
}

// exprMapAccess renders a chplan.MapAccess. The default (AttrStrategyMap,
// or a Map operand that isn't a bare column — a composed intermediate map
// like mapUpdate/mapConcat's result really IS a ClickHouse Map regardless
// of the strategy of the column it was seeded from) shape is unchanged:
// `<Map>[<Key>]`.
//
// cerberus issue #2777: when m.Map is a bare *chplan.ColumnRef whose
// AttrStrategy (b.attrStrategies) is AttrStrategyJSON, AND m.Key is a
// compile-time literal (*chplan.LitString) — ClickHouse's JSON
// dynamic-subcolumn path syntax is a syntax-level path expression, not a
// function argument, so a non-literal key cannot use it and falls back to
// the Map shape (which will fail at query time against a real JSON column,
// exactly as it did before this issue's work; no plan in this codebase
// currently builds a non-literal MapAccess against a bare attribute-map
// column, see attribute_lookup.go) — the rendering instead reads the
// dynamic subcolumn:
//
//	coalesce(<col>.<key, backtick-quoted>.:String, the empty string)
//
// Two decisions this bakes in, both empirically verified against real
// ClickHouse (26.5, JSON GA since 25.3) rather than assumed:
//
//  1. Dotted OTel keys (http.status_code) nest by ClickHouse's JSON
//     default — inserting {"http.status_code":"200"} creates a two-level
//     path, and a single backtick-quoted identifier carrying the literal
//     dotted string as ONE token reads it back correctly: ClickHouse's
//     JSON path grammar splits on the dot for nesting regardless of
//     backtick quoting, so b.Ident(key) — the same backtick-doubling
//     identifier writer QualIdent uses, safe against a key containing
//     backticks/spaces/anything else — is both the safe AND the correct
//     choice; no manual segment-splitting is needed. This targets
//     ClickHouse's DEFAULT dot-nesting behaviour only; a table whose
//     ingestion path also or ever set json_type_escape_dots_in_keys=1
//     (percent-encoded flat keys, CH 25.8+) is an explicit, documented
//     non-goal of this slice — tracked as cerberus issue #3063's
//     mixed-history hazard.
//  2. Missing-vs-empty semantics deliberately DIFFER between a raw JSON
//     path read and a Map subscript, and the coalesce wrap exists to
//     NORMALISE that difference away at this one rendering boundary: a
//     JSON path read on a missing key returns SQL NULL (verified: a
//     present-but-empty-string value reads back as the empty string, a
//     genuinely absent path reads back NULL — JSON's Dynamic-subcolumn
//     read distinguishes the two), whereas a ClickHouse Map subscript on
//     a missing key returns the value type's default — the empty string,
//     never NULL. Every existing chplan-level lowering
//     (OTelDottedFallbackChain's terminal bare-MapAccess arm, PromQL/LogQL
//     label resolution, ...) was written against the Map contract and
//     relies on bare MapAccess silently defaulting to the empty string for
//     an absent key, using a SEPARATE mapContains/FnMapContainsKey check
//     wherever it needs to distinguish present-empty from absent.
//     Coalescing to the empty string here reproduces that exact contract
//     for a JSON-typed column so every one of those call sites keeps
//     working unmodified against either physical storage; genuine
//     presence/absence still goes through FnMapContainsKey, whose JSON
//     rendering (below) preserves the real distinction via
//     has(JSONAllPaths(...), ...). Pinned by a chDB differential test
//     (attr_strategy_json_chdb_test.go).
func (b *Builder) exprMapAccess(m *chplan.MapAccess) error {
	if col, key, ok := jsonAttrKeyAccess(b, m); ok {
		return b.jsonAttrCoalesce(col, key)
	}
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

// jsonAttrCoalesce renders the shared JSON dynamic-subcolumn read shape
//
//	coalesce(<col>.<path>.:String, '')
//
// that exprMapAccess's and exprFieldAccess's JSON branches both reproduce to
// preserve the Map contract's "absent key reads as the empty string"
// behavior (see exprMapAccess's doc for why). path is always a compile-time
// Go string in both callers (a literal map key via jsonAttrKeyAccess, or a
// FieldAccess.Path), never a chplan.Expr, so it is rendered via b.Ident
// rather than b.Expr/b.Arg.
func (b *Builder) jsonAttrCoalesce(col *chplan.ColumnRef, path string) error {
	b.sb.WriteString("coalesce(")
	if err := b.Expr(col); err != nil {
		return err
	}
	b.sb.WriteByte('.')
	b.Ident(path)
	b.sb.WriteString(".:String, '')")
	return nil
}

// jsonAttrColumn reports whether e is a bare *chplan.ColumnRef whose
// AttrStrategy (per b.attrStrategies) is AttrStrategyJSON — the shared
// precondition exprMapAccess and exprMapContainsKey both branch on.
func jsonAttrColumn(b *Builder, e chplan.Expr) (*chplan.ColumnRef, bool) {
	col, ok := e.(*chplan.ColumnRef)
	if !ok {
		return nil, false
	}
	if b.attrStrategies.Lookup(col.Name) != AttrStrategyJSON {
		return nil, false
	}
	return col, true
}

// jsonAttrKeyAccess reports whether m is a JSON-strategy attribute-map
// access with a compile-time-known key: m.Map is a bare JSON-strategy
// ColumnRef (jsonAttrColumn) and m.Key is a *chplan.LitString. Both
// conditions are required — see exprMapAccess's doc for why a non-literal
// key can't use the JSON dynamic-subcolumn path syntax.
func jsonAttrKeyAccess(b *Builder, m *chplan.MapAccess) (*chplan.ColumnRef, string, bool) {
	col, ok := jsonAttrColumn(b, m.Map)
	if !ok {
		return nil, "", false
	}
	key, ok := m.Key.(*chplan.LitString)
	if !ok {
		return nil, "", false
	}
	return col, key.V, true
}

// exprMapContainsKey renders chplan.FnMapContainsKey. exprFunc dispatches
// here ahead of the generic fnResolutions lookup (see exprFunc's doc for
// why this can't be a fnRender hook stored in that table). The default
// shape — args[0] not a JSON-strategy bare column, or an unexpected arity
// — is unchanged: `mapContains(<args>)`, byte-identical to the plain-Name
// resolution this replaces.
//
// cerberus issue #2777: when args[0] is a bare JSON-strategy ColumnRef,
// existence renders as
//
//	has(JSONAllPaths(<col>), <key>)
//
// — ClickHouse's JSON type reports the FULL dotted OTel key as one leaf
// path string in JSONAllPaths (verified: {"http.status_code":"200"}
// reports the path "http.status_code", not the two intermediate/nested
// segments), so args[1] renders exactly as it would for mapContains — no
// per-segment decomposition needed, and (unlike exprMapAccess's JSON
// branch) args[1] need not be a literal, since has(...) takes a plain
// runtime argument rather than a path-syntax token. This is the real
// present-vs-absent existence check exprMapAccess's coalesce-to-empty-string wrap
// deliberately gives up in exchange for reproducing the Map contract —
// see that function's doc. Pinned by a chDB differential test
// (attr_strategy_json_chdb_test.go).
func (b *Builder) exprMapContainsKey(args []chplan.Expr) error {
	if len(args) == 2 {
		if col, ok := jsonAttrColumn(b, args[0]); ok {
			b.sb.WriteString("has(JSONAllPaths(")
			if err := b.Expr(col); err != nil {
				return err
			}
			b.sb.WriteString("), ")
			if err := b.Expr(args[1]); err != nil {
				return err
			}
			b.sb.WriteByte(')')
			return nil
		}
	}
	b.sb.WriteString("mapContains(")
	for i, a := range args {
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
// `anchoredRegex` is `^(?s:<regex>)$` — see [anchorLabelReplaceRegex] —
// so the match is full-string, matching Prometheus's `label_replace`
// anchoring rule (`promql/functions.go`:
// `"^(?s:" + regexStr + ")$"`). The outer mapFilter drops the dst label
// when the substituted replacement is the empty string — Prom's "labels
// set to empty values are dropped" rule.
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
// regime (both branches produce the same string). The compose +
// compatibility harnesses now run CH 25.8 (reference Prom executes on
// that same harness CH), so emit-side and reference-side moved in
// lock-step — the short-circuit stays because it is forward-safe and
// keeps emission byte-identical across the version move.
func (b *Builder) exprLabelReplace(l *chplan.LabelReplace) error {
	anchored := anchorLabelReplaceRegex(l.Regex)
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
	b.sb.WriteString(", ")
	if err := b.labelReplaceSubstitution(l, anchored); err != nil {
		return err
	}
	b.sb.WriteString("))), ")
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteString("))")
	return nil
}

// anchorLabelReplaceRegex anchors a `label_replace` regex to a
// full-string match, the same way reference Prometheus does
// (`promql/functions.go`: `"^(?s:" + regexStr + ")$"`). ClickHouse's RE2
// engine accepts the `(?s:...)` non-capturing flag group natively, so no
// other change is needed to the emitted pattern.
//
// The `internal/qlcommon` capture-group resolver (`newCaptureGroups`,
// which decides which capture-group index a `$N` / `$name` replacement
// reference in this same LabelReplace resolves to) anchors identically —
// the two must never drift, or the group indices this emitter reads off
// `l.Segments` stop lining up with the regex ClickHouse actually
// evaluates. `internal/chsql` may not import `internal/qlcommon`
// (`.go-arch-lint.yml`), so the anchoring form is duplicated rather than
// shared — see [qlcommon.anchorRegex]'s doc comment for the two
// independent behaviour differences a bare `^...$` gets wrong
// (alternation escaping the anchors, `.` not matching newline).
func anchorLabelReplaceRegex(regex string) string {
	return "^(?s:" + regex + ")$"
}

// labelReplaceSubstitution renders the substituted value itself — the
// branch taken when the regex matched and the source value is non-empty.
//
// Two forms, per chplan.LabelReplace: the `replaceRegexpOne` template,
// and the `concat` over `extractGroups` that carries templates
// referencing a capture group above CH's `\9` ceiling.
func (b *Builder) labelReplaceSubstitution(l *chplan.LabelReplace, anchored string) error {
	if len(l.Segments) == 0 {
		b.sb.WriteString("replaceRegexpOne(")
		if err := b.srcValue(l); err != nil {
			return err
		}
		b.sb.WriteString(", ")
		b.Arg(anchored)
		b.sb.WriteString(", ")
		b.Arg(l.Replacement)
		b.sb.WriteByte(')')
		return nil
	}
	// `concat` needs at least one argument, and a decomposition is never
	// empty when it exists — a template that produced no segments has an
	// empty replacement and takes the template path above.
	b.sb.WriteString("concat(")
	for i, seg := range l.Segments {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		if err := b.labelReplaceSegment(l, seg, anchored); err != nil {
			return err
		}
	}
	b.sb.WriteByte(')')
	return nil
}

// labelReplaceSegment renders one decomposed replacement segment.
//
// A literal run binds as a parameter. A capture reference indexes
// `extractGroups`, which returns every group as an `Array(String)` and so
// has no substitution ceiling; out-of-range and no-match indexing both
// yield the empty string, which is what Go's ExpandString substitutes for
// a group that bound to nothing. The whole-match group is read off the
// source value directly — the regex is anchored, so a match spans the
// entire source string, and `extractGroups` numbers from the first real
// group.
//
// A segment carrying Fallbacks references a NAME that several capture
// groups share. Go's ExpandString expands it to the first of those groups
// that took part in the match, and the lowering only produces this shape
// once it has proved that no match can pair a first participant capturing
// the EMPTY string with a later listed group capturing text — so over the
// groups it listed, "took part" and "captured something non-empty" pick
// the same one, and the choice becomes an ordinary array search:
//
//	arrayFirst(x -> x != '', [<subscript>, <subscript>, …])
//
// `arrayFirst` returns the element type's default — the empty string —
// when no element qualifies, which is what ExpandString substitutes when
// none of the like-named groups took part. See
// chplan.LabelReplaceSegment.Fallbacks.
func (b *Builder) labelReplaceSegment(l *chplan.LabelReplace, seg chplan.LabelReplaceSegment, anchored string) error {
	switch seg.Group {
	case chplan.NoCaptureGroup:
		b.Arg(seg.Literal)
		return nil
	case chplan.WholeMatchGroup:
		return b.srcValue(l)
	}
	// srcValue renders a plan sub-expression and can fail, while a Frag
	// cannot report an error. The closure records the first failure and
	// the caller surfaces it once rendering is done.
	var srcErr error
	src := Frag(func(fb *Builder) {
		if err := fb.srcValue(l); err != nil && srcErr == nil {
			srcErr = err
		}
	})
	extract := labelReplaceExtractRegex(l, anchored)
	if len(seg.Fallbacks) == 0 {
		captureGroupAt(src, extract, seg.Group)(b)
		return srcErr
	}
	candidates := make([]Frag, 0, 1+len(seg.Fallbacks))
	candidates = append(candidates, captureGroupAt(src, extract, seg.Group))
	for _, idx := range seg.Fallbacks {
		candidates = append(candidates, captureGroupAt(src, extract, idx))
	}
	if len(seg.NegativeProbes) != 0 {
		args := make([]Frag, 0, 2*len(candidates)+1)
		for i, candidate := range candidates {
			witness := candidate
			if len(seg.Probes) != 0 {
				witness = captureGroupAt(src, extract, seg.Probes[i])
			}
			condition := Neq(witness, Lit(""))
			if len(seg.NegativeProbes[i]) > 0 {
				// A nullable carrier's own capture may be empty even when it
				// participated. Here the mandatory alternation selected its
				// branch exactly when none of its non-empty siblings did.
				condition = Eq(captureGroupAt(src, extract, seg.NegativeProbes[i][0]), Lit(""))
				for _, idx := range seg.NegativeProbes[i][1:] {
					condition = And(condition, Eq(captureGroupAt(src, extract, idx), Lit("")))
				}
			}
			args = append(args, condition, candidate)
		}
		args = append(args, Lit(""))
		Call("multiIf", args...)(b)
		return srcErr
	}
	if len(seg.Probes) == 0 {
		Call(
			"arrayFirst",
			Lambda1(labelReplaceCandidateParam, Neq(BareIdent(labelReplaceCandidateParam), Lit(""))),
			Array(candidates...),
		)(b)
		return srcErr
	}
	// A probed carrier is one whose own capture cannot say whether it took
	// part in the match, so the test and the answer read different groups.
	// `arrayFirst` over TWO arrays is exactly that shape: the predicate
	// runs over the participation array, and the element returned is the
	// one at the same position of the value array.
	witnesses := make([]Frag, 0, len(seg.Probes))
	for _, idx := range seg.Probes {
		witnesses = append(witnesses, captureGroupAt(src, extract, idx))
	}
	Call(
		"arrayFirst",
		Lambda2(labelReplaceCandidateParam, labelReplaceWitnessParam,
			Neq(BareIdent(labelReplaceWitnessParam), Lit(""))),
		Array(candidates...),
		Array(witnesses...),
	)(b)
	return srcErr
}

// labelReplaceExtractRegex is the pattern the `extractGroups` calls read.
// It is the plan's probed rewrite when it has one — whose synthetic
// groups the segment indices are numbered against — and the plain
// anchored regex otherwise.
func labelReplaceExtractRegex(l *chplan.LabelReplace, anchored string) string {
	if l.ProbedRegex == "" {
		return anchored
	}
	return anchorLabelReplaceRegex(l.ProbedRegex)
}

// labelReplaceCandidateParam is the lambda parameter naming one
// like-named capture group's capture inside the `arrayFirst` search
// labelReplaceSegment emits. It is emitter-chosen and never collides with
// a column: CH resolves a lambda parameter ahead of any outer name.
const labelReplaceCandidateParam = "x"

// labelReplaceWitnessParam is the second lambda parameter of the
// two-array `arrayFirst` search, naming the capture whose non-emptiness
// reports that the candidate beside it took part in the match. Like
// labelReplaceCandidateParam it is emitter-chosen, and CH resolves a
// lambda parameter ahead of any outer name.
const labelReplaceWitnessParam = "p"

// captureGroupAt returns a Frag for one capture group's value:
// `extractGroups(<src>, <anchored>)[<group>]`.
func captureGroupAt(src Frag, anchored string, group int) Frag {
	return Subscript(
		Call("extractGroups", src, Lit(anchored)),
		Lit(int64(group)),
	)
}

// srcValue renders the source label's value: `<map>[<src>]`. CH map
// subscript of a missing key returns the empty string, which is how
// PromQL reads an absent source label.
func (b *Builder) srcValue(l *chplan.LabelReplace) error {
	if err := b.Expr(l.Map); err != nil {
		return err
	}
	b.sb.WriteByte('[')
	b.Arg(l.Src)
	b.sb.WriteByte(']')
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

// textIndexLikeMinTokenLength is the minimum literal-word length (in
// runes) exprLineContent will emit as a `lower(<Source>) LIKE '%tok%'`
// prefilter conjunct (chopt text_index_line_filter, cerberus issue #2773).
// Mirrors ClickHouse's own text_index_like_min_pattern_length default —
// confirmed via `SELECT value FROM system.settings WHERE name =
// 'text_index_like_min_pattern_length'` against a live 26.4+ server, which
// reports 4. A word shorter than this is still logically implied by the
// row predicate if emitted (the superset guarantee doesn't depend on
// length), but the server's own LIKE-via-text-index dictionary scan can't
// use it for pruning, so dropping it keeps the WHERE clause from growing
// with conjuncts that add per-row evaluation cost with no offsetting
// benefit.
const textIndexLikeMinTokenLength = 4

// asciiLower lowercases only ASCII 'A'-'Z' bytes, leaving every other byte
// — including any UTF-8 continuation byte of a non-ASCII rune — untouched.
// This matches ClickHouse's `lower()` function EXACTLY: `lower()` is
// ASCII-only (confirmed live: `lower('CAFÉ')` → `cafÉ`, the trailing 'É'
// unchanged); `lowerUTF8()` is the separate, Unicode-aware function CH
// offers, and idx_lower_body's DDL expression uses plain `lower(Body)`, not
// lowerUTF8. Go's Unicode-aware strings.ToLower would silently diverge from
// what `lower(Body)` actually contains for any non-ASCII letter, breaking
// the LIKE prefilter's strict-superset guarantee: a needle lowered
// differently than the column it is matched against can mismatch a row the
// exact row predicate DOES match, and the surrounding AND would then wrongly
// drop it.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// escapeLikeLiteral escapes s for embedding inside a ClickHouse LIKE
// pattern's literal portion: backslash (CH's LIKE escape character) and the
// two LIKE metacharacters, `%` and `_`. All three replacements run in ONE
// strings.Replacer pass over the ORIGINAL bytes of s, so a backslash
// introduced by escaping a `%` or `_` is never itself re-escaped — the
// double-escaping bug a naive sequential ReplaceAll chain would have.
// Needed for real log content: an unescaped `_` in a word like "user_id" is
// read by LIKE as the any-single-char wildcard rather than a literal
// underscore, and an unescaped `%` in "92% done" is read as the any-
// sequence wildcard — both would silently widen a conjunct's match set
// beyond the true substring it is meant to test for.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLikeLiteral(s string) string {
	return likeEscaper.Replace(s)
}

// textIndexLikeTokens splits literal into the whitespace-delimited words
// exprLineContent's ANDed LIKE prefilter checks (cerberus issue #2773) —
// "for multi-word |= literals, emit ANDed per-token LIKE conjuncts" in the
// issue's own words, where a multi-word literal's "tokens" are its words.
// A word shorter than textIndexLikeMinTokenLength is dropped (see that
// constant's doc). A literal with no qualifying word returns nil, telling
// the caller to skip the prefilter and fall back to the exact predicate
// alone.
func textIndexLikeTokens(literal string) []string {
	fields := strings.Fields(literal)
	tokens := make([]string, 0, len(fields))
	for _, f := range fields {
		if utf8.RuneCountInString(f) >= textIndexLikeMinTokenLength {
			tokens = append(tokens, f)
		}
	}
	return tokens
}

// textIndexRegexLiteral reports whether pattern is safely extractable as a
// plain substring literal for the ANDed LIKE prefilter (cerberus issue
// #2773): pattern is parsed with regexp/syntax under RE2 semantics — the
// same engine ClickHouse's match() runs, per internal/logql/lower.go's own
// regexpMergeLabels precedent — and simplified. Only when the result is a
// SINGLE OpLiteral (no anchors, alternation, character classes,
// quantifiers, or any other RE2 construct survives Simplify) is "matches
// this regex" provably equivalent to "contains this substring" for CH's
// unanchored match() search — the equivalence the LIKE prefilter's
// strict-superset argument depends on. Any other pattern shape returns
// ok=false and the caller falls back to match()-only, the boundary the
// issue itself calls for: partial literal-prefix/substring extraction from
// an arbitrary RE2 AST (alternation branches, anchors, repetition) is
// deliberately NOT attempted — this package does not compile RE2 semantics
// into index predicates, only recognizes the special case where a "regex"
// carries none.
func textIndexRegexLiteral(pattern string) (string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	if re.Op != syntax.OpLiteral {
		return "", false
	}
	return string(re.Rune), true
}

// textIndexPrefilterArgs returns the fully-escaped `%tok%` LIKE needles
// exprLineContent should AND together ahead of l's row predicate, or nil
// when no prefilter applies:
//
//   - l.TextIndexPrefilter is false (chopt text_index_line_filter is
//     disabled, or the lowerer never set it) — the whole rewrite is
//     inert, matching the plain predicate byte-for-byte.
//   - l.Negated is true — a superset prefilter has no sound dual for a
//     "must not contain" predicate; see LineContent.TextIndexPrefilter's
//     doc comment.
//   - l.IsRegex is true and the Pattern is not a plain literal in
//     disguise (textIndexRegexLiteral).
//   - the extracted literal has no word long enough to be worth
//     prefiltering (textIndexLikeTokens).
func textIndexPrefilterArgs(l *chplan.LineContent) []string {
	if !l.TextIndexPrefilter || l.Negated {
		return nil
	}
	literal := l.Pattern
	if l.IsRegex {
		lit, ok := textIndexRegexLiteral(l.Pattern)
		if !ok {
			return nil
		}
		literal = lit
	}
	words := textIndexLikeTokens(literal)
	if len(words) == 0 {
		return nil
	}
	args := make([]string, len(words))
	for i, w := range words {
		args[i] = "%" + escapeLikeLiteral(asciiLower(w)) + "%"
	}
	return args
}

func (b *Builder) exprLineContent(l *chplan.LineContent) error {
	renderRow := func() error {
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

	prefilterArgs := textIndexPrefilterArgs(l)
	if len(prefilterArgs) == 0 {
		return renderRow()
	}

	// Strict-superset prefilter (cerberus issue #2773): each conjunct below
	// can only ELIMINATE granules/rows the row predicate rendered by
	// renderRow() would also have rejected — see
	// LineContent.TextIndexPrefilter's doc comment — so ANDing them ahead of
	// the unchanged row predicate never changes the result set, only how
	// much of Body ClickHouse has to decompress and scan to compute it.
	b.sb.WriteByte('(')
	for _, arg := range prefilterArgs {
		b.sb.WriteString("lower(")
		if err := b.Expr(l.Source); err != nil {
			return err
		}
		b.sb.WriteString(") LIKE ")
		b.Arg(arg)
		b.sb.WriteString(" AND ")
	}
	if err := renderRow(); err != nil {
		return err
	}
	b.sb.WriteByte(')')
	return nil
}

// exprFieldAccess renders a chplan.FieldAccess. A materialized attribute
// column reads byte-identical to the Map subscript below (see
// chplan.FieldAccess.MaterializedColumn's doc and
// AddColumnBuilder.Default's real-ClickHouse evidence) but avoids
// decompressing the wide Attributes Map, so the emitter prefers it
// whenever the lowering layer populated it — checked first,
// unconditionally, since a materialized reference never touches the
// carrier map at all regardless of the carrier's AttrStrategy.
//
// cerberus issue #3062: FieldAccess is "conceptually a generalised
// MapAccess" (see its own doc) but is a DISTINCT chplan.Expr type, so it
// does not automatically inherit exprMapAccess's JSON branch — TraceQL's
// lowerAttribute (internal/traceql/lower.go) builds FieldAccess, never a
// bare MapAccess, for every span/resource/scope attribute read, which is
// precisely why wiring chsql.AttrStrategies end-to-end was NOT sufficient
// on its own to make a JSON-typed traces attribute column work: this
// method is the second half of that fix. f.Path is always a compile-time
// Go string (not a chplan.Expr), so — unlike exprMapAccess's key, which
// must additionally be checked for *chplan.LitString — the JSON branch
// here needs only jsonAttrColumn's bare-JSON-strategy-column check on
// f.Source. Renders the identical
//
//	coalesce(<col>.<path, backtick-quoted>.:String, '')
//
// shape exprMapAccess's JSON branch does, and for the same reason: every
// TraceQL comparison lowering (coerceFieldAccess's toFloat64OrNull wrap,
// equality, regex) is written against the Map contract of "absent key
// reads as the empty string", so normalising the JSON path's NULL-on-
// missing behaviour away here keeps every one of those lowerings working
// unmodified against either physical storage — pinned by a chDB
// differential (internal/api/tempo/attr_strategy_json_chdb_test.go)
// exercising both
// the FieldAccess read itself and a numeric comparison built on top of
// it.
func (b *Builder) exprFieldAccess(f *chplan.FieldAccess) error {
	if f.MaterializedColumn != "" {
		b.Ident(f.MaterializedColumn)
		return nil
	}
	if col, ok := jsonAttrColumn(b, f.Source); ok {
		return b.jsonAttrCoalesce(col, f.Path)
	}
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

// ddlToken emits a fixed ClickHouse DDL keyword / punctuation token
// verbatim. It is the closed-surface primitive backing the typed DDL
// statement builders in ddl.go (CreateDatabase and the ON CLUSTER /
// ENGINE / TTL clause constructors) — the DDL counterpart of the clause
// keywords QueryBuilder.writeInto writes for SELECT. The argument is
// ALWAYS a compile-time-constant DDL token (e.g. "CREATE DATABASE ",
// " ENGINE = ", "IF NOT EXISTS "), never user, plan, or config data —
// names flow through Ident / BareIdent and values through InlineLit /
// Call. Keeping it here (not in ddl.go) is what lets the DDL builders
// compose statements without any raw sb.Write of their own, so the
// "tokens are written only in builder.go" discipline holds.
func ddlToken(s string) Frag {
	return func(b *Builder) { b.sb.WriteString(s) }
}

// BareIdent returns a Frag that emits name literally — no backtick
// quoting. The narrow trust contract: name MUST be emitter/operator-fixed
// text, never user or query data — never a value that could carry
// attacker- or tenant-controlled bytes through to raw SQL. The typical
// shape is a CH-safe bare identifier (the CH grammar requires
// `[a-zA-Z_][a-zA-Z0-9_]*`) — lambda parameter names
// (`mapFilter((k, v) -> k IN (?), col)` — `k` is not a column) and other
// emitter-controlled bare tokens. One documented exception: a
// data-skipping index TYPE clause (AddIndexBuilder.frag, indexType) may
// legitimately be a parametrized CH type keyword like `set(100)` rather
// than a bare identifier — still safe under this contract (indexType is
// always a fixed, code-authored string, never data), just outside the
// identifier grammar the common case follows.
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
		case bool:
			// Bare CH boolean literal (true / false) — the form a
			// ClickHouse SETTINGS / DDL value takes when it isn't a
			// quoted string. ClickHouse accepts the bare keywords for
			// boolean settings.
			b.sb.WriteString(strconv.FormatBool(x))
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
	// Render's callers (RenderDDL) only ever hand it DDL Frags, which
	// compose Ident / InlineLit / Call and never reach chplan.Expr — so
	// b.err is always nil here. Discarding it keeps Render's signature
	// stable for its one production caller; see Builder.err.
	sql, args, _ := b.Build()
	return sql, args
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
//
// It has no caller in the emitters, and a new one needs a reason. A CH
// Map compares POSITIONALLY over its (keys, values) arrays, so a full-row
// dedup over any projection carrying ResourceAttributes / SpanAttributes
// treats one span redelivered under a different OTLP attribute key order
// as two rows. Every span-row union in this package therefore dedupes on
// span IDENTITY instead — `UnionAll` + `LIMIT 1 BY (TraceId, SpanId)`;
// see emitStructuralSpanUnion (structural_join.go) and emitSetOperation
// (set_op.go). Reach for UnionDistinct only where the dedup tuple is
// genuinely the whole row AND provably carries no Map column.
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

// RawAs wraps expr in "<expr> AS <bareAlias>" with the alias emitted
// VERBATIM (no backticks). It is the un-quoted sibling of As, for the
// windowed-array idiom's internal aliases (`series_array`,
// `window_pairs`, `anchor_ts`, …) that are emitter-chosen, never
// user-supplied, and must stay un-backticked to keep the byte-level
// golden fixtures stable. The alias flows through verbatim, which is
// what keeps the AS keyword + alias inside builder.go's closed token
// surface rather than a raw sb.Write at the call site. Empty bareAlias
// renders the expression bare (no AS clause).
func RawAs(expr Frag, bareAlias string) Frag {
	if bareAlias == "" {
		return expr
	}
	return func(b *Builder) {
		expr(b)
		verbatim(" AS " + bareAlias)(b)
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

// TupleIndex returns a Frag rendering "<tuple>.<n>" — CH's 1-based
// positional tuple-element access (`t.1`, `t.2`). n is written directly
// into the SQL text rather than bound as a `?` placeholder: CH's tuple
// dot-index syntax requires a compile-time integer literal at that
// position, not a parameter. n must be >= 1 (CH tuple indices are
// 1-based); callers pass a small, self-evident positional constant
// (invariant 13's own carve-out), never a computed or business value.
func TupleIndex(tuple Frag, n int) Frag {
	if n < 1 {
		panic(fmt.Sprintf("chsql: TupleIndex n must be >= 1 (1-based), got %d", n))
	}
	return func(b *Builder) {
		tuple(b)
		b.sb.WriteByte('.')
		b.sb.WriteString(strconv.Itoa(n))
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

// CounterOrDeltaSum renders the window's total increase over `pairs`
// (an Array(Tuple(ts, value)), typically `window_pairs`, ascending by
// timestamp), branching at RUNTIME on a per-series-window
// AggregationTemporality read (`temporality`) between:
//
//   - DELTA (schema.AggregationTemporalityDelta): each stored sample is
//     already the increase since the PREVIOUS sample only, so the
//     window's total increase is the sum of its raw values EXCLUDING the
//     earliest (first-in-time) sample —
//     `arraySum(arrayMap(x -> tupleElement(x, 2), pairs)) - tupleElement(pairs[1], 2)`.
//     The earliest sample's own value already covers the interval ENDING
//     at (and therefore starting BEFORE) `pairs[1]`'s timestamp — the same
//     coverage the shared extrapolation layer's `duration_to_start`
//     independently reconstructs from the window's edge to `first_ts`.
//     Summing every raw value including that first one would double-count
//     that leading interval: it both feeds the raw sum AND gets
//     re-extrapolated via `first_ts`. Dropping it makes this the exact
//     analogue of the CUMULATIVE branch's `last - first` telescoping sum
//     (a running total built by prefix-summing these same DELTA values
//     satisfies `running(last_ts) - running(first_ts) ==
//     sum(pairs[2:]) `, an identity independent of how uniform the deltas
//     are), which is what makes a DELTA counter and its equivalent
//     CUMULATIVE running-total counter agree exactly, not just
//     approximately, once the shared extrapolation factor is layered on
//     top of either.
//   - anything else (CUMULATIVE, or the legacy zero/UNSPECIFIED
//     reading): Prometheus's counter-reset-aware delta,
//     `arraySum(CounterDelta(pairs))` — the historical, pre-#1628
//     behaviour every rate / increase lowering applied unconditionally.
//
// temporality is nil for callers whose RangeWindow carries no
// TemporalityColumn (Gauge-table input, or the Scan.UnionTables
// cross-table routing, which drops the Sum-only column from its
// projection so the union's column list still matches) — with a nil
// temporality, CounterOrDeltaSum renders the CUMULATIVE branch
// unconditionally, byte-identical to every query emitted before #1628.
//
// This is the ONE branch every counter-reading range function shares:
// rate / increase call it here; the classic-histogram bucket fold
// (internal/promql's counterIncreaseFold) transcribes the identical
// branch into the bucket-array domain, since that path builds a
// chplan.Expr tree rather than a chsql.Frag and so cannot call this
// function directly — see counterIncreaseFold's doc comment for the
// cross-reference. See issue #1628.
func CounterOrDeltaSum(pairs, temporality Frag) Frag {
	cumulative := Call("arraySum", CounterDelta(pairs))
	if temporality == nil {
		return cumulative
	}
	firstVal := tupleElemFrag(Subscript(pairs, InlineLit(int64(1))), 2)
	delta := Sub(Call("arraySum", pairsValuesFrag(pairs)), firstVal)
	return If(Eq(temporality, InlineLit(schema.AggregationTemporalityDelta)), delta, cumulative)
}

// CounterOrDeltaPairDelta renders the increase between ONE adjacent pair
// of counter samples — `prev` at the earlier timestamp, `curr` at the
// later one — branching at RUNTIME on the same per-series
// AggregationTemporality read CounterOrDeltaSum branches on:
//
//   - DELTA (schema.AggregationTemporalityDelta): `curr` is ALREADY the
//     increase over the interval ending at its own timestamp, which for
//     an adjacent pair is exactly `(prev_ts, curr_ts]` — so the pair's
//     increase is the raw `curr` value, nothing subtracted. This is
//     CounterOrDeltaSum's DELTA branch specialised to a two-element
//     window: `sum(values) - values[first]` over `[prev, curr]` is
//     `prev + curr - prev`, i.e. `curr`.
//   - anything else (CUMULATIVE, or the legacy zero/UNSPECIFIED
//     reading): Prometheus's counter-reset-aware pair delta,
//     `if(curr < prev, curr, curr - prev)` — the historical,
//     pre-#1628 behaviour irate applied unconditionally.
//
// temporality is nil for callers whose RangeWindow carries no
// TemporalityColumn, and renders the CUMULATIVE branch alone —
// byte-identical to every query emitted before this branch existed.
//
// This is the two-sample sibling of CounterOrDeltaSum: `irate` reads
// only the window's last two samples, so it needs the pair form rather
// than the whole-window sum. `idelta` deliberately does NOT call it —
// it is a gauge function that never applied the counter-reset rule in
// the first place, so it has no temporality-dependent branch. See
// issue #1963.
//
// prev and curr are each rendered up to twice (once per branch), so
// callers pass bare token Frags (a tupleElement over an aliased array),
// never a Frag carrying `?` bindings.
func CounterOrDeltaPairDelta(prev, curr func() Frag, temporality Frag) Frag {
	cumulative := If(Lt(curr(), prev()), curr(), Sub(curr(), prev()))
	if temporality == nil {
		return cumulative
	}
	return If(Eq(temporality, InlineLit(schema.AggregationTemporalityDelta)), curr(), cumulative)
}

// pairsValuesFrag renders `arrayMap(x -> tupleElement(x, 2), <pairs>)`
// — the values-only projection out of an arbitrary Array(Tuple(ts,
// value)) Frag. Distinct from range_window.go's windowValsFrag, which
// hard-wires the `window_pairs` alias; this variant takes the pairs
// Frag as an argument so CounterOrDeltaSum works for both the
// `window_pairs`-aliased row-shape path and any future caller with a
// differently-named or inline pairs expression.
func pairsValuesFrag(pairs Frag) Frag {
	return Call(
		"arrayMap",
		Lambda1("x", Call("tupleElement", BareIdent("x"), InlineLit(int64(2)))),
		pairs,
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
//
// subquerySQL is deliberately unexported (rather than named Build) so
// *QueryBuilder's public, two-value Build() — consumed across
// internal/api/{prom,loki,tempo}, internal/optcorpus,
// internal/routerrules and internal/preflight — keeps its existing
// signature. Only the two types chsql itself defines implement
// Subqueryable, so the unexported method is satisfiable without
// reaching outside this package.
type Subqueryable interface {
	subquerySQL() (string, []any, error)
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
		sql, args, err := s.subquerySQL()
		if err != nil && b.err == nil {
			b.err = err
		}
		b.sb.WriteByte('(')
		b.sb.WriteString(sql)
		b.sb.WriteByte(')')
		b.args = append(b.args, args...)
	}
}

// Spliced returns a Frag that splices an already-rendered (sql, args)
// pair into the stream verbatim — NO surrounding parens, unlike
// Subquery. Used when the rendered SELECT must sit bare in a context
// that supplies its own parens (e.g. the right-hand side of the list-
// form In, which parenthesises its arguments): wrapping it in Subquery
// would double-paren. The args splice at the position the Frag emits so
// positional `?` ordering follows the SQL text. The narrow contract:
// sql is emitter-rendered SQL (a QueryBuilder.Build() result), never
// user input.
func Spliced(s Subqueryable) Frag {
	return func(b *Builder) {
		sql, args, err := s.subquerySQL()
		if err != nil && b.err == nil {
			b.err = err
		}
		b.sb.WriteString(sql)
		b.args = append(b.args, args...)
	}
}

// PreRenderedSQL adapts an already-rendered (sql, args) pair into a
// Subqueryable so it can flow through Subquery without raw-string
// composition. Holds an opaque CH SQL string plus its positional args;
// it carries legacy chsql.Emit output that pre-dates the QueryBuilder
// migration, and the emitter's own recursive render (emitter.renderNode →
// subqueryFrag), whose child statement is text by the time the parent
// composes it.
//
// Don't reach for this to BUILD SQL — compose with QueryBuilder + typed
// Frags instead. Its only legitimate input is emitter-rendered text.
type PreRenderedSQL struct {
	SQL  string
	Args []any
}

// subquerySQL satisfies Subqueryable. p.SQL is always already-rendered
// text (from the legacy string emitter), so it never carries a render
// error.
func (p PreRenderedSQL) subquerySQL() (string, []any, error) { return p.SQL, p.Args, nil }

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

// Codec returns a Frag rendering "CODEC(<stage0>, <stage1>, ...)" — a
// ClickHouse column compression codec clause. Each stage is a codec
// identifier: BareIdent("DoubleDelta") for a no-argument codec
// (Delta / DoubleDelta / GCD / Gorilla / FPC without an explicit level),
// or Call("ZSTD", InlineLit(1)) for a parameterized one — so
// Codec(BareIdent("DoubleDelta"), Call("ZSTD", InlineLit(1))) renders
// "CODEC(DoubleDelta, ZSTD(1))". Used by
// chsql.AlterTableModifyColumnCodec (cerberus issue #2768).
func Codec(stages ...Frag) Frag {
	return Call("CODEC", stages...)
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
//
// Window omits the frame clause, so CH applies its default: RANGE
// UNBOUNDED PRECEDING TO CURRENT ROW when ORDER BY is present (peer rows
// sharing an ORDER BY key see the same running value), or the whole
// partition when it isn't. Callers that need each row numbered
// individually — a strict per-row running frame even across equal ORDER
// BY keys — use WindowFrame with an explicit frame clause such as
// RowsUnboundedPrecedingToCurrentRow.
func Window(fn Frag, partitionBy []Frag, orderBy []OrderKey) Frag {
	return windowFrag(fn, partitionBy, orderBy, nil)
}

// WindowFrame is [Window] with an explicit trailing frame clause: "<fn>
// OVER (PARTITION BY ... ORDER BY ... <frame>)". A nil frame renders
// identically to Window (CH's default frame applies). Use a frame-clause
// constructor such as RowsUnboundedPrecedingToCurrentRow to override CH's
// default RANGE frame with a strict per-row ROWS frame.
func WindowFrame(fn Frag, partitionBy []Frag, orderBy []OrderKey, frame Frag) Frag {
	return windowFrag(fn, partitionBy, orderBy, frame)
}

func windowFrag(fn Frag, partitionBy []Frag, orderBy []OrderKey, frame Frag) Frag {
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
			first = false
		}
		if frame != nil {
			if !first {
				b.sb.WriteByte(' ')
			}
			frame(b)
		}
		b.sb.WriteByte(')')
	}
}

// RowsUnboundedPrecedingToCurrentRow returns a Frag rendering the window
// frame clause "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW": a
// strict per-row running frame that gives every row its own value even
// when it shares its ORDER BY key with a neighbour, unlike CH's default
// RANGE UNBOUNDED PRECEDING TO CURRENT ROW frame (which peer-groups rows
// with equal ORDER BY keys). Pass to WindowFrame wherever a windowed
// aggregate must number or accumulate strictly per row — e.g. a running
// rank over a key that repeats.
func RowsUnboundedPrecedingToCurrentRow() Frag {
	return func(b *Builder) {
		b.sb.WriteString("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	}
}

// RowsCurrentRowToUnboundedFollowing returns a Frag rendering the window
// frame clause "ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING" — the
// forward-looking sibling of RowsUnboundedPrecedingToCurrentRow.
//
// A lead-style function (leadInFrame) reads an offset AHEAD of the current
// row, and a window function can only see rows the FRAME itself admits: under
// RowsUnboundedPrecedingToCurrentRow the frame's own upper bound IS the
// current row, so any forward offset always falls outside it and the
// function returns its out-of-frame default on every row, never the actual
// next row (verified against chDB — every row read the DateTime64 zero
// default). RowsCurrentRowToUnboundedFollowing is the complementary frame
// that actually admits the rows ahead, exactly as
// RowsUnboundedPrecedingToCurrentRow admits the rows behind for lagInFrame.
func RowsCurrentRowToUnboundedFollowing() Frag {
	return func(b *Builder) {
		b.sb.WriteString("ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING")
	}
}

// Star returns a Frag rendering "*" — the unqualified wildcard for
// SELECT *. Use QualStar for the qualified "<table>.*" form.
func Star() Frag {
	return func(b *Builder) { b.sb.WriteByte('*') }
}

// StarReplace returns a Frag rendering ClickHouse's asterisk modifier
// "* REPLACE (<expr> AS <col>, …)" — the wildcard with named columns
// substituted by an expression while every other column, and crucially
// every column's NAME, passes through untouched. That name preservation
// is the point: it lets a rewrite reshape a column in place without any
// downstream reference to it having to change. An empty list renders the
// bare "*", so a caller need not guard the degenerate case.
func StarReplace(replacements []Frag) Frag {
	return func(b *Builder) {
		b.sb.WriteByte('*')
		if len(replacements) == 0 {
			return
		}
		b.sb.WriteString(" REPLACE (")
		for i, r := range replacements {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			r(b)
		}
		b.sb.WriteByte(')')
	}
}

// StarExcept returns a Frag rendering ClickHouse's asterisk modifier
// "<star> EXCEPT (<col>, …)" — the wildcard with the named columns
// dropped from its expansion while every other column, and every other
// column's name, passes through untouched. star is the wildcard the
// modifier applies to: [Star] for the bare "*", [QualStar] for the
// table-qualified form.
//
// It is the projection-side counterpart to a synthetic column. An emitter
// that must carry a marker through a subquery — a UNION-ALL arm tag, a
// windowed gate — projects it on the inside and strips it here, so the
// statement it hands back exposes exactly its input's column set and no
// downstream consumer has to learn about the marker. An empty column list
// renders the bare wildcard, so a caller need not guard that case.
//
// The " EXCEPT (" / ")" glue is emitter-chosen syntax with no operand of
// its own, so it rides verbatim; the column names flow through Col's
// backtick quoting.
func StarExcept(star Frag, cols ...string) Frag {
	return func(b *Builder) {
		star(b)
		if len(cols) == 0 {
			return
		}
		b.sb.WriteString(" EXCEPT (")
		for i, c := range cols {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			Col(c)(b)
		}
		b.sb.WriteByte(')')
	}
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

// InSubquery returns a Frag rendering "<left> IN <sub>" — the set-
// membership predicate where the right-hand side is a single subquery
// Frag that already carries its own surrounding parens (e.g. a
// traceScopeFrag's `(SELECT … )` or a QueryBuilder.Frag()). Unlike the
// list-form In (which wraps a comma list in parens), this emits no
// parens of its own, so a self-parenthesising subquery renders as the
// CH-idiomatic `<left> IN (SELECT …)` with exactly one paren pair.
// Sibling of NotInSubquery for the IN direction; used by the nested-set
// annotate anchor's trace-id scope filter.
func InSubquery(left, sub Frag) Frag {
	return func(b *Builder) {
		left(b)
		b.sb.WriteString(" IN ")
		sub(b)
	}
}

// NotInSubquery returns a Frag rendering "<left> NOT IN (<sub>)" — the
// anti-set membership predicate where the right-hand side is a single
// subquery rather than an element list. `sub` is rendered inside one
// pair of parens (typically a QueryBuilder.Frag() that already wraps
// itself, yielding the CH-idiomatic `NOT IN (SELECT …)`). The list-form
// `In` constructor parenthesises a comma list; this is its subquery
// sibling for the NOT-IN direction. Used by the range-mode
// absent_over_time anti-join.
func NotInSubquery(left, sub Frag) Frag {
	return func(b *Builder) {
		left(b)
		b.sb.WriteString(" NOT IN (")
		sub(b)
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

// cteClause is one entry in a QueryBuilder's WITH chain. Two shapes
// are supported:
//
//   - Recursive (Anchor + Recursive both set):
//     `WITH RECURSIVE <name> AS (<anchor> UNION ALL <recursive>)`.
//     Used by structural_join.go's >> / << closure emitter.
//
//   - Non-recursive (Body set, Anchor + Recursive nil):
//     `WITH <name> AS (<body>)` — a NAME for a relation, never a
//     materialisation of one: ClickHouse inlines the body at every
//     reference, so this shape deduplicates the emitted TEXT and
//     nothing else. It is the right tool when the duplication being
//     removed is textual (a subplan spliced into several places would
//     grow a chain's SQL exponentially) and the wrong one when the
//     duplication being removed is WORK — see the Scalar shape below,
//     and set_op.go for a rewrite that removes the extra references
//     instead of naming them. Body renders the (already-parenthesised)
//     subquery Frag.
//
//   - Scalar (Body set, Scalar true):
//     `WITH (<body>) AS <name>` — ClickHouse's scalar-CTE form, where
//     the alias binds a VALUE rather than a relation. This is the only
//     WITH shape ClickHouse evaluates exactly once no matter how many
//     times the alias is referenced: a non-recursive relational CTE is
//     INLINED at each reference, so N references cost N scans. Used by
//     the emit chokepoint to bind a structure-tab query's repeated
//     top-N trace-id set once (chplan.BindBoundedTraceScope). Note the
//     inverted token order — the parenthesised subquery comes FIRST and
//     the alias second, unlike the relational shapes above.
type cteClause struct {
	Name      string
	Anchor    *QueryBuilder
	Recursive *QueryBuilder
	Body      Frag
	Scalar    bool
}

// QueryBuilder accumulates a SELECT statement's parts. Slots are
// appended to in order; rendering walks each slot, emitting the
// canonical clause prefix (SELECT, FROM, WHERE, …) and joining
// per-slot Frags with the right separator.
//
// PREWHERE is a first-class slot, distinct from WHERE. ClickHouse
// evaluates PREWHERE before WHERE on the primary-key columns,
// pruning rows before the full row read; the chsql emitter's Filter(Scan)
// path promotes eligible predicates from WHERE → PREWHERE. Modelling
// PREWHERE separately here makes that partition a slot-level operation
// rather than a string rewrite on rendered SQL.
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
	arrayJoin  []Frag
	where      []Frag
	prewhere   []Frag
	groupBy    []Frag
	having     []Frag
	qualify    []Frag
	orderBy    []orderKey
	limit      int64
	hasLimit   bool
	limitBy    []Frag

	// attrStrategies is threaded into the Builder subquerySQL renders
	// against (see AttrStrategies). Set via WithAttrStrategies; the zero
	// value (nil) renders every attribute-map column as Map, exactly as
	// before cerberus issue #2777. chsql.emitSelect is the one production
	// setter — it copies the emitter's resolved strategies onto every
	// QueryBuilder it renders, so a plan assembled across many nested
	// QueryBuilders (one per chplan node) sees the same resolved
	// strategies throughout without each node's lowering/emit code having
	// to know about it.
	attrStrategies AttrStrategies
}

// WithAttrStrategies sets the AttrStrategies this QueryBuilder's Build /
// subquerySQL / Frag renders attribute-map accesses against. Build and
// subquerySQL construct a fresh Builder via
// NewBuilderWithAttrStrategies(s.attrStrategies); Frag writes directly into
// the CALLER's own *Builder instead, so it temporarily swaps that outer
// Builder's attrStrategies to s.attrStrategies for the duration of the
// write (restoring the previous value afterward) whenever s.attrStrategies
// is non-nil — a nil s.attrStrategies leaves the outer Builder's own
// strategies in effect unchanged, so every pre-#3063 QueryBuilder that
// never called WithAttrStrategies keeps rendering exactly as before. See
// AttrStrategies's doc for why this is scoped per signal rather than a
// single global map.
func (s *QueryBuilder) WithAttrStrategies(strategies AttrStrategies) *QueryBuilder {
	s.attrStrategies = strategies
	return s
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

// ArrayJoin appends one or more terms to the `ARRAY JOIN` clause, which
// CH renders after FROM (and any JOINs) and before PREWHERE / WHERE.
// Each term is a full Frag — typically `As(arrayExpr, alias)` — and
// multiple terms in a single ARRAY JOIN explode their arrays IN LOCKSTEP
// (the i-th element of every array lands on the same output row), which
// is exactly the parallel-explosion semantics the native
// timeSeriesRateToGrid emitter needs to pair each per-grid-point rate
// value with its parallel timeSeriesRange anchor timestamp. (Contrast
// the `arrayJoin(...)` scalar function, which cross-products independent
// arrays.)
//
// Multiple ArrayJoin calls chain into a single comma-separated clause.
func (s *QueryBuilder) ArrayJoin(terms ...Frag) *QueryBuilder {
	s.arrayJoin = append(s.arrayJoin, terms...)
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

// With registers a non-recursive `WITH <name> AS (<body>)` CTE in
// front of the SELECT. body is the (already-parenthesised) subquery
// Frag — its args land ahead of the outer SELECT's args, in emission
// order, because writeInto renders the CTE chain before the SELECT
// keyword.
//
// The name binds a RELATION, and ClickHouse inlines the body at every
// reference — N references cost N evaluations. So this shape buys
// emitted-TEXT linearity and never fewer reads: reach for it to stop a
// subplan spliced into several places from growing a chain's SQL
// exponentially, and reach for [QueryBuilder.WithScalar] (or a rewrite
// that leaves only one reference, as set_op.go's `&&` does) when the
// goal is to stop re-reading the relation.
//
// When a QueryBuilder mixes recursive and non-recursive CTEs the head
// renders as `WITH RECURSIVE` (CH accepts non-recursive entries under
// the RECURSIVE keyword); a chain of only non-recursive CTEs renders
// the bare `WITH`.
//
// Passing a nil body panics at render time.
func (s *QueryBuilder) With(name string, body Frag) *QueryBuilder {
	s.ctes = append(s.ctes, cteClause{Name: name, Body: body})
	return s
}

// WithScalar registers a `WITH (<body>) AS <name>` scalar CTE in front of
// the SELECT: the alias binds the VALUE the body's single-row, single-column
// SELECT produces, and ClickHouse evaluates that body EXACTLY ONCE however
// many times the alias is referenced — including from nested subqueries,
// since a WITH alias declared on the outermost statement is visible
// throughout it.
//
// That single-evaluation property is the whole reason this shape exists
// alongside [QueryBuilder.With]. A non-recursive relational CTE is inlined by
// ClickHouse at every reference, so `WITH x AS (SELECT …)` referenced N times
// reads N times — it dedupes the emitted TEXT, never the work. The scalar
// form dedupes the work.
//
// The natural payload is an array: `(SELECT groupArray(TraceId) FROM (<top-N
// traces>)) AS ids`, tested with `has(ids, TraceId)`. `TraceId IN ids` is NOT
// available — ClickHouse rejects a scalar array as an IN operand ("Function
// 'in' is supported only if second argument is constant or table
// expression") — while `has` against a bound array still drives the spans
// table's TraceId bloom-filter skip index, so granule pruning is preserved.
//
// body is a QueryBuilder (not a Frag) so this method owns the surrounding
// parens, matching WithRecursive's anchor/recursive children; passing a nil
// body panics at render time.
func (s *QueryBuilder) WithScalar(name string, body *QueryBuilder) *QueryBuilder {
	s.ctes = append(s.ctes, cteClause{Name: name, Body: Subquery(body), Scalar: true})
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

// Having appends a post-aggregation predicate (multiple conditions are
// AND-joined). HAVING filters on aggregate results — `max(TimeUnix) >= ?`
// over a `GROUP BY MetricName` — so unlike WHERE it can be served from an
// aggregating projection that pre-computes the aggregate, which a raw
// `WHERE TimeUnix >= ?` on the same column cannot.
func (s *QueryBuilder) Having(conds ...Frag) *QueryBuilder {
	s.having = append(s.having, conds...)
	return s
}

// Qualify appends a post-window predicate (multiple conditions are
// AND-joined). QUALIFY is to window functions what HAVING is to
// aggregates: it filters on the value of a `… OVER (…)` expression,
// which WHERE cannot do because window functions are evaluated after
// WHERE.
//
// Its value over the equivalent `SELECT *, <win> AS g FROM … WHERE g`
// rewrite is that the window column never enters the projection, so the
// result keeps the input's exact column set and the caller needs no
// `* EXCEPT (g)` to strip it back off. That is what lets the `&&`
// single-pass trace gate (set_op.go) keep `SELECT *` over the bare spans
// table — the shape whose column set every downstream consumer, and the
// spec harness's star expansion, already understand.
//
// Renders after HAVING and before ORDER BY, matching ClickHouse's clause
// order.
func (s *QueryBuilder) Qualify(conds ...Frag) *QueryBuilder {
	s.qualify = append(s.qualify, conds...)
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
//
// cerberus issue #3063 point 2: writeInto renders directly into the
// PARENT Frag's Builder (that is the whole point — positional args stay
// one shared slice), which means it inherits the parent's
// b.attrStrategies by default. A caller that resolved and set its OWN
// AttrStrategies on s via WithAttrStrategies (label_values.go's
// UNION-ALL arms, each built as an independent QueryBuilder before being
// spliced together via Frag) would otherwise have that assignment
// silently discarded — the ad-hoc-query-builder equivalent of the
// "bypasses chplan entirely" class of bug this issue names, except here
// the bypass is inside chsql's own Frag composition primitive rather
// than in a caller. s.attrStrategies is nil (the zero value) for every
// pre-#3063 QueryBuilder that never called WithAttrStrategies, so this
// swap is a genuine no-op for every one of chsql's ~50+ other Frag()
// call sites: they keep inheriting the parent's strategies exactly as
// before.
func (s *QueryBuilder) Frag() Frag {
	return func(b *Builder) {
		b.sb.WriteByte('(')
		if s.attrStrategies != nil {
			prev := b.attrStrategies
			b.attrStrategies = s.attrStrategies
			s.writeInto(b)
			b.attrStrategies = prev
		} else {
			s.writeInto(b)
		}
		b.sb.WriteByte(')')
	}
}

// Build renders the SELECT statement to (sql, args). Equivalent to
// running Frag() into a fresh Builder, minus the surrounding parens.
func (s *QueryBuilder) Build() (string, []any) {
	sql, args, _ := s.subquerySQL()
	return sql, args
}

// subquerySQL is Build's error-propagating counterpart (see
// Subqueryable). It is what the recursive emitter's emitSelect/splice
// and the Subquery/Spliced Frag helpers call instead of Build, so a
// nested QueryBuilder's Builder.err first-error-wins state (#1449)
// reaches the outer render rather than being silently dropped the way
// the old pre-flight-then-discard-and-render-again pattern needed to
// work around. Unexported: Build stays the public two-value surface
// every non-chsql caller already depends on.
func (s *QueryBuilder) subquerySQL() (string, []any, error) {
	b := NewBuilderWithAttrStrategies(s.attrStrategies)
	s.writeInto(b)
	return b.Build()
}

// writeCTEs renders the `WITH [RECURSIVE] <name> AS (...)` head ahead
// of the SELECT keyword. Split out of writeInto to keep the latter's
// cognitive complexity bounded. The head is `WITH RECURSIVE` when any
// entry is recursive (Anchor + Recursive set); a chain of only
// non-recursive `With` CTEs renders the bare `WITH`. CH accepts
// non-recursive entries under the RECURSIVE keyword, so the mixed case
// stays valid.
func (s *QueryBuilder) writeCTEs(b *Builder) {
	if len(s.ctes) == 0 {
		return
	}
	recursive := false
	for _, c := range s.ctes {
		if c.Anchor != nil || c.Recursive != nil {
			recursive = true
			break
		}
	}
	if recursive {
		b.sb.WriteString("WITH RECURSIVE ")
	} else {
		b.sb.WriteString("WITH ")
	}
	for i, c := range s.ctes {
		if i > 0 {
			b.sb.WriteString(", ")
		}
		// CTE names render bare — CH accepts unquoted identifiers for
		// CTE aliases, and the existing structural_join fixture pins
		// `_struct_closure` (no backticks). The caller is responsible
		// for passing a CH-identifier-safe token.
		//
		// The scalar shape inverts the token order: ClickHouse spells a
		// value-valued CTE `(<subquery>) AS <name>`, not
		// `<name> AS (<subquery>)`.
		if c.Scalar {
			c.writeBody(b)
			b.sb.WriteString(" AS ")
			b.sb.WriteString(c.Name)
			continue
		}
		b.sb.WriteString(c.Name)
		b.sb.WriteString(" AS ")
		c.writeBody(b)
	}
	b.sb.WriteByte(' ')
}

// writeBody renders a single CTE entry's body: the non-recursive
// `(<subquery>)` Body Frag (which carries its own surrounding parens),
// or the recursive `(<anchor> UNION ALL <recursive>)` shape.
func (c cteClause) writeBody(b *Builder) {
	switch {
	case c.Body != nil:
		// Non-recursive CTE: the Body Frag already renders its own
		// surrounding parens (it's an arm subquery Frag), so emit it
		// verbatim — `<name> AS (<subquery>)`.
		c.Body(b)
	case c.Anchor != nil && c.Recursive != nil:
		b.sb.WriteByte('(')
		c.Anchor.writeInto(b)
		b.sb.WriteString(" UNION ALL ")
		c.Recursive.writeInto(b)
		b.sb.WriteByte(')')
	default:
		// No Body means the entry was registered via WithRecursive;
		// both Anchor and Recursive must be set.
		panic("chsql: WithRecursive requires non-nil anchor and recursive")
	}
}

func (s *QueryBuilder) writeInto(b *Builder) {
	s.writeCTEs(b)
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
	if len(s.arrayJoin) > 0 {
		b.sb.WriteString(" ARRAY JOIN ")
		for i, f := range s.arrayJoin {
			if i > 0 {
				b.sb.WriteString(", ")
			}
			f(b)
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
	if len(s.having) > 0 {
		b.sb.WriteString(" HAVING ")
		And(s.having...)(b)
	}
	if len(s.qualify) > 0 {
		b.sb.WriteString(" QUALIFY ")
		And(s.qualify...)(b)
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
