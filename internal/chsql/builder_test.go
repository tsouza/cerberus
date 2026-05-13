package chsql

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestBuilder_Empty — the zero-value Builder renders empty SQL and a
// nil args slice. Confirms NewBuilder is unnecessary; the zero value
// is usable.
func TestBuilder_Empty(t *testing.T) {
	t.Parallel()

	var b Builder
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
			b := NewBuilder()
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

	b := NewBuilder()
	b.QualIdent("L", "Value")
	if got, want := b.String(), "`L`.`Value`"; got != want {
		t.Errorf("QualIdent = %q; want %q", got, want)
	}
}

// TestBuilder_Arg — `?` placeholder appends a positional arg.
func TestBuilder_Arg(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	b.Arg("hello")
	b.writeSQL(", ")
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

	b := NewBuilder()
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

	b := NewBuilder()
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

	b := NewBuilder()
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
	b := NewBuilder()
	b.MapFilterExcept("Attributes")
}

// TestBuilder_Now64 — bare CH builtin, no args.
func TestBuilder_Now64(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
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

	b := NewBuilder()
	b.SubtractNanos(func(b *Builder) { b.Now64() }, int64(5*time.Minute))
	if got, want := b.String(), "(now64(9) - toIntervalNanosecond(300000000000))"; got != want {
		t.Errorf("SubtractNanos = %q; want %q", got, want)
	}
}

// TestBuilder_SubtractNanos_PreservesArgOrder — args bound inside lhs
// appear in the args slice at the position they were emitted (i.e.
// before the literal ns).
func TestBuilder_SubtractNanos_PreservesArgOrder(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	b.SubtractNanos(func(b *Builder) {
		b.writeSQL("max(")
		b.MapAt("Attributes", "service.name")
		b.writeSQL(")")
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
	b := NewBuilder()
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
	b := NewBuilder()
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

	b := NewBuilder()
	b.Lambda([]string{"k", "v"}, func(b *Builder) {
		b.writeSQL("k = ")
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

	b := NewBuilder()
	b.ParamAgg(
		"quantile",
		[]func(b *Builder){
			func(b *Builder) { b.Arg(0.95) },
		},
		[]func(b *Builder){
			func(b *Builder) { b.Ident("Value") },
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

	b := NewBuilder()
	b.ParamAgg(
		"sum",
		nil,
		[]func(b *Builder){
			func(b *Builder) { b.Ident("Value") },
		},
	)
	if got, want := b.String(), "sum(`Value`)"; got != want {
		t.Errorf("ParamAgg(no params) = %q; want %q", got, want)
	}
}

// TestBuilder_ParamAgg_MultiParam — quantiles(0.5, 0.9)(value) style.
func TestBuilder_ParamAgg_MultiParam(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	b.ParamAgg(
		"quantiles",
		[]func(b *Builder){
			func(b *Builder) { b.Arg(0.5) },
			func(b *Builder) { b.Arg(0.9) },
		},
		[]func(b *Builder){
			func(b *Builder) { b.Ident("Value") },
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

	b := NewBuilder()
	Col("Value")(b)
	b.writeSQL(", ")
	Lit(42)(b)
	b.writeSQL(", ")
	Raw("now64(9)")(b)
	b.writeSQL(", ")
	Qual("L", "TimeUnix")(b)
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

	b := NewBuilder()
	As(Col("Value"), "v")(b)
	if got, want := b.String(), "`Value` AS `v`"; got != want {
		t.Errorf("As(Col,v) = %q; want %q", got, want)
	}

	b2 := NewBuilder()
	As(Col("Value"), "")(b2)
	if got, want := b2.String(), "`Value`"; got != want {
		t.Errorf("As(Col,\"\") = %q; want %q", got, want)
	}
}

// TestUnionAll_JoinsParts — UnionAll joins multiple Frags with the
// " UNION ALL " keyword in stream order, and args bound inside each
// part land at the position they're emitted.
func TestUnionAll_JoinsParts(t *testing.T) {
	t.Parallel()

	left := NewQuery().
		Select(Col("MetricName")).
		From(Col("gauge")).
		Where(func(b *Builder) {
			b.Ident("MetricName")
			b.writeSQL(" = ")
			b.Arg("a")
		})
	right := NewQuery().
		Select(Col("MetricName")).
		From(Col("sum")).
		Where(func(b *Builder) {
			b.Ident("MetricName")
			b.writeSQL(" = ")
			b.Arg("b")
		})

	b := NewBuilder()
	UnionAll(left.Frag(), right.Frag())(b)
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

	only := NewQuery().
		Select(Col("x")).
		From(Col("t"))

	b := NewBuilder()
	UnionAll(only.Frag())(b)
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
	UnionAll()
}

// TestQueryBuilder_SelectAs — SelectAs slot adds "<expr> AS <alias>"
// to the SELECT list without composing the AS keyword by hand at the
// call site.
func TestQueryBuilder_SelectAs(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		SelectAs(Col("MetricName"), "name").
		SelectAs(Col("Value"), "").
		From(Col("otel_metrics_gauge")).
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

	sql, args := NewQuery().Build()
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

	sql, args := NewQuery().
		Select(Col("MetricName"), Col("Value")).
		From(Col("otel_metrics_gauge")).
		Where(func(b *Builder) {
			b.Ident("MetricName")
			b.writeSQL(" = ")
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

	sql, args := NewQuery().
		Select(Col("Value")).
		From(Col("otel_metrics_gauge")).
		Prewhere(func(b *Builder) {
			b.Ident("TimeUnix")
			b.writeSQL(" > ")
			b.Now64()
		}).
		Where(
			func(b *Builder) {
				b.Ident("MetricName")
				b.writeSQL(" = ")
				b.Arg("http_requests_total")
			},
			func(b *Builder) {
				b.Ident("Value")
				b.writeSQL(" > ")
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

	sql, args := NewQuery().
		Select(Col("MetricName"), Raw("sum(`Value`) AS `total`")).
		From(Col("otel_metrics_gauge")).
		GroupBy(Col("MetricName")).
		OrderBy(Col("total"), true).
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

	inner := NewQuery().
		Select(Col("MetricName"), Col("Value")).
		From(Col("otel_metrics_gauge")).
		Where(func(b *Builder) {
			b.MapAt("Attributes", "service.name")
			b.writeSQL(" = ")
			b.Arg("api")
		})

	sql, args := NewQuery().
		Select(Col("Value")).
		From(inner.Frag()).
		Where(func(b *Builder) {
			b.Ident("Value")
			b.writeSQL(" > ")
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

	sql, args := NewQuery().
		Select(Raw("L.`Value`"), Raw("R.`Value`")).
		From(func(b *Builder) {
			b.Ident("otel_metrics_sum")
			b.writeSQL(" AS L")
		}).
		Join(
			InnerJoin,
			func(b *Builder) {
				b.Ident("otel_metrics_gauge")
				b.writeSQL(" AS R")
			},
			func(b *Builder) {
				b.writeSQL("L.")
				b.Ident("MetricName")
				b.writeSQL(" = ")
				b.Arg("http_requests_total")
			},
		).
		Where(func(b *Builder) {
			b.writeSQL("R.")
			b.Ident("Value")
			b.writeSQL(" > ")
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
		kind JoinKind
		want string
	}{
		{InnerJoin, "INNER JOIN"},
		{LeftJoin, "LEFT JOIN"},
		{RightJoin, "RIGHT JOIN"},
		{FullJoin, "FULL JOIN"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			sql, _ := NewQuery().
				From(Col("a")).
				Join(tc.kind, Col("b"), Raw("1 = 1")).
				Build()
			want := "SELECT * FROM `a` " + tc.want + " `b` ON 1 = 1"
			if sql != want {
				t.Errorf("kind=%v sql=%q want=%q", tc.kind, sql, want)
			}
		})
	}

	// CrossJoin drops the ON clause; the on Frag is allowed to be nil.
	sql, _ := NewQuery().
		From(Col("a")).
		Join(CrossJoin, Col("b"), nil).
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

	anchor := NewQuery().
		Select(Col("id"), Raw("0 AS _depth")).
		From(Col("nodes")).
		Where(func(b *Builder) {
			b.Ident("id")
			b.writeSQL(" = ")
			b.Arg(1)
		})

	step := NewQuery().
		Select(
			func(b *Builder) {
				b.writeSQL("n.")
				b.Ident("id")
			},
			Raw("c._depth + 1"),
		).
		From(func(b *Builder) {
			b.Ident("nodes")
			b.writeSQL(" AS n")
		}).
		Join(
			InnerJoin,
			Raw("closure AS c"),
			Raw("n.parent = c.id"),
		).
		Where(func(b *Builder) {
			b.writeSQL("c._depth < ")
			b.Arg(5)
		})

	sql, args := NewQuery().
		WithRecursive("closure", anchor, step).
		Select(Col("id")).
		From(Raw("closure")).
		Where(Raw("_depth > 0")).
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
	NewQuery().
		WithRecursive("closure", nil, nil).
		From(Col("x")).
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
	NewQuery().
		From(Col("a")).
		Join(InnerJoin, Col("b"), nil).
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
			b := NewBuilder()
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

// --- typed operator / punctuation Frag constructors (R6.11a) -----------

// TestOperatorFrags_BinaryOps — each comparison + arithmetic operator
// renders "<l> <op> <r>" with single spaces around the op token, and
// the Lit-bound argument lands in the args slice in emission order.
func TestOperatorFrags_BinaryOps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		frag    Frag
		wantSQL string
	}{
		{"Eq", Eq(Col("a"), Lit(int64(1))), "`a` = ?"},
		{"Neq", Neq(Col("a"), Lit(int64(1))), "`a` != ?"},
		{"Gt", Gt(Col("a"), Lit(int64(1))), "`a` > ?"},
		{"Gte", Gte(Col("a"), Lit(int64(1))), "`a` >= ?"},
		{"Lt", Lt(Col("a"), Lit(int64(1))), "`a` < ?"},
		{"Lte", Lte(Col("a"), Lit(int64(1))), "`a` <= ?"},
		{"Like", Like(Col("a"), Lit("x%")), "`a` LIKE ?"},
		{"NotLike", NotLike(Col("a"), Lit("x%")), "`a` NOT LIKE ?"},
		{"Add", Add(Col("a"), Lit(int64(1))), "`a` + ?"},
		{"Sub", Sub(Col("a"), Lit(int64(1))), "`a` - ?"},
		{"Mul", Mul(Col("a"), Lit(int64(2))), "`a` * ?"},
		{"Div", Div(Col("a"), Lit(int64(2))), "`a` / ?"},
		{"Mod", Mod(Col("a"), Lit(int64(2))), "`a` % ?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewBuilder()
			tc.frag(b)
			if got := b.String(); got != tc.wantSQL {
				t.Errorf("%s SQL = %q; want %q", tc.name, got, tc.wantSQL)
			}
			if len(b.Args()) != 1 {
				t.Errorf("%s args len = %d; want 1", tc.name, len(b.Args()))
			}
		})
	}
}

// TestAnd_JoinsParts — And joins predicates with " AND " and binds
// args in left-to-right order.
func TestAnd_JoinsParts(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	And(
		Eq(Col("a"), Lit(int64(1))),
		Eq(Col("b"), Lit(int64(2))),
		Eq(Col("c"), Lit(int64(3))),
	)(b)
	sql, args := b.Build()
	want := "`a` = ? AND `b` = ? AND `c` = ?"
	if sql != want {
		t.Errorf("And SQL = %q; want %q", sql, want)
	}
	if !reflect.DeepEqual(args, []any{int64(1), int64(2), int64(3)}) {
		t.Errorf("And args = %v", args)
	}
}

// TestAnd_PanicsOnEmpty — And() with zero parts is a programmer error.
func TestAnd_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on And()")
		}
	}()
	And()
}

// TestOr_JoinsParts — Or joins predicates with " OR ".
func TestOr_JoinsParts(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Or(
		Eq(Col("a"), Lit(int64(1))),
		Eq(Col("b"), Lit(int64(2))),
	)(b)
	if got, want := b.String(), "`a` = ? OR `b` = ?"; got != want {
		t.Errorf("Or SQL = %q; want %q", got, want)
	}
}

// TestOr_PanicsOnEmpty — Or() with zero parts is a programmer error.
func TestOr_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on Or()")
		}
	}()
	Or()
}

// TestNot_Prefixes — Not emits "NOT " before the inner Frag and does
// not add parens; precedence is the caller's responsibility.
func TestNot_Prefixes(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Not(Eq(Col("a"), Lit(int64(1))))(b)
	if got, want := b.String(), "NOT `a` = ?"; got != want {
		t.Errorf("Not SQL = %q; want %q", got, want)
	}
}

// TestNeg_Prefixes — Neg emits "-" with no space before the operand.
func TestNeg_Prefixes(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Neg(Col("a"))(b)
	if got, want := b.String(), "-`a`"; got != want {
		t.Errorf("Neg SQL = %q; want %q", got, want)
	}
}

// TestParen_Wraps — Paren wraps the inner Frag with no inner spaces.
func TestParen_Wraps(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Paren(Or(
		Eq(Col("a"), Lit(int64(1))),
		Eq(Col("b"), Lit(int64(2))),
	))(b)
	if got, want := b.String(), "(`a` = ? OR `b` = ?)"; got != want {
		t.Errorf("Paren SQL = %q; want %q", got, want)
	}
}

// TestTuple_RendersCommaSeparated — Tuple emits "(<p0>, <p1>, ...)".
func TestTuple_RendersCommaSeparated(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Tuple(Lit(int64(1)), Lit(int64(2)), Lit(int64(3)))(b)
	sql, args := b.Build()
	if got, want := sql, "(?, ?, ?)"; got != want {
		t.Errorf("Tuple SQL = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(args, []any{int64(1), int64(2), int64(3)}) {
		t.Errorf("Tuple args = %v", args)
	}
}

// TestTuple_PanicsOnEmpty — Tuple() with zero parts is a programmer
// error.
func TestTuple_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on Tuple()")
		}
	}()
	Tuple()
}

// TestCast_Wraps — Cast renders "CAST(<f> AS <typ>)" with the type
// name emitted verbatim (no quoting).
func TestCast_Wraps(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Cast(Col("a"), "Float64")(b)
	if got, want := b.String(), "CAST(`a` AS Float64)"; got != want {
		t.Errorf("Cast SQL = %q; want %q", got, want)
	}
}

// TestConcat_NoSeparator — Concat emits parts back-to-back without
// any glue. Args bind in emission order.
func TestConcat_NoSeparator(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Concat(
		Col("a"),
		Raw(" = "),
		Lit(int64(1)),
	)(b)
	sql, args := b.Build()
	if got, want := sql, "`a` = ?"; got != want {
		t.Errorf("Concat SQL = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(args, []any{int64(1)}) {
		t.Errorf("Concat args = %v", args)
	}
}

// TestConcat_PanicsOnEmpty — Concat() with zero parts is a programmer
// error.
func TestConcat_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on Concat()")
		}
	}()
	Concat()
}

// TestIn_RendersList — In emits "<left> IN (<r0>, <r1>, ...)" and
// binds Lit args in emission order.
func TestIn_RendersList(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	In(Col("a"), Lit("x"), Lit("y"), Lit("z"))(b)
	sql, args := b.Build()
	if got, want := sql, "`a` IN (?, ?, ?)"; got != want {
		t.Errorf("In SQL = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(args, []any{"x", "y", "z"}) {
		t.Errorf("In args = %v", args)
	}
}

// TestIn_PanicsOnEmpty — In with no right-hand parts is a programmer
// error (empty IN list is a CH syntax error).
func TestIn_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on In() with empty right")
		}
	}()
	In(Col("a"))
}

// TestCall_NoArgs — Call with zero args renders as "<name>()", valid
// for nullary CH functions like now().
func TestCall_NoArgs(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Call("now")(b)
	if got, want := b.String(), "now()"; got != want {
		t.Errorf("Call(now) = %q; want %q", got, want)
	}
}

// TestCall_SingleArg — Call with one arg renders as "<name>(<a0>)".
func TestCall_SingleArg(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Call("any", Col("v"))(b)
	if got, want := b.String(), "any(`v`)"; got != want {
		t.Errorf("Call(any,v) = %q; want %q", got, want)
	}
}

// TestCall_MultipleArgs — Call with multiple args comma-separates them
// and binds inner args at their emission position.
func TestCall_MultipleArgs(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Call("if", Eq(Col("a"), Lit(1)), Lit("y"), Lit("n"))(b)
	sql, args := b.Build()
	if want := "if(`a` = ?, ?, ?)"; sql != want {
		t.Errorf("Call(if,...) = %q; want %q", sql, want)
	}
	if wantArgs := []any{1, "y", "n"}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Args = %v; want %v", args, wantArgs)
	}
}

// TestParametric_OneParamOneArg — the basic parametric aggregate shape
// "<name>(<p>)(<a>)" with a single param and single arg.
func TestParametric_OneParamOneArg(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Parametric("quantile", []Frag{Lit(0.5)}, Col("Value"))(b)
	sql, args := b.Build()
	if want := "quantile(?)(`Value`)"; sql != want {
		t.Errorf("Parametric = %q; want %q", sql, want)
	}
	if wantArgs := []any{0.5}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Args = %v; want %v", args, wantArgs)
	}
}

// TestParametric_MultiParamMultiArg — params and args lists are both
// comma-separated; args bind in stream order after params.
func TestParametric_MultiParamMultiArg(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Parametric("quantiles", []Frag{Lit(0.5), Lit(0.9)}, Col("a"), Col("b"))(b)
	sql, args := b.Build()
	if want := "quantiles(?, ?)(`a`, `b`)"; sql != want {
		t.Errorf("Parametric = %q; want %q", sql, want)
	}
	if wantArgs := []any{0.5, 0.9}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Args = %v; want %v", args, wantArgs)
	}
}

// TestParametric_PanicsOnEmptyParams — zero params is rejected so the
// API stays distinguishable from a plain Call.
func TestParametric_PanicsOnEmptyParams(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on Parametric with empty params")
		}
	}()
	Parametric("quantile", nil, Col("Value"))
}

// TestStar — the unqualified wildcard renders as "*".
func TestStar(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Star()(b)
	if got, want := b.String(), "*"; got != want {
		t.Errorf("Star = %q; want %q", got, want)
	}
}

// TestQualStar_BasicQuoting — QualStar renders "<table>.*" with the
// table identifier backtick-quoted.
func TestQualStar_BasicQuoting(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	QualStar("L")(b)
	if got, want := b.String(), "`L`.*"; got != want {
		t.Errorf("QualStar(L) = %q; want %q", got, want)
	}
}

// TestQualStar_EscapesBackticks — embedded backticks in the table
// identifier are doubled (mirrors Ident's escape).
func TestQualStar_EscapesBackticks(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	QualStar("a`b")(b)
	if got, want := b.String(), "`a``b`.*"; got != want {
		t.Errorf("QualStar(a`b) = %q; want %q", got, want)
	}
}

// TestDistinct — Distinct prefixes its operand with "DISTINCT ".
func TestDistinct(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Distinct(Col("Value"))(b)
	if got, want := b.String(), "DISTINCT `Value`"; got != want {
		t.Errorf("Distinct = %q; want %q", got, want)
	}
}

// TestIsNull — IsNull appends " IS NULL" to its operand.
func TestIsNull(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	IsNull(Col("Value"))(b)
	if got, want := b.String(), "`Value` IS NULL"; got != want {
		t.Errorf("IsNull = %q; want %q", got, want)
	}
}

// TestIsNotNull — IsNotNull appends " IS NOT NULL" to its operand.
func TestIsNotNull(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	IsNotNull(Col("Value"))(b)
	if got, want := b.String(), "`Value` IS NOT NULL"; got != want {
		t.Errorf("IsNotNull = %q; want %q", got, want)
	}
}

// TestBetween — Between renders "<f> BETWEEN <lo> AND <hi>" and binds
// args in stream order.
func TestBetween(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Between(Col("ts"), Lit(1), Lit(10))(b)
	sql, args := b.Build()
	if want := "`ts` BETWEEN ? AND ?"; sql != want {
		t.Errorf("Between = %q; want %q", sql, want)
	}
	if wantArgs := []any{1, 10}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Args = %v; want %v", args, wantArgs)
	}
}
