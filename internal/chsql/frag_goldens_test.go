package chsql

import (
	"reflect"
	"strings"
	"testing"
)

// TestFrag_Goldens is the unified golden table for every public Frag
// constructor in builder.go. The existing tests in builder_test.go cover
// individual Frags at a per-constructor granularity; this table-driven
// aggregator pins one canonical shape per Frag so a regression in any
// single constructor surfaces in one place rather than scattered across
// dozens of bespoke tests.
//
// Each entry builds the Frag, runs it through a fresh Builder, and
// asserts both the rendered SQL string and the bound args slice.
// Constructors that panic on bad input have their own dedicated tests
// in builder_test.go — this table covers the happy path only.
func TestFrag_Goldens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		frag     Frag
		wantSQL  string
		wantArgs []any
	}{
		// ── Basic identifier / literal Frags ──────────────────────────
		{name: "Col", frag: Col("Value"), wantSQL: "`Value`"},
		{name: "Col_embedded_backtick", frag: Col("weird`name"), wantSQL: "`weird``name`"},
		{name: "Qual", frag: Qual("L", "Value"), wantSQL: "`L`.`Value`"},
		{name: "Lit_string", frag: Lit("api"), wantSQL: "?", wantArgs: []any{"api"}},
		{name: "Lit_int", frag: Lit(42), wantSQL: "?", wantArgs: []any{42}},
		{name: "Lit_float", frag: Lit(0.5), wantSQL: "?", wantArgs: []any{0.5}},
		{name: "BareIdent", frag: BareIdent("k"), wantSQL: "k"},
		{name: "BareIdent_dotted", frag: BareIdent("c._depth"), wantSQL: "c._depth"},
		{name: "InlineLit_int", frag: InlineLit(7), wantSQL: "7"},
		{name: "InlineLit_int64", frag: InlineLit(int64(9)), wantSQL: "9"},
		{name: "InlineLit_float", frag: InlineLit(3.14), wantSQL: "3.14"},
		{name: "InlineLit_string", frag: InlineLit("foo"), wantSQL: "'foo'"},
		{name: "InlineLit_string_escapes", frag: InlineLit(`a'b\c`), wantSQL: `'a\'b\\c'`},
		{name: "Star", frag: Star(), wantSQL: "*"},
		{name: "QualStar", frag: QualStar("L"), wantSQL: "`L`.*"},
		{name: "Distinct", frag: Distinct(Col("Value")), wantSQL: "DISTINCT `Value`"},

		// ── Comparison binary operators ───────────────────────────────
		{
			name:     "Eq",
			frag:     Eq(Col("MetricName"), Lit("http_requests_total")),
			wantSQL:  "`MetricName` = ?",
			wantArgs: []any{"http_requests_total"},
		},
		{
			name:     "Neq",
			frag:     Neq(Col("MetricName"), Lit("up")),
			wantSQL:  "`MetricName` != ?",
			wantArgs: []any{"up"},
		},
		{
			name:     "Lt",
			frag:     Lt(Col("Value"), Lit(0.5)),
			wantSQL:  "`Value` < ?",
			wantArgs: []any{0.5},
		},
		{
			name:     "Lte",
			frag:     Lte(Col("Value"), Lit(1.0)),
			wantSQL:  "`Value` <= ?",
			wantArgs: []any{1.0},
		},
		{
			name:     "Gt",
			frag:     Gt(Col("Value"), Lit(0.0)),
			wantSQL:  "`Value` > ?",
			wantArgs: []any{0.0},
		},
		{
			name:     "Gte",
			frag:     Gte(Col("Value"), Lit(0.1)),
			wantSQL:  "`Value` >= ?",
			wantArgs: []any{0.1},
		},
		{
			name:     "Like",
			frag:     Like(Col("Body"), Lit("%err%")),
			wantSQL:  "`Body` LIKE ?",
			wantArgs: []any{"%err%"},
		},
		{
			name:     "NotLike",
			frag:     NotLike(Col("Body"), Lit("%ok%")),
			wantSQL:  "`Body` NOT LIKE ?",
			wantArgs: []any{"%ok%"},
		},

		// ── Logical combinators ───────────────────────────────────────
		{
			name:     "And_two",
			frag:     And(Eq(Col("a"), Lit(1)), Eq(Col("b"), Lit(2))),
			wantSQL:  "`a` = ? AND `b` = ?",
			wantArgs: []any{1, 2},
		},
		{
			name:     "And_three",
			frag:     And(Eq(Col("a"), Lit(1)), Eq(Col("b"), Lit(2)), Eq(Col("c"), Lit(3))),
			wantSQL:  "`a` = ? AND `b` = ? AND `c` = ?",
			wantArgs: []any{1, 2, 3},
		},
		{
			name:     "Or_two",
			frag:     Or(Eq(Col("a"), Lit(1)), Eq(Col("b"), Lit(2))),
			wantSQL:  "`a` = ? OR `b` = ?",
			wantArgs: []any{1, 2},
		},
		{
			name:    "Not",
			frag:    Not(Col("active")),
			wantSQL: "NOT `active`",
		},
		{
			name:    "IsNull",
			frag:    IsNull(Col("Value")),
			wantSQL: "`Value` IS NULL",
		},
		{
			name:    "IsNotNull",
			frag:    IsNotNull(Col("Value")),
			wantSQL: "`Value` IS NOT NULL",
		},

		// ── Arithmetic operators ──────────────────────────────────────
		{
			name:    "Add",
			frag:    Add(Col("x"), Col("y")),
			wantSQL: "`x` + `y`",
		},
		{
			name:    "Sub",
			frag:    Sub(Col("x"), Col("y")),
			wantSQL: "`x` - `y`",
		},
		{
			name:    "Mul",
			frag:    Mul(Col("x"), Col("y")),
			wantSQL: "`x` * `y`",
		},
		{
			name:    "Div",
			frag:    Div(Col("x"), Col("y")),
			wantSQL: "`x` / `y`",
		},
		{
			name:    "Mod",
			frag:    Mod(Col("x"), Col("y")),
			wantSQL: "`x` % `y`",
		},
		{
			name:    "Neg",
			frag:    Neg(Col("Value")),
			wantSQL: "-`Value`",
		},
		{
			name:    "Paren_wraps_no_inner_ws",
			frag:    Paren(Add(Col("x"), Col("y"))),
			wantSQL: "(`x` + `y`)",
		},

		// ── Set membership / type / range ─────────────────────────────
		{
			name:     "In_single",
			frag:     In(Col("Status"), Lit("ok")),
			wantSQL:  "`Status` IN (?)",
			wantArgs: []any{"ok"},
		},
		{
			name:     "In_multiple",
			frag:     In(Col("Status"), Lit("ok"), Lit("warn"), Lit("err")),
			wantSQL:  "`Status` IN (?, ?, ?)",
			wantArgs: []any{"ok", "warn", "err"},
		},
		{
			name:     "Between",
			frag:     Between(Col("Value"), Lit(0.0), Lit(1.0)),
			wantSQL:  "`Value` BETWEEN ? AND ?",
			wantArgs: []any{0.0, 1.0},
		},
		{
			name:    "Cast_basic",
			frag:    Cast(Col("Value"), "Float64"),
			wantSQL: "CAST(`Value` AS Float64)",
		},
		{
			name:     "Cast_with_lit",
			frag:     Cast(Lit("3.14"), "Float64"),
			wantSQL:  "CAST(? AS Float64)",
			wantArgs: []any{"3.14"},
		},

		// ── Calls / arrays / tuples / subscripts ──────────────────────
		{name: "Call_nullary", frag: Call("now"), wantSQL: "now()"},
		{
			name:    "Call_single_arg",
			frag:    Call("length", Col("Body")),
			wantSQL: "length(`Body`)",
		},
		{
			name:     "Call_multi_arg",
			frag:     Call("concat", Lit("a"), Lit("b"), Lit("c")),
			wantSQL:  "concat(?, ?, ?)",
			wantArgs: []any{"a", "b", "c"},
		},
		{
			name:     "Parametric_one_param_one_arg",
			frag:     Parametric("quantile", []Frag{Lit(0.5)}, Col("Value")),
			wantSQL:  "quantile(?)(`Value`)",
			wantArgs: []any{0.5},
		},
		{
			name:     "Parametric_multi_param_multi_arg",
			frag:     Parametric("quantilesExact", []Frag{Lit(0.5), Lit(0.9)}, Col("a"), Col("b")),
			wantSQL:  "quantilesExact(?, ?)(`a`, `b`)",
			wantArgs: []any{0.5, 0.9},
		},
		{
			name:    "Array_empty",
			frag:    Array(),
			wantSQL: "[]",
		},
		{
			name:    "Array_inlines",
			frag:    Array(InlineLit(int64(0)), InlineLit(int64(1)), InlineLit(int64(2))),
			wantSQL: "[0, 1, 2]",
		},
		{
			name:     "Array_with_lits",
			frag:     Array(Lit("a"), Lit("b")),
			wantSQL:  "[?, ?]",
			wantArgs: []any{"a", "b"},
		},
		{
			name:    "Tuple_two",
			frag:    Tuple(Col("a"), Col("b")),
			wantSQL: "(`a`, `b`)",
		},
		{
			name:     "Subscript_with_arg",
			frag:     Subscript(Col("Attributes"), Lit("service.name")),
			wantSQL:  "`Attributes`[?]",
			wantArgs: []any{"service.name"},
		},

		// ── Higher-order: If / Lambda / As ────────────────────────────
		{
			name:    "If",
			frag:    If(Eq(Col("a"), Col("b")), Col("a"), Col("b")),
			wantSQL: "if(`a` = `b`, `a`, `b`)",
		},
		{
			name:    "Lambda1",
			frag:    Lambda1("x", Add(BareIdent("x"), InlineLit(int64(1)))),
			wantSQL: "x -> x + 1",
		},
		{
			name:    "Lambda2",
			frag:    Lambda2("k", "v", Eq(BareIdent("k"), Lit("job"))),
			wantSQL: "(k, v) -> k = ?", wantArgs: []any{"job"},
		},
		{
			name:    "As_aliased",
			frag:    As(Col("Value"), "v"),
			wantSQL: "`Value` AS `v`",
		},
		{
			name:    "As_empty_alias_passthrough",
			frag:    As(Col("Value"), ""),
			wantSQL: "`Value`",
		},

		// ── Set ops at Frag level ─────────────────────────────────────
		{
			name:    "UnionAll_two",
			frag:    UnionAll(NewQuery().From(Col("a")).Frag(), NewQuery().From(Col("b")).Frag()),
			wantSQL: "(SELECT * FROM `a`) UNION ALL (SELECT * FROM `b`)",
		},
		{
			name:    "UnionDistinct_two",
			frag:    UnionDistinct(NewQuery().From(Col("a")).Frag(), NewQuery().From(Col("b")).Frag()),
			wantSQL: "(SELECT * FROM `a`) UNION DISTINCT (SELECT * FROM `b`)",
		},
		{
			name:    "UnionAll_single_no_keyword",
			frag:    UnionAll(NewQuery().From(Col("a")).Frag()),
			wantSQL: "(SELECT * FROM `a`)",
		},

		// ── Subquery wrapping ─────────────────────────────────────────
		{
			name:     "Subquery_QueryBuilder_inlines_parens_and_args",
			frag:     Subquery(NewQuery().Select(Col("Value")).From(Col("t")).Where(Eq(Col("x"), Lit(1)))),
			wantSQL:  "(SELECT `Value` FROM `t` WHERE `x` = ?)",
			wantArgs: []any{1},
		},
		{
			name:     "Subquery_PreRenderedSQL",
			frag:     Subquery(PreRenderedSQL{SQL: "SELECT 1", Args: []any{}}),
			wantSQL:  "(SELECT 1)",
			wantArgs: nil,
		},
		{
			name:     "Subquery_PreRenderedSQL_with_args",
			frag:     Subquery(PreRenderedSQL{SQL: "SELECT ?", Args: []any{42}}),
			wantSQL:  "(SELECT ?)",
			wantArgs: []any{42},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewBuilder()
			tc.frag(b)
			gotSQL, gotArgs := b.Build()
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL = %q; want %q", gotSQL, tc.wantSQL)
			}
			if tc.wantArgs == nil {
				if len(gotArgs) != 0 {
					t.Errorf("Args = %v; want nil/empty", gotArgs)
				}
				return
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Errorf("Args = %v; want %v", gotArgs, tc.wantArgs)
			}
		})
	}
}

// TestFrag_NestedComposition pins a deeper expression tree to ensure
// that the Frag combinators compose lossless-ly: a nested `And(Or(Eq,
// Neq), Not(In(...)))` shape renders top-down with correct precedence
// (no auto-parenthesisation; callers wrap with Paren) and binds args in
// emission order.
func TestFrag_NestedComposition(t *testing.T) {
	t.Parallel()

	expr := And(
		Paren(Or(Eq(Col("a"), Lit(1)), Neq(Col("b"), Lit(2)))),
		Not(In(Col("c"), Lit("x"), Lit("y"))),
	)
	b := NewBuilder()
	expr(b)
	sql, args := b.Build()
	want := "(`a` = ? OR `b` != ?) AND NOT `c` IN (?, ?)"
	if sql != want {
		t.Errorf("SQL = %q; want %q", sql, want)
	}
	if w := []any{1, 2, "x", "y"}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestFrag_ArgOrderingAcrossSlots — once a Frag is plugged into a
// QueryBuilder, the bound args interleave across slots in the SQL's
// `?` order: SELECT args, then FROM (subquery) args, then JOIN ON
// args, then PREWHERE, then WHERE, then HAVING-less, then GROUP BY,
// then ORDER BY. This is the contract that nested subqueries depend
// on; it deserves a dedicated assertion.
func TestFrag_ArgOrderingAcrossSlots(t *testing.T) {
	t.Parallel()

	inner := NewQuery().
		Select(Col("MetricName"), Col("Value")).
		From(Col("otel_metrics_gauge")).
		Where(Eq(Col("MetricName"), Lit("inner_metric")))

	sql, args := NewQuery().
		Select(As(Lit("outer_proj_arg"), "p")).
		From(inner.Frag()).
		Where(Eq(Col("Value"), Lit(0.5))).
		OrderBy(Col("Value"), true).
		Limit(10).
		Build()

	wantSQL := "SELECT ? AS `p` FROM (" +
		"SELECT `MetricName`, `Value` FROM `otel_metrics_gauge`" +
		" WHERE `MetricName` = ?" +
		") WHERE `Value` = ? ORDER BY `Value` DESC LIMIT 10"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	// SELECT alias arg, inner WHERE arg, outer WHERE arg — in that
	// exact textual order.
	if w := []any{"outer_proj_arg", "inner_metric", 0.5}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestFrag_InlineLit_StringEscapesPreserveBytes pins the byte-level
// escape policy: single-quote and backslash both get a backslash prefix,
// every other byte (including unicode) passes through verbatim. CH
// accepts the resulting string literal byte-for-byte.
func TestFrag_InlineLit_StringEscapesPreserveBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{`hello`, `'hello'`},
		{`it's`, `'it\'s'`},
		{`a\b`, `'a\\b'`},
		{`mixed'\path`, `'mixed\'\\path'`},
		// Non-ASCII passes through (UTF-8 bytes don't include `'` or `\`).
		{`café`, `'café'`},
		{"", `''`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			b := NewBuilder()
			InlineLit(tc.in)(b)
			if got := b.String(); got != tc.want {
				t.Errorf("InlineLit(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFrag_InlineLit_PanicMessage — the panic message includes the
// rejected Go type so a wrong callsite is easy to locate. A []byte is a
// genuinely unsupported type (int / int64 / float64 / bool / string are the
// supported set), so it stands in as the rejected example.
func TestFrag_InlineLit_PanicMessage(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("InlineLit([]byte) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "InlineLit unsupported type") {
			t.Errorf("panic value = %v; want message mentioning 'InlineLit unsupported type'", r)
		}
		if !strings.Contains(msg, "uint8") {
			t.Errorf("panic value = %v; want message mentioning the rejected type", r)
		}
	}()
	// InlineLit returns a closure; the panic fires only when the closure
	// runs against a Builder. Invoke it here to surface the panic.
	InlineLit([]byte("x"))(NewBuilder())
}

// TestFrag_InlineLit_Bool pins that a bool renders as the bare CH keyword
// (true / false), the form a boolean MergeTree SETTINGS value takes.
func TestFrag_InlineLit_Bool(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   bool
		want string
	}{
		{true, "true"},
		{false, "false"},
	} {
		b := NewBuilder()
		InlineLit(tc.in)(b)
		if got := b.String(); got != tc.want {
			t.Errorf("InlineLit(%v) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestFrag_Array_BindsAtPosition — Array elements emit their args
// in element order, then any subsequent Frag's args land after. This is
// the array literal companion to TestFrag_HelpersBindAtPosition.
func TestFrag_Array_BindsAtPosition(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Array(Lit(1), Lit(2), Lit(3))(b)
	b.writeSQL(", ")
	Lit(4)(b)
	sql, args := b.Build()
	if want := "[?, ?, ?], ?"; sql != want {
		t.Errorf("SQL = %q; want %q", sql, want)
	}
	if w := []any{1, 2, 3, 4}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestFrag_Call_BindsAtPosition — Call args emit their bindings in
// argument order; an outer expression composing Call(…) with another
// arg-bearing Frag interleaves correctly.
func TestFrag_Call_BindsAtPosition(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	// concat(?, ?) , ?
	Call("concat", Lit("a"), Lit("b"))(b)
	b.writeSQL(", ")
	Lit("c")(b)
	sql, args := b.Build()
	if want := "concat(?, ?), ?"; sql != want {
		t.Errorf("SQL = %q; want %q", sql, want)
	}
	if w := []any{"a", "b", "c"}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestFrag_PrecedenceContract — the Frag surface deliberately does not
// auto-parenthesise. `And(a, Or(b, c))` therefore renders as
// `a AND b OR c`; the caller wraps the Or in Paren if SQL precedence
// requires it. Pin that contract so future "helpful" auto-paren changes
// surface as a test break.
func TestFrag_PrecedenceContract(t *testing.T) {
	t.Parallel()

	noParen := And(Eq(Col("a"), Lit(1)), Or(Eq(Col("b"), Lit(2)), Eq(Col("c"), Lit(3))))
	b1 := NewBuilder()
	noParen(b1)
	if got, want := b1.String(), "`a` = ? AND `b` = ? OR `c` = ?"; got != want {
		t.Errorf("no-paren = %q; want %q", got, want)
	}

	withParen := And(Eq(Col("a"), Lit(1)), Paren(Or(Eq(Col("b"), Lit(2)), Eq(Col("c"), Lit(3)))))
	b2 := NewBuilder()
	withParen(b2)
	if got, want := b2.String(), "`a` = ? AND (`b` = ? OR `c` = ?)"; got != want {
		t.Errorf("with-paren = %q; want %q", got, want)
	}
}
