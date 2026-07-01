package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// lowerCompare parses + lowers and unwraps the MetricsCompare node.
func lowerCompare(t *testing.T, q string) *chplan.MetricsCompare {
	t.Helper()
	expr, err := tempo.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower(%q): %v", q, err)
	}
	switch v := plan.(type) {
	case *chplan.MetricsCompare:
		return v
	case *chplan.Filter:
		if inner, ok := v.Input.(*chplan.MetricsCompare); ok {
			return inner
		}
	}
	t.Fatalf("Lower(%q): expected *chplan.MetricsCompare (or Filter wrap), got %T", q, plan)
	return nil
}

// TestLowerMetricsCompare_Defaults — `{} | compare({status = error})`
// lowers with the upstream default topN=10, no selection window, and
// the rootName/rootServiceName lookup join enabled on the default
// OTel-CH schema.
func TestLowerMetricsCompare_Defaults(t *testing.T) {
	t.Parallel()

	mc := lowerCompare(t, `{} | compare({status = error})`)
	if mc.TopN != 10 {
		t.Errorf("TopN = %d, want 10 (upstream default)", mc.TopN)
	}
	if mc.StartNs != 0 || mc.EndNs != 0 {
		t.Errorf("window = (%d, %d], want unset", mc.StartNs, mc.EndNs)
	}
	if mc.RootLookup == nil {
		t.Error("RootLookup = nil, want the per-trace root-span relation on the default schema")
	}
	if mc.TraceIDColumn != "TraceId" {
		t.Errorf("TraceIDColumn = %q, want TraceId", mc.TraceIDColumn)
	}
	bin, ok := mc.Selection.(*chplan.Binary)
	if !ok || bin.Op != chplan.OpEq {
		t.Fatalf("Selection = %#v, want Binary OpEq on StatusCode", mc.Selection)
	}
	lit, ok := bin.Right.(*chplan.LitString)
	if !ok || lit.V != "Error" {
		t.Errorf("Selection RHS = %#v, want LitString 'Error' (OTel-CH TitleCase)", bin.Right)
	}
	if mc.Pairs == nil {
		t.Fatal("Pairs = nil, want the span-attribute explosion expression")
	}
	if mc.Inner == nil {
		t.Fatal("Inner = nil, want the lowered spanset prefix")
	}
}

// TestLowerMetricsCompare_TopNAndWindow — the 4-arg form threads topN
// and ANDs the (start, end] unix-nanosecond window into the selection
// predicate (upstream isSelection: window first, then filter).
func TestLowerMetricsCompare_TopNAndWindow(t *testing.T) {
	t.Parallel()

	mc := lowerCompare(t, `{} | compare({status = error}, 5, 1100, 1300)`)
	if mc.TopN != 5 {
		t.Errorf("TopN = %d, want 5", mc.TopN)
	}
	if mc.StartNs != 1100 || mc.EndNs != 1300 {
		t.Errorf("window = (%d, %d], want (1100, 1300]", mc.StartNs, mc.EndNs)
	}
	outer, ok := mc.Selection.(*chplan.Binary)
	if !ok || outer.Op != chplan.OpAnd {
		t.Fatalf("Selection = %#v, want top-level AND(window, filter)", mc.Selection)
	}
	win, ok := outer.Left.(*chplan.Binary)
	if !ok || win.Op != chplan.OpAnd {
		t.Fatalf("Selection.Left = %#v, want AND(ts > start, ts <= end)", outer.Left)
	}
	lo, ok := win.Left.(*chplan.Binary)
	if !ok || lo.Op != chplan.OpGt {
		t.Fatalf("window lower bound = %#v, want OpGt (strict, upstream `spanStartTime > start`)", win.Left)
	}
	hi, ok := win.Right.(*chplan.Binary)
	if !ok || hi.Op != chplan.OpLe {
		t.Fatalf("window upper bound = %#v, want OpLe (inclusive, upstream `spanStartTime <= end`)", win.Right)
	}
}

// TestLowerMetricsCompare_DrilldownVerbatim — the exact query Grafana
// Traces Drilldown's Comparison tab issues (crawl signature
// traceql-metrics-compare-unsupported-422) parses, lowers, and emits
// SQL end to end.
func TestLowerMetricsCompare_DrilldownVerbatim(t *testing.T) {
	t.Parallel()

	const q = `{nestedSetParent<0 && true} | compare({status = error}, 10)`
	expr, err := tempo.Parse(q)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("EmitString: %v", err)
	}
	for _, want := range []string{
		"arrayJoin",    // attribute-pair explosion
		"is_selection", // cohort flag
		"LEFT JOIN",    // rootName / rootServiceName lookup
		"ParentSpanId", // root-span predicate inside the lookup
		"mapContains",  // well-known dedicated-attribute nil fallback
		"tupleElement", // attr / val projection
		"GROUP BY",     // per-(cohort, attr, val) counts
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q:\n%s", want, sql)
		}
	}
}

// TestLowerMetricsCompare_FoldsTrivialAndTrue — the Drilldown-appended
// `&& true` conjunct is folded out in lowering: the emitted selection
// predicate carries the meaningful `ParentSpanId = ”` (nestedSetParent<0)
// alone, not `... AND true`.
func TestLowerMetricsCompare_FoldsTrivialAndTrue(t *testing.T) {
	t.Parallel()

	const q = `{nestedSetParent<0 && true} | compare({status = error}, 10)`
	expr, err := tempo.Parse(q)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The folded inner scan filter must still be present (the conjunct is
	// dropped, not the whole predicate) and the trivial `AND true` gone.
	if !strings.Contains(sql, "ParentSpanId") {
		t.Errorf("emitted SQL lost the nestedSetParent<0 predicate:\n%s", sql)
	}
	if strings.Contains(sql, "AND true") || strings.Contains(sql, "AND TRUE") {
		t.Errorf("emitted SQL still carries a trivial `AND true` conjunct:\n%s", sql)
	}
}

// bareSelectionOp returns the top-level operator of the compare
// selection predicate. An un-windowed compare lowers `{status = error}`
// to a single `Binary{OpEq}`; a windowed compare wraps it as
// `Binary{OpAnd, window, filter}`. The op therefore distinguishes the
// two shapes.
func bareSelectionOp(t *testing.T, mc *chplan.MetricsCompare) chplan.BinaryOp {
	t.Helper()
	bin, ok := mc.Selection.(*chplan.Binary)
	if !ok {
		t.Fatalf("Selection = %#v, want *chplan.Binary", mc.Selection)
	}
	return bin.Op
}

// TestLowerMetricsCompare_ZeroStartNoWindow — a compare window whose start
// is exactly 0 is NOT a window: the `startNs > 0 && endNs > 0` guard is
// false (0 is not > 0), so the selection stays the bare `status = error`
// filter (top-level OpEq), never AND(window, filter).
//
// This pins the boundary + logical operators on that guard: turning the
// left `>` into `>=` (0 >= 0 is true), or the `&&` into `||`
// (false || true is true), would wrongly wrap the selection in the
// (start, end] window — flipping the top-level op from OpEq to OpAnd.
func TestLowerMetricsCompare_ZeroStartNoWindow(t *testing.T) {
	t.Parallel()

	mc := lowerCompare(t, `{} | compare({status = error}, 5, 0, 1300)`)
	if mc.StartNs != 0 || mc.EndNs != 1300 {
		t.Fatalf("window = (%d, %d], want (0, 1300]", mc.StartNs, mc.EndNs)
	}
	if op := bareSelectionOp(t, mc); op != chplan.OpEq {
		t.Errorf("Selection top-level op = %v, want OpEq (bare filter, no window ANDed for startNs=0)", op)
	}
}

// TestLowerMetricsCompare_ZeroEndNoWindow — the mirror of the start=0 case:
// an end of exactly 0 also fails the guard (0 is not > 0), so the
// selection stays the bare filter.
//
// Turning the right `>` into `>=` (0 >= 0 is true), or the `&&` into `||`
// (true || false is true), would wrongly add the window and flip the
// top-level op to OpAnd.
func TestLowerMetricsCompare_ZeroEndNoWindow(t *testing.T) {
	t.Parallel()

	mc := lowerCompare(t, `{} | compare({status = error}, 5, 1100, 0)`)
	if mc.StartNs != 1100 || mc.EndNs != 0 {
		t.Fatalf("window = (%d, %d], want (1100, 0]", mc.StartNs, mc.EndNs)
	}
	if op := bareSelectionOp(t, mc); op != chplan.OpEq {
		t.Errorf("Selection top-level op = %v, want OpEq (bare filter, no window ANDed for endNs=0)", op)
	}
}

// TestLowerMetricsCompare_PairsSkipRootWithoutColumns — blanking the
// parent-span-id column drops the root lookup (and the rootName /
// rootServiceName pairs) instead of failing the query.
func TestLowerMetricsCompare_PairsSkipRootWithoutColumns(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	s.ParentSpanIDColumn = ""
	expr, err := tempo.Parse(`{} | compare({status = error})`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	mc, ok := plan.(*chplan.MetricsCompare)
	if !ok {
		if f, fok := plan.(*chplan.Filter); fok {
			mc, ok = f.Input.(*chplan.MetricsCompare)
		}
		if !ok {
			t.Fatalf("expected MetricsCompare, got %T", plan)
		}
	}
	if mc.RootLookup != nil {
		t.Error("RootLookup should be nil when ParentSpanIDColumn is blank")
	}
	sql, _, err := chsql.Emit(context.Background(), mc)
	if err != nil {
		t.Fatalf("EmitString: %v", err)
	}
	if strings.Contains(sql, "__root_name") {
		t.Errorf("SQL must not reference __root_name without the lookup join:\n%s", sql)
	}
}
