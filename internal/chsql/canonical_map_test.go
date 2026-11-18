package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// typeCastStrategies is the SchemaStrategies a server below 25.1 renders
// under (chopt.FeatureCanonicalMapTypeCast).
var typeCastStrategies = SchemaStrategies{CanonicalMapTypeCast: true}

func TestCanonicalMap_BareWithoutTypeCast(t *testing.T) {
	t.Parallel()

	sql, args := Render(CanonicalMap(Col("Attributes")))
	if want := "mapSort(`Attributes`)"; sql != want {
		t.Fatalf("CanonicalMap rendered %q, want %q", sql, want)
	}
	if len(args) != 0 {
		t.Fatalf("CanonicalMap over a column bound %v, want no args", args)
	}
}

func TestCanonicalMap_TypeCastReadsTheOperandType(t *testing.T) {
	t.Parallel()

	sql, args := Render(UnderSchemaStrategies(typeCastStrategies, CanonicalMap(Col("Attributes"))))
	if want := "CAST(mapSort(`Attributes`), toTypeName(`Attributes`))"; sql != want {
		t.Fatalf("CanonicalMap rendered %q, want %q", sql, want)
	}
	if len(args) != 0 {
		t.Fatalf("CanonicalMap over a column bound %v, want no args", args)
	}
}

// The operand is rendered once and replayed into toTypeName, so an operand
// that binds arguments binds them once per copy, in rendering order — the
// placeholders and the bound arguments stay aligned.
func TestCanonicalMap_TypeCastReplaysOperandArgs(t *testing.T) {
	t.Parallel()

	keep := Call("mapFilter", Lambda2("k", "v", And(Eq(BareIdent("k"), Lit("job")), Eq(BareIdent("v"), Lit("api")))), Col("Attributes"))
	sql, args := Render(UnderSchemaStrategies(typeCastStrategies, CanonicalMap(keep)))
	inner := "mapFilter((k, v) -> k = ? AND v = ?, `Attributes`)"
	if want := "CAST(mapSort(" + inner + "), toTypeName(" + inner + "))"; sql != want {
		t.Fatalf("CanonicalMap rendered %q, want %q", sql, want)
	}
	want := []any{"job", "api", "job", "api"}
	if len(args) != len(want) {
		t.Fatalf("CanonicalMap bound %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("CanonicalMap bound %v, want %v", args, want)
		}
	}
}

// The plan IR's canonical key (chplan.FnMapSort) renders through
// CanonicalMap, so it follows the statement's strategies too.
func TestExprCanonicalMap_FollowsStrategies(t *testing.T) {
	t.Parallel()

	key := chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"})
	render := func(s SchemaStrategies) string {
		sql, _ := Render(UnderSchemaStrategies(s, func(b *Builder) { _ = b.Expr(key) }))
		return sql
	}
	if got, want := render(SchemaStrategies{}), "mapSort(`Attributes`)"; got != want {
		t.Errorf("zero strategies: rendered %q, want %q", got, want)
	}
	if got, want := render(typeCastStrategies), "CAST(mapSort(`Attributes`), toTypeName(`Attributes`))"; got != want {
		t.Errorf("CanonicalMapTypeCast: rendered %q, want %q", got, want)
	}
}

func TestExprCanonicalMap_RejectsArity(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.FuncCall{Fn: chplan.FnMapSort})
	if err == nil {
		t.Fatal("chplan.FnMapSort with no argument rendered without an error")
	}
}

// The cell stamps its CanonicalMapTypeCast on every signal at Load, whatever
// it holds, including strategies a re-probe stores after it was set, and
// independently of PrewhereByServer.
func TestSchemaStrategiesCell_CanonicalMapTypeCast(t *testing.T) {
	t.Parallel()

	c := NewSchemaStrategiesCell(ResolvedSchemaStrategies{})
	if got := c.Load(); got.Metrics.CanonicalMapTypeCast || got.Logs.CanonicalMapTypeCast || got.Traces.CanonicalMapTypeCast {
		t.Fatalf("unset cell loads CanonicalMapTypeCast on some signal: %+v", got)
	}

	c.SetCanonicalMapTypeCast(true)
	c.Store(ResolvedSchemaStrategies{Logs: SchemaStrategies{SumTimestampTimezone: "UTC"}})
	got := c.Load()
	if !got.Metrics.CanonicalMapTypeCast || !got.Logs.CanonicalMapTypeCast || !got.Traces.CanonicalMapTypeCast {
		t.Fatalf("set cell after a Store loads CanonicalMapTypeCast=false on some signal: %+v", got)
	}
	if got.Metrics.PrewhereByServer || got.Logs.SumTimestampTimezone != "UTC" {
		t.Fatalf("Load changed fields other than CanonicalMapTypeCast: %+v", got)
	}

	c.SetCanonicalMapTypeCast(false)
	if got := c.Load(); got.Metrics.CanonicalMapTypeCast || got.Logs.CanonicalMapTypeCast || got.Traces.CanonicalMapTypeCast {
		t.Fatalf("cleared cell still loads CanonicalMapTypeCast: %+v", got)
	}
}

// The replayed copy of the operand carries the physical-table reads of the
// rendered one, so a statement's scan count (Builder.physicalScans) covers
// both copies the server analyses.
func TestCanonicalMap_TypeCastReplaysOperandScans(t *testing.T) {
	t.Parallel()

	operand := Subquery(NewQuery().From(PhysicalTable("otel_metrics_gauge")).Select(Call("any", Col("Attributes"))))
	_, _, bare := RenderCounted(CanonicalMap(operand))
	_, _, cast := RenderCounted(UnderSchemaStrategies(typeCastStrategies, CanonicalMap(operand)))
	if bare != 1 || cast != 2 {
		t.Fatalf("physical scans: bare=%d cast=%d, want 1 and 2", bare, cast)
	}
}

func TestParsedMapType_FollowsStrategies(t *testing.T) {
	t.Parallel()

	merged := Call("mapConcat", Col("Attributes"), Call("map", Lit("le"), Col("le")))
	bare, args := Render(ParsedMapType(merged))
	if want := "mapConcat(`Attributes`, map(?, `le`))"; bare != want {
		t.Errorf("zero strategies: rendered %q, want %q", bare, want)
	}
	if len(args) != 1 {
		t.Errorf("zero strategies: bound %v, want one argument", args)
	}
	cast, args := Render(UnderSchemaStrategies(typeCastStrategies, ParsedMapType(merged)))
	inner := "mapConcat(`Attributes`, map(?, `le`))"
	if want := "CAST(" + inner + ", toTypeName(" + inner + "))"; cast != want {
		t.Errorf("CanonicalMapTypeCast: rendered %q, want %q", cast, want)
	}
	if len(args) != 2 || args[0] != "le" || args[1] != "le" {
		t.Errorf("CanonicalMapTypeCast: bound %v, want the operand's argument once per copy", args)
	}
}

// Every plan-IR Map function in parsedMapTypeFns renders through
// ParsedMapType, so the statement's strategies decide its cast; a Map
// function that returns the parsed type renders bare under either.
func TestExprParsedMapType_FollowsStrategies(t *testing.T) {
	t.Parallel()

	attrs := &chplan.ColumnRef{Name: "Attributes"}
	le := &chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{&chplan.LitString{V: "le"}, &chplan.ColumnRef{Name: "le"}}}
	identity := &chplan.Lambda{Params: []string{"k", "v"}, Body: &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{&chplan.ColumnRef{Name: "k"}, &chplan.ColumnRef{Name: "v"}}}}
	cases := []struct {
		name string
		expr chplan.Expr
		bare string
		cast bool
	}{
		{"mapConcat", &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{attrs, le}}, "mapConcat(`Attributes`, map(?, `le`))", true},
		{"mapApply", &chplan.FuncCall{Fn: chplan.FnMapApply, Args: []chplan.Expr{identity, attrs}}, "mapApply((k, v) -> tuple(`k`, `v`), `Attributes`)", true},
		{"mapUpdate", &chplan.FuncCall{Fn: chplan.FnMapUpdate, Args: []chplan.Expr{attrs, le}}, "mapUpdate(`Attributes`, map(?, `le`))", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			render := func(s SchemaStrategies) string {
				sql, _ := Render(UnderSchemaStrategies(s, func(b *Builder) { _ = b.Expr(tc.expr) }))
				return sql
			}
			if got := render(SchemaStrategies{}); got != tc.bare {
				t.Errorf("zero strategies: rendered %q, want %q", got, tc.bare)
			}
			want := tc.bare
			if tc.cast {
				want = "CAST(" + tc.bare + ", toTypeName(" + tc.bare + "))"
			}
			if got := render(typeCastStrategies); got != want {
				t.Errorf("CanonicalMapTypeCast: rendered %q, want %q", got, want)
			}
		})
	}
}

// A canonical key over a mapConcat casts once: the key's own cast gives the
// column the parsed type, so its operand renders without a second cast.
func TestExprCanonicalMap_OperandCastOnce(t *testing.T) {
	t.Parallel()

	merged := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"}}}
	key := chplan.CanonicalAttributesExpr(merged)
	sql, _ := Render(UnderSchemaStrategies(typeCastStrategies, func(b *Builder) { _ = b.Expr(key) }))
	inner := "mapConcat(`Attributes`, `Extra`)"
	if want := "CAST(mapSort(" + inner + "), toTypeName(" + inner + "))"; sql != want {
		t.Errorf("rendered %q, want %q", sql, want)
	}
}
