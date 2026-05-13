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
// Frags that can be plugged into a QueryBuilder slot; args bind in
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

// TestUnionAll_JoinsParts — UnionAll joins multiple Frags with the
// " UNION ALL " keyword in stream order, and args bound inside each
// part land at the position they're emitted.
func TestUnionAll_JoinsParts(t *testing.T) {
	t.Parallel()

	left := chsql.NewQuery().
		Select(chsql.Col("MetricName")).
		From(chsql.Col("gauge")).
		Where(func(b *chsql.Builder) {
			b.Ident("MetricName")
			b.WriteSQL(" = ")
			b.Arg("a")
		})
	right := chsql.NewQuery().
		Select(chsql.Col("MetricName")).
		From(chsql.Col("sum")).
		Where(func(b *chsql.Builder) {
			b.Ident("MetricName")
			b.WriteSQL(" = ")
			b.Arg("b")
		})

	b := chsql.NewBuilder()
	chsql.UnionAll(left.Frag(), right.Frag())(b)
	sql, args := b.Build()
	want := "(SELECT `MetricName` FROM `gauge` WHERE `MetricName` = ?)" +
		" UNION ALL " +
		"(SELECT `MetricName` FROM `sum` WHERE `MetricName` = ?)"
	if sql != want {
		t.Errorf("UnionAll = %q; want %q", sql, want)
	}
	wantArgs := []any{"a", "b"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Args = %v; want %v", args, wantArgs)
	}
}

// TestUnionAll_SinglePart — one part renders without the UNION ALL
// keyword (the join separator only applies between parts).
func TestUnionAll_SinglePart(t *testing.T) {
	t.Parallel()

	only := chsql.NewQuery().
		Select(chsql.Col("x")).
		From(chsql.Col("t"))

	b := chsql.NewBuilder()
	chsql.UnionAll(only.Frag())(b)
	if got, want := b.String(), "(SELECT `x` FROM `t`)"; got != want {
		t.Errorf("UnionAll(single) = %q; want %q", got, want)
	}
}

// TestUnionAll_PanicsOnEmpty — UnionAll with zero parts is a
// programmer error.
func TestUnionAll_PanicsOnEmpty(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on UnionAll()")
		}
	}()
	chsql.UnionAll()
}

// TestQueryBuilder_SelectAs — SelectAs slot adds "<expr> AS <alias>"
// to the SELECT list without composing the AS keyword by hand at the
// call site.
func TestQueryBuilder_SelectAs(t *testing.T) {
	t.Parallel()

	sql, _ := chsql.NewQuery().
		SelectAs(chsql.Col("MetricName"), "name").
		SelectAs(chsql.Col("Value"), "").
		From(chsql.Col("otel_metrics_gauge")).
		Build()
	want := "SELECT `MetricName` AS `name`, `Value` FROM `otel_metrics_gauge`"
	if sql != want {
		t.Errorf("SelectAs = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_Empty — empty QueryBuilder renders "SELECT *".
// (No FROM is fine; CH accepts SELECT * alone for fixture-style
// shapes, even if it's not what production emits.)
func TestQueryBuilder_Empty(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewQuery().Build()
	if want := "SELECT *"; sql != want {
		t.Errorf("empty SELECT = %q; want %q", sql, want)
	}
	if args != nil {
		t.Errorf("empty SELECT args = %v; want nil", args)
	}
}

// TestQueryBuilder_Basic — Select / From / Where / Limit composed
// in order, args from a Lit() in Where bind at the WHERE position.
func TestQueryBuilder_Basic(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewQuery().
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

// TestQueryBuilder_Prewhere — PREWHERE is emitted before WHERE in
// the rendered SQL; multiple predicates in either slot join with AND.
func TestQueryBuilder_Prewhere(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewQuery().
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

// TestQueryBuilder_GroupByOrderBy — GROUP BY + ORDER BY composition;
// DESC flag on the OrderBy key emits the DESC keyword.
func TestQueryBuilder_GroupByOrderBy(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewQuery().
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

// TestQueryBuilder_NestedSubquery — the worst-case nested case the
// roadmap calls out: an inner SELECT with its own placeholders feeds
// the outer SELECT's FROM, and an outer WHERE adds another `?`. Args
// must appear in the same order as the `?` placeholders in the
// rendered SQL.
func TestQueryBuilder_NestedSubquery(t *testing.T) {
	t.Parallel()

	inner := chsql.NewQuery().
		Select(chsql.Col("MetricName"), chsql.Col("Value")).
		From(chsql.Col("otel_metrics_gauge")).
		Where(func(b *chsql.Builder) {
			b.MapAt("Attributes", "service.name")
			b.WriteSQL(" = ")
			b.Arg("api")
		})

	sql, args := chsql.NewQuery().
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

// TestQueryBuilder_Join — QueryBuilder.Join appends an INNER JOIN
// (or other JoinKind) clause; args from the source and ON fragments
// land in emission order between the SELECT/FROM args and the WHERE
// args.
func TestQueryBuilder_Join(t *testing.T) {
	t.Parallel()

	sql, args := chsql.NewQuery().
		Select(chsql.Raw("L.`Value`"), chsql.Raw("R.`Value`")).
		From(func(b *chsql.Builder) {
			b.Ident("otel_metrics_sum")
			b.WriteSQL(" AS L")
		}).
		Join(
			chsql.InnerJoin,
			func(b *chsql.Builder) {
				b.Ident("otel_metrics_gauge")
				b.WriteSQL(" AS R")
			},
			func(b *chsql.Builder) {
				b.WriteSQL("L.")
				b.Ident("MetricName")
				b.WriteSQL(" = ")
				b.Arg("http_requests_total")
			},
		).
		Where(func(b *chsql.Builder) {
			b.WriteSQL("R.")
			b.Ident("Value")
			b.WriteSQL(" > ")
			b.Arg(0.5)
		}).
		Build()

	wantSQL := "SELECT L.`Value`, R.`Value`" +
		" FROM `otel_metrics_sum` AS L" +
		" INNER JOIN `otel_metrics_gauge` AS R ON L.`MetricName` = ?" +
		" WHERE R.`Value` > ?"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	if want := []any{"http_requests_total", 0.5}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestQueryBuilder_JoinKinds — every JoinKind constant renders as its
// literal SQL keyword pair. CrossJoin suppresses the ON clause.
func TestQueryBuilder_JoinKinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind chsql.JoinKind
		want string
	}{
		{chsql.InnerJoin, "INNER JOIN"},
		{chsql.LeftJoin, "LEFT JOIN"},
		{chsql.RightJoin, "RIGHT JOIN"},
		{chsql.FullJoin, "FULL JOIN"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			sql, _ := chsql.NewQuery().
				From(chsql.Col("a")).
				Join(tc.kind, chsql.Col("b"), chsql.Raw("1 = 1")).
				Build()
			want := "SELECT * FROM `a` " + tc.want + " `b` ON 1 = 1"
			if sql != want {
				t.Errorf("kind=%v sql=%q want=%q", tc.kind, sql, want)
			}
		})
	}

	// CrossJoin drops the ON clause; the on Frag is allowed to be nil.
	sql, _ := chsql.NewQuery().
		From(chsql.Col("a")).
		Join(chsql.CrossJoin, chsql.Col("b"), nil).
		Build()
	if want := "SELECT * FROM `a` CROSS JOIN `b`"; sql != want {
		t.Errorf("CrossJoin sql=%q want=%q", sql, want)
	}
}

// TestQueryBuilder_WithRecursive — WithRecursive renders the
// `WITH RECURSIVE <name> AS (<anchor> UNION ALL <recursive>)` head and
// threads anchor + recursive args in order ahead of the outer
// SELECT's args.
func TestQueryBuilder_WithRecursive(t *testing.T) {
	t.Parallel()

	anchor := chsql.NewQuery().
		Select(chsql.Col("id"), chsql.Raw("0 AS _depth")).
		From(chsql.Col("nodes")).
		Where(func(b *chsql.Builder) {
			b.Ident("id")
			b.WriteSQL(" = ")
			b.Arg(1)
		})

	step := chsql.NewQuery().
		Select(
			func(b *chsql.Builder) {
				b.WriteSQL("n.")
				b.Ident("id")
			},
			chsql.Raw("c._depth + 1"),
		).
		From(func(b *chsql.Builder) {
			b.Ident("nodes")
			b.WriteSQL(" AS n")
		}).
		Join(
			chsql.InnerJoin,
			chsql.Raw("closure AS c"),
			chsql.Raw("n.parent = c.id"),
		).
		Where(func(b *chsql.Builder) {
			b.WriteSQL("c._depth < ")
			b.Arg(5)
		})

	sql, args := chsql.NewQuery().
		WithRecursive("closure", anchor, step).
		Select(chsql.Col("id")).
		From(chsql.Raw("closure")).
		Where(chsql.Raw("_depth > 0")).
		Build()

	wantSQL := "WITH RECURSIVE closure AS (" +
		"SELECT `id`, 0 AS _depth FROM `nodes` WHERE `id` = ?" +
		" UNION ALL " +
		"SELECT n.`id`, c._depth + 1 FROM `nodes` AS n" +
		" INNER JOIN closure AS c ON n.parent = c.id" +
		" WHERE c._depth < ?" +
		") SELECT `id` FROM closure WHERE _depth > 0"
	if sql != wantSQL {
		t.Errorf("SQL = %q; want %q", sql, wantSQL)
	}
	if want := []any{1, 5}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestQueryBuilder_WithRecursive_PanicsOnNil — passing a nil anchor
// or recursive panics at render time. The slot stores them via the
// fluent API without inspection, so the guard fires when writeInto
// walks the CTE chain.
func TestQueryBuilder_WithRecursive_PanicsOnNil(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("WithRecursive(nil, nil) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "WithRecursive") {
			t.Errorf("panic value = %v; want message mentioning WithRecursive", r)
		}
	}()
	chsql.NewQuery().
		WithRecursive("closure", nil, nil).
		From(chsql.Col("x")).
		Build()
}

// TestQueryBuilder_Join_PanicsOnNilON — Join with a nil ON Frag and
// a non-CrossJoin kind panics at render time (CrossJoin is the only
// kind that legitimately omits ON).
func TestQueryBuilder_Join_PanicsOnNilON(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Join(InnerJoin, ..., nil) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Join") {
			t.Errorf("panic value = %v; want message mentioning Join", r)
		}
	}()
	chsql.NewQuery().
		From(chsql.Col("a")).
		Join(chsql.InnerJoin, chsql.Col("b"), nil).
		Build()
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
