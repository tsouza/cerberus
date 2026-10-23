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
