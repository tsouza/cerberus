package chsql

import (
	"context"
	"strings"
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

// logShapedPlan builds the plan of a parser-stage log query: a scan, a
// row layer that defines the labels map L with mapConcat, a Filter on it,
// the narrow Project the sort reads, ORDER BY … LIMIT, and above the limit
// a second definition of L and the Attributes output that merges it again.
// It returns the plan and the three mapConcat calls (below the limit, above
// it, and the output).
func logShapedPlan() (chplan.Node, []*chplan.FuncCall) {
	merge := func(a, b chplan.Expr) *chplan.FuncCall {
		return &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{a, b}}
	}
	col := func(name string) *chplan.ColumnRef { return &chplan.ColumnRef{Name: name} }
	below := merge(col("ResourceAttributes"), col("Extracted"))
	above := merge(col("ResourceAttributes"), col("Extracted"))
	output := merge(col("_logql_labels_1"), col("Level"))
	layer := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_logs"},
		Projections: []chplan.Projection{
			{Expr: col("Body")},
			{Expr: col("ResourceAttributes")},
			{Expr: col("Extracted")},
			{Expr: col("Level")},
			{Expr: col("Timestamp")},
			{Expr: below, Alias: "_logql_labels_1"},
		},
	}
	filtered := &chplan.Filter{Input: layer, Predicate: &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.MapAccess{Map: col("_logql_labels_1"), Key: &chplan.LitString{V: "query_type"}},
		Right: &chplan.LitString{V: "SELECT"},
	}}
	narrow := &chplan.Project{Input: filtered, Projections: []chplan.Projection{
		{Expr: col("Body")},
		{Expr: col("ResourceAttributes")},
		{Expr: col("Extracted")},
		{Expr: col("Level")},
		{Expr: col("Timestamp")},
	}}
	limited := &chplan.Limit{Count: 1000, Input: &chplan.OrderBy{
		Input: narrow, Keys: []chplan.OrderKey{{Expr: col("Timestamp"), Desc: true}},
	}}
	again := &chplan.Project{Input: limited, Projections: []chplan.Projection{
		{Expr: col("Body")},
		{Expr: col("Level")},
		{Expr: col("Timestamp")},
		{Expr: above, Alias: "_logql_labels_1"},
	}}
	top := &chplan.Project{Input: again, Projections: []chplan.Projection{
		{Expr: col("Body"), Alias: "Line"},
		{Expr: output, Alias: "Attributes"},
		{Expr: col("Timestamp"), Alias: "TimeUnix"},
	}}
	return top, []*chplan.FuncCall{below, above, output}
}

// A Map a Filter consumes, or one only Projects and a Limit carry to the
// result, reaches no operator that spills: the sort reads plain columns.
func TestMapTypeCastFree_LogQueryMapsAreFree(t *testing.T) {
	t.Parallel()
	plan, calls := logShapedPlan()
	free := mapTypeCastFree(plan)
	for i, f := range calls {
		if !free[f] {
			t.Errorf("mapConcat %d is cast; nothing that spills reads it", i)
		}
	}
}

// A Map an Aggregate reads, or one an OrderBy carries, is owed: its
// spilled blocks are read back. A node other than a Filter, a Limit or a
// Project owes every column of its input.
func TestMapTypeCastFree_SpillingConsumersOweTheMap(t *testing.T) {
	t.Parallel()
	attrs := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
	}}
	unread := &chplan.FuncCall{Fn: chplan.FnMapApply, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{"k", "v"}, Body: &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
			&chplan.ColumnRef{Name: "k"}, &chplan.ColumnRef{Name: "v"},
		}}},
		&chplan.ColumnRef{Name: "Attributes"},
	}}
	merged := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: attrs, Alias: "Attributes"},
			{Expr: unread, Alias: "Unread"},
			{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
	}
	grouped := &chplan.Aggregate{
		Input:          merged,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases: []string{"Attributes"},
		AggFuncs:       []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"}},
	}
	if free := mapTypeCastFree(grouped); free[attrs] || free[unread] {
		t.Errorf("a column an Aggregate's input carries renders uncast: %v", free)
	}

	sorted := &chplan.OrderBy{Input: merged, Keys: []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Value"}}}}
	if free := mapTypeCastFree(sorted); free[attrs] || free[unread] {
		t.Errorf("a column a sort carries renders uncast: %v", free)
	}
}

// One Map call shared by a free projection and a GROUP BY key is cast:
// a call is free only when every place it renders is.
func TestMapTypeCastFree_SharedCallFollowsItsOwedUse(t *testing.T) {
	t.Parallel()
	shared := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
	}}
	plan := &chplan.Project{
		Input: &chplan.Aggregate{
			Input:          &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy:        []chplan.Expr{shared},
			GroupByAliases: []string{"Attributes"},
		},
		Projections: []chplan.Projection{{Expr: shared, Alias: "Again"}},
	}
	if mapTypeCastFree(plan)[shared] {
		t.Errorf("a mapConcat a GROUP BY key also renders is free")
	}
}

// Under the strategy, an emission renders the log query's Map calls bare,
// and a grouped Map cast.
func TestEmit_MapTypeCastOnlyWhereItCanSpill(t *testing.T) {
	t.Parallel()
	ctx := WithSchemaStrategies(context.Background(), typeCastStrategies)
	plan, _ := logShapedPlan()
	sql, _, err := Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit log plan: %v", err)
	}
	if strings.Contains(sql, "toTypeName") {
		t.Errorf("log plan renders a type cast:\n%s", sql)
	}

	grouped := &chplan.Aggregate{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
				}}, Alias: "Attributes"},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		GroupBy:        []chplan.Expr{chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"})},
		GroupByAliases: []string{"Attributes"},
		AggFuncs:       []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"}},
	}
	sql, _, err = Emit(ctx, grouped)
	if err != nil {
		t.Fatalf("emit grouped plan: %v", err)
	}
	for _, want := range []string{
		"CAST(mapConcat(`Attributes`, `Extra`), toTypeName(mapConcat(`Attributes`, `Extra`)))",
		"CAST(mapSort(`Attributes`), toTypeName(`Attributes`))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("grouped plan lacks %s:\n%s", want, sql)
		}
	}
}

// A Project that computes a window function sorts its rows: every Map it
// carries or partitions by is owed, even when nothing above reads it.
func TestMapTypeCastFree_WindowProjectOwesItsMaps(t *testing.T) {
	t.Parallel()
	attrs := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
	}}
	below := &chplan.Project{
		Input:       &chplan.Scan{Table: "otel_metrics_histogram"},
		Projections: []chplan.Projection{{Expr: attrs, Alias: "Attributes"}, {Expr: &chplan.ColumnRef{Name: "Value"}}},
	}
	window := &chplan.Project{Input: below, Projections: []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: "Value"}},
		{Expr: &chplan.WindowExpr{Fn: chplan.FnCount, PartitionBy: []chplan.Expr{
			chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}),
		}}, Alias: "n"},
	}}
	filterKey := chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Value"}).(*chplan.FuncCall)
	top := &chplan.Project{
		Input: &chplan.Filter{Input: window, Predicate: &chplan.Binary{
			Op: chplan.OpEq, Left: filterKey, Right: &chplan.ColumnRef{Name: "Value"},
		}},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Value"}}},
	}
	free := mapTypeCastFree(top)
	if !free[filterKey] {
		t.Errorf("a canonical key a Filter above the window consumes is cast")
	}
	if free[attrs] {
		t.Errorf("a Map a window partitions by renders uncast")
	}
	for f := range free {
		if f.Fn == chplan.FnMapSort && f != filterKey {
			t.Errorf("the window's PARTITION BY key renders uncast")
		}
	}
}

// A canonical key in a projection no operator reads is free like any Map
// call; an unaliased computed projection is owed whenever any column of its
// Project is, here the one a sort above carries.
func TestMapTypeCastFree_UnnamedAndKeyProjections(t *testing.T) {
	t.Parallel()
	key := chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}).(*chplan.FuncCall)
	unnamed := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
	}}
	inner := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_logs"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: key, Alias: "Key"},
			{Expr: unnamed},
		},
	}
	free := mapTypeCastFree(inner)
	if !free[key] || !free[unnamed] {
		t.Errorf("projections of the statement's result are cast: key free %v, unnamed free %v", free[key], free[unnamed])
	}
	reader := &chplan.OrderBy{
		Input: &chplan.Project{Input: inner, Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Body"}}}},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Body"}}},
	}
	free = mapTypeCastFree(reader)
	if !free[key] {
		t.Errorf("a key projection nothing above reads is cast")
	}
	if free[unnamed] {
		t.Errorf("an unnamed computed projection renders uncast while its Project's columns are read")
	}
}

// A node two Projects read owes the union of what they read, and every
// column once a spilling node reads it directly too.
func TestMapTypeCastFree_SharedNodeOwesTheUnion(t *testing.T) {
	t.Parallel()
	col := func(name string) *chplan.ColumnRef { return &chplan.ColumnRef{Name: name} }
	merged := &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{col("Attributes"), col("Extra")}}
	shared := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_logs"},
		Projections: []chplan.Projection{
			{Expr: col("A")},
			{Expr: col("B")},
			{Expr: col("C")},
			{Expr: merged, Alias: "M"},
		},
	}
	readA := &chplan.Project{Input: shared, Projections: []chplan.Projection{{Expr: col("A")}}}
	readBC := &chplan.Project{Input: shared, Projections: []chplan.Projection{{Expr: col("B")}, {Expr: col("C")}}}
	if free := mapTypeCastFree(&chplan.UnionAll{Inputs: []chplan.Node{readA, readBC}}); !free[merged] {
		t.Errorf("a Map neither reader reads is cast")
	}
	readM := &chplan.Project{Input: shared, Projections: []chplan.Projection{{Expr: col("M")}}}
	if free := mapTypeCastFree(&chplan.UnionAll{Inputs: []chplan.Node{readA, readBC, readM}}); free[merged] {
		t.Errorf("a Map one of three readers reads renders uncast")
	}
	if free := mapTypeCastFree(&chplan.UnionAll{Inputs: []chplan.Node{readA, readBC, shared}}); free[merged] {
		t.Errorf("a Map the UnionAll reads directly renders uncast")
	}
}

// An operand of a canonical key that fails to render fails the key.
func TestExprCanonicalMap_OperandErrorSurfaces(t *testing.T) {
	t.Parallel()
	key := chplan.CanonicalAttributesExpr(&chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnMapSort, Args: []chplan.Expr{
			&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "Extra"},
		}},
		&chplan.ColumnRef{Name: "Extra"},
	}})
	for _, strategies := range []SchemaStrategies{{}, typeCastStrategies} {
		b := &Builder{strategies: strategies}
		if err := b.Expr(key); err == nil {
			t.Errorf("strategies %+v: a key over an unrenderable operand renders without error: %s", strategies, b.sb.String())
		}
	}
}
