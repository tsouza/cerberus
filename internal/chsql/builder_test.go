package chsql_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestBuilder_Empty — the zero-value Builder renders empty SQL and a
// nil args slice. Confirms NewBuilder is unnecessary; the zero value
// is usable.
func TestBuilder_Empty(t *testing.T) {
	t.Parallel()

	var b chsql.Builder
	sql, args := b.Build()
	if sql != "" {
		t.Errorf("empty Builder produced SQL %q; want empty", sql)
	}
	if args != nil {
		t.Errorf("empty Builder produced args %v; want nil", args)
	}
}

// TestBuilder_Ident — backtick quoting with embedded-backtick doubling.
// Mirrors the existing emit_node.go behaviour so the future R6.2 port
// is a one-for-one swap.
func TestBuilder_Ident(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, in, want string
	}{
		{"plain", "Attributes", "`Attributes`"},
		{"with_backtick", "weird`name", "`weird``name`"},
		{"empty", "", "``"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := chsql.NewBuilder()
			b.Ident(tc.in)
			if got := b.String(); got != tc.want {
				t.Errorf("Ident(%q) = %q; want %q", tc.in, got, tc.want)
			}
			if args := b.Args(); args != nil {
				t.Errorf("Ident bound args %v; want none", args)
			}
		})
	}
}

// TestBuilder_QualIdent — "qual"."name" with both parts backtick-quoted.
func TestBuilder_QualIdent(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.QualIdent("L", "Value")
	if got, want := b.String(), "`L`.`Value`"; got != want {
		t.Errorf("QualIdent = %q; want %q", got, want)
	}
}

// TestBuilder_Arg — `?` placeholder appends a positional arg.
func TestBuilder_Arg(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.Arg("hello")
	b.WriteSQL(", ")
	b.Arg(42)
	gotSQL, gotArgs := b.Build()
	if want := "?, ?"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if want := []any{"hello", 42}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestBuilder_MapAt — Attributes['?'] form, key bound as positional arg.
func TestBuilder_MapAt(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.MapAt("Attributes", "service.name")
	gotSQL, gotArgs := b.Build()
	if want := "`Attributes`[?]"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if want := []any{"service.name"}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestBuilder_MapKeys — mapKeys(`Attributes`).
func TestBuilder_MapKeys(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.MapKeys("Attributes")
	gotSQL, gotArgs := b.Build()
	if want := "mapKeys(`Attributes`)"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if gotArgs != nil {
		t.Errorf("Args = %v; want nil", gotArgs)
	}
}

// TestBuilder_MapFilterExcept — mapFilter NOT IN form, all keys bound.
func TestBuilder_MapFilterExcept(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.MapFilterExcept("Attributes", "instance", "job")
	gotSQL, gotArgs := b.Build()
	want := "mapFilter((k, v) -> NOT (k IN (?, ?)), `Attributes`)"
	if gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if w := []any{"instance", "job"}; !reflect.DeepEqual(gotArgs, w) {
		t.Errorf("Args = %v; want %v", gotArgs, w)
	}
}

// TestBuilder_MapFilterExceptPanicsOnEmpty — empty keys is a programmer
// error: the resulting CH SQL would never filter anything, which is
// never the caller's intent.
func TestBuilder_MapFilterExceptPanicsOnEmpty(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MapFilterExcept with no keys did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "MapFilterExcept") {
			t.Errorf("panic value = %v; want message mentioning MapFilterExcept", r)
		}
	}()
	b := chsql.NewBuilder()
	b.MapFilterExcept("Attributes")
}

// TestBuilder_Now64 — bare CH builtin, no args.
func TestBuilder_Now64(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.Now64()
	if got, want := b.String(), "now64(9)"; got != want {
		t.Errorf("Now64 = %q; want %q", got, want)
	}
}

// TestBuilder_SubtractNanos — wraps (<lhs> - toIntervalNanosecond(<ns>))
// with the lhs callback running at the right position so its args
// land before the ns literal.
func TestBuilder_SubtractNanos(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.SubtractNanos(func(b *chsql.Builder) { b.Now64() }, int64(5*time.Minute))
	if got, want := b.String(), "(now64(9) - toIntervalNanosecond(300000000000))"; got != want {
		t.Errorf("SubtractNanos = %q; want %q", got, want)
	}
}

// TestBuilder_SubtractNanos_PreservesArgOrder — args bound inside lhs
// appear in the args slice at the position they were emitted (i.e.
// before the literal ns).
func TestBuilder_SubtractNanos_PreservesArgOrder(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.SubtractNanos(func(b *chsql.Builder) {
		b.WriteSQL("max(")
		b.MapAt("Attributes", "service.name")
		b.WriteSQL(")")
	}, 1000)
	gotSQL, gotArgs := b.Build()
	wantSQL := "(max(`Attributes`[?]) - toIntervalNanosecond(1000))"
	if gotSQL != wantSQL {
		t.Errorf("SQL = %q; want %q", gotSQL, wantSQL)
	}
	if want := []any{"service.name"}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestBuilder_DateTime64Lit — fixed-format CH DateTime64(9) literal,
// nanosecond precision, UTC.
func TestBuilder_DateTime64Lit(t *testing.T) {
	t.Parallel()

	// 2026-05-13 06:07:08.123456789 UTC.
	tm := time.Date(2026, 5, 13, 6, 7, 8, 123456789, time.UTC)
	b := chsql.NewBuilder()
	b.DateTime64Lit(tm)
	want := "toDateTime64('2026-05-13 06:07:08.123456789', 9)"
	if got := b.String(); got != want {
		t.Errorf("DateTime64Lit = %q; want %q", got, want)
	}
}

// TestBuilder_DateTime64Lit_NormalisesToUTC — non-UTC time is rendered
// in UTC so fixtures are reproducible across local timezones.
func TestBuilder_DateTime64Lit_NormalisesToUTC(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-05-13 00:00:00 in America/Sao_Paulo == 2026-05-13 03:00:00 UTC.
	tm := time.Date(2026, 5, 13, 0, 0, 0, 0, loc)
	b := chsql.NewBuilder()
	b.DateTime64Lit(tm)
	want := "toDateTime64('2026-05-13 03:00:00.000000000', 9)"
	if got := b.String(); got != want {
		t.Errorf("DateTime64Lit = %q; want %q", got, want)
	}
}

// TestBuilder_Lambda — "(k, v) -> NOT (k IN (?))" with the body
// callback running at the lambda-body position.
func TestBuilder_Lambda(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.Lambda([]string{"k", "v"}, func(b *chsql.Builder) {
		b.WriteSQL("k = ")
		b.Arg("env")
	})
	gotSQL, gotArgs := b.Build()
	if want := "(k, v) -> k = ?"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if want := []any{"env"}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestBuilder_ParamAgg_Parameterised — quantile(0.95)(value) style,
// with both params and args via callbacks.
func TestBuilder_ParamAgg_Parameterised(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.ParamAgg(
		"quantile",
		[]func(b *chsql.Builder){
			func(b *chsql.Builder) { b.Arg(0.95) },
		},
		[]func(b *chsql.Builder){
			func(b *chsql.Builder) { b.Ident("Value") },
		},
	)
	gotSQL, gotArgs := b.Build()
	if want := "quantile(?)(`Value`)"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if want := []any{0.95}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestBuilder_ParamAgg_NoParams — non-parameterised form drops the
// leading params parens, matching CH's "sum(value)" shape.
func TestBuilder_ParamAgg_NoParams(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.ParamAgg(
		"sum",
		nil,
		[]func(b *chsql.Builder){
			func(b *chsql.Builder) { b.Ident("Value") },
		},
	)
	if got, want := b.String(), "sum(`Value`)"; got != want {
		t.Errorf("ParamAgg(no params) = %q; want %q", got, want)
	}
}

// TestBuilder_ParamAgg_MultiParam — quantiles(0.5, 0.9)(value) style.
func TestBuilder_ParamAgg_MultiParam(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	b.ParamAgg(
		"quantiles",
		[]func(b *chsql.Builder){
			func(b *chsql.Builder) { b.Arg(0.5) },
			func(b *chsql.Builder) { b.Arg(0.9) },
		},
		[]func(b *chsql.Builder){
			func(b *chsql.Builder) { b.Ident("Value") },
		},
	)
	gotSQL, gotArgs := b.Build()
	if want := "quantiles(?, ?)(`Value`)"; gotSQL != want {
		t.Errorf("SQL = %q; want %q", gotSQL, want)
	}
	if want := []any{0.5, 0.9}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestFrag_HelpersBindAtPosition — Col / Lit / Raw / Qual produce
// Frags that can be plugged into a SelectBuilder slot; args bind in
// emission order.
func TestFrag_HelpersBindAtPosition(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	chsql.Col("Value")(b)
	b.WriteSQL(", ")
	chsql.Lit(42)(b)
	b.WriteSQL(", ")
	chsql.Raw("now64(9)")(b)
	b.WriteSQL(", ")
	chsql.Qual("L", "TimeUnix")(b)
	gotSQL, gotArgs := b.Build()
	wantSQL := "`Value`, ?, now64(9), `L`.`TimeUnix`"
	if gotSQL != wantSQL {
		t.Errorf("SQL = %q; want %q", gotSQL, wantSQL)
	}
	if want := []any{42}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("Args = %v; want %v", gotArgs, want)
	}
}

// TestAs_WrapsWithAlias — As(expr, alias) emits "<expr> AS <alias>"
// with the alias backtick-quoted; an empty alias passes the inner
// Frag through unchanged.
func TestAs_WrapsWithAlias(t *testing.T) {
	t.Parallel()

	b := chsql.NewBuilder()
	chsql.As(chsql.Col("Value"), "v")(b)
	if got, want := b.String(), "`Value` AS `v`"; got != want {
		t.Errorf("As(Col,v) = %q; want %q", got, want)
	}

	b2 := chsql.NewBuilder()
	chsql.As(chsql.Col("Value"), "")(b2)
	if got, want := b2.String(), "`Value`"; got != want {
		t.Errorf("As(Col,\"\") = %q; want %q", got, want)
	}
}

// TestSelectBuilder_SelectAs — SelectAs slot adds "<expr> AS <alias>"
// to the SELECT list without composing the AS keyword by hand at the
// call site.
func TestSelectBuilder_SelectAs(t *testing.T) {
	t.Parallel()

	sql, _ := chsql.NewSelect().
		SelectAs(chsql.Col("MetricName"), "name").
		SelectAs(chsql.Col("Value"), "").
		From(chsql.Col("otel_metrics_gauge")).
		Build()
	want := "SELECT `MetricName` AS `name`, `Value` FROM `otel_metrics_gauge`"
	if sql != want {
		t.Errorf("SelectAs = %q; want %q", sql, want)
	}
}

// TestSelectBuilder_Empty — empty SelectBuilder renders "SELECT *".
// (No FROM is fine; CH accepts SELECT * alone for fixture-style
// shapes, even if it's not what production emits.)
func TestSelectBuilder_Empty(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewSelect().Build()
	if want := "SELECT *"; sql != want {
		t.Errorf("empty SELECT = %q; want %q", sql, want)
	}
	if args != nil {
		t.Errorf("empty SELECT args = %v; want nil", args)
	}
}

// TestSelectBuilder_Basic — Select / From / Where / Limit composed
// in order, args from a Lit() in Where bind at the WHERE position.
func TestSelectBuilder_Basic(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewSelect().
		Select(chsql.Col("MetricName"), chsql.Col("Value")).
		From(chsql.Col("otel_metrics_gauge")).
		Where(func(b *chsql.Builder) {
			b.Ident("MetricName")
			b.WriteSQL(" = ")
			b.Arg("http_requests_total")
		}).
		Limit(100).
		Build()

	wantSQL := "SELECT `MetricName`, `Value` FROM `otel_metrics_gauge`" +
		" WHERE `MetricName` = ? LIMIT 100"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	if want := []any{"http_requests_total"}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestSelectBuilder_Prewhere — PREWHERE is emitted before WHERE in
// the rendered SQL; multiple predicates in either slot join with AND.
func TestSelectBuilder_Prewhere(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewSelect().
		Select(chsql.Col("Value")).
		From(chsql.Col("otel_metrics_gauge")).
		Prewhere(func(b *chsql.Builder) {
			b.Ident("TimeUnix")
			b.WriteSQL(" > ")
			b.Now64()
		}).
		Where(
			func(b *chsql.Builder) {
				b.Ident("MetricName")
				b.WriteSQL(" = ")
				b.Arg("http_requests_total")
			},
			func(b *chsql.Builder) {
				b.Ident("Value")
				b.WriteSQL(" > ")
				b.Arg(0.5)
			},
		).
		Build()

	wantSQL := "SELECT `Value` FROM `otel_metrics_gauge`" +
		" PREWHERE `TimeUnix` > now64(9)" +
		" WHERE `MetricName` = ? AND `Value` > ?"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	if want := []any{"http_requests_total", 0.5}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestSelectBuilder_GroupByOrderBy — GROUP BY + ORDER BY composition;
// DESC flag on the OrderBy key emits the DESC keyword.
func TestSelectBuilder_GroupByOrderBy(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewSelect().
		Select(chsql.Col("MetricName"), chsql.Raw("sum(`Value`) AS `total`")).
		From(chsql.Col("otel_metrics_gauge")).
		GroupBy(chsql.Col("MetricName")).
		OrderBy(chsql.Col("total"), true).
		Limit(10).
		Build()

	wantSQL := "SELECT `MetricName`, sum(`Value`) AS `total`" +
		" FROM `otel_metrics_gauge`" +
		" GROUP BY `MetricName`" +
		" ORDER BY `total` DESC" +
		" LIMIT 10"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	if args != nil {
		t.Errorf("Args = %v; want nil", args)
	}
}

// TestSelectBuilder_NestedSubquery — the worst-case nested case the
// roadmap calls out: an inner SELECT with its own placeholders feeds
// the outer SELECT's FROM, and an outer WHERE adds another `?`. Args
// must appear in the same order as the `?` placeholders in the
// rendered SQL.
func TestSelectBuilder_NestedSubquery(t *testing.T) {
	t.Parallel()

	inner := chsql.NewSelect().
		Select(chsql.Col("MetricName"), chsql.Col("Value")).
		From(chsql.Col("otel_metrics_gauge")).
		Where(func(b *chsql.Builder) {
			b.MapAt("Attributes", "service.name")
			b.WriteSQL(" = ")
			b.Arg("api")
		})

	sql, args := chsql.NewSelect().
		Select(chsql.Col("Value")).
		From(inner.Frag()).
		Where(func(b *chsql.Builder) {
			b.Ident("Value")
			b.WriteSQL(" > ")
			b.Arg(0.5)
		}).
		Build()

	wantSQL := "SELECT `Value` FROM (" +
		"SELECT `MetricName`, `Value` FROM `otel_metrics_gauge`" +
		" WHERE `Attributes`[?] = ?" +
		") WHERE `Value` > ?"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	// `?` order: MapAt key, inner WHERE rhs, outer WHERE rhs.
	if want := []any{"service.name", "api", 0.5}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestBuilder_Expr — Builder.Expr renders representative chplan
// expression shapes with byte-identical output to the legacy
// emitter.emitExpr. Locked here so the RC6 R6.2 port can't drift from
// the canonical shape before R6.4 collapses both paths.
func TestBuilder_Expr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		expr    chplan.Expr
		wantSQL string
		wantArg []any
	}{
		{
			name:    "column_ref",
			expr:    &chplan.ColumnRef{Name: "MetricName"},
			wantSQL: "`MetricName`",
		},
		{
			name:    "column_ref_qualified",
			expr:    &chplan.ColumnRef{Qualifier: "L", Name: "Value"},
			wantSQL: "`L`.`Value`",
		},
		{
			name:    "lit_string",
			expr:    &chplan.LitString{V: "http_requests_total"},
			wantSQL: "?",
			wantArg: []any{"http_requests_total"},
		},
		{
			name: "binary_eq",
			expr: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "x"},
			},
			wantSQL: "(`MetricName` = ?)",
			wantArg: []any{"x"},
		},
		{
			name: "binary_match",
			expr: &chplan.Binary{
				Op:    chplan.OpMatch,
				Left:  &chplan.ColumnRef{Name: "ServiceName"},
				Right: &chplan.LitString{V: "^api-.*"},
			},
			wantSQL: "match(`ServiceName`, ?)",
			wantArg: []any{"^api-.*"},
		},
		{
			// TraceQL link / event spanset filters lower to this shape
			// (see chplan.NestedArrayExists). Key + Value bind through
			// Arg, so the rendered SQL carries two parameter slots.
			name: "nested_array_exists_eq",
			expr: &chplan.NestedArrayExists{
				Column:   "Links",
				SubField: "Attributes",
				Key:      "span_id",
				Op:       chplan.OpEq,
				Value:    &chplan.LitString{V: "abc"},
			},
			wantSQL: "arrayExists(x -> x[?] = ?, `Links`.`Attributes`)",
			wantArg: []any{"span_id", "abc"},
		},
		{
			name: "nested_array_exists_ne",
			expr: &chplan.NestedArrayExists{
				Column:   "Events",
				SubField: "Attributes",
				Key:      "severity",
				Op:       chplan.OpNe,
				Value:    &chplan.LitString{V: "info"},
			},
			wantSQL: "arrayExists(x -> x[?] != ?, `Events`.`Attributes`)",
			wantArg: []any{"severity", "info"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := chsql.NewBuilder()
			if err := b.Expr(tc.expr); err != nil {
				t.Fatalf("Expr: %v", err)
			}
			sql, args := b.Build()
			if sql != tc.wantSQL {
				t.Errorf("SQL = %q; want %q", sql, tc.wantSQL)
			}
			if !reflect.DeepEqual(args, tc.wantArg) {
				t.Errorf("Args = %v; want %v", args, tc.wantArg)
			}
		})
	}
}
