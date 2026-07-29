package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// canonicalAttributesExpr establishes an invariant the whole engine leans
// on, so the tests below assert it over a whole lowered plan rather than
// at the one call site. Everything downstream — the LWR staleness
// collapse, a RangeWindow's partition, `without(...)`, `topk`'s
// PARTITION BY, the vector-join arms — keys series identity on the
// Attributes Map, and a CH Map compares positionally over its (keys,
// values) arrays. One un-canonicalised binding anywhere on the read path
// is enough to split a logical series in two.

// TestCanonicalAttributesExpr_WrapsInMapSort pins the helper's shape: the
// emitters and the `mapSort` name are what make the invariant real, so a
// rename or an accidental pass-through must fail here first.
func TestCanonicalAttributesExpr_WrapsInMapSort(t *testing.T) {
	t.Parallel()
	inner := &chplan.ColumnRef{Name: "Attributes"}
	got, ok := canonicalAttributesExpr(inner).(*chplan.FuncCall)
	if !ok {
		t.Fatalf("got %T, want *chplan.FuncCall", canonicalAttributesExpr(inner))
	}
	if got.Name != "mapSort" {
		t.Fatalf("func name: got %q, want %q", got.Name, "mapSort")
	}
	if len(got.Args) != 1 || got.Args[0] != chplan.Expr(inner) {
		t.Fatalf("args: got %#v, want exactly the input expr", got.Args)
	}
}

// TestSelectorAttributesExpr_AlwaysCanonicalOrBare pins the two lowering
// entry points that bind the Attributes column. Either they canonicalise,
// or they return the bare ColumnRef — the catalog / pre-merged cases,
// where the value was already canonicalised upstream. What must never
// happen is a merged-but-unsorted map, which reads as a valid label set
// while carrying whatever key order the store handed back.
func TestSelectorAttributesExpr_AlwaysCanonicalOrBare(t *testing.T) {
	t.Parallel()

	custom := schema.DefaultOTelMetrics()
	custom.ResourceAttributesColumn = ""
	custom.ServiceNameColumn = ""

	cases := []struct {
		name   string
		s      schema.Metrics
		ctx    lowerCtx
		isBare bool
	}{
		{name: "default_schema", s: schema.DefaultOTelMetrics()},
		{name: "resource_merge_opted_out", s: custom},
		{
			name:   "catalog_mode",
			s:      schema.DefaultOTelMetrics(),
			ctx:    lowerCtx{catalog: &metadataCatalog{}},
			isBare: true,
		},
		{
			name:   "pre_merged_by_arms",
			s:      schema.DefaultOTelMetrics(),
			ctx:    lowerCtx{attributesPreMerged: true},
			isBare: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectorAttributesExpr(tc.ctx, tc.s)
			if tc.isBare {
				if !isBareAttributesRef(got, tc.s) {
					t.Fatalf("got %#v, want the bare %s ColumnRef", got, tc.s.AttributesColumn)
				}
				return
			}
			call, ok := got.(*chplan.FuncCall)
			if !ok || call.Name != "mapSort" {
				t.Fatalf("got %#v, want a mapSort(...) wrap", got)
			}
		})
	}
}

// TestLoweredPlans_AttributesBindingsAreCanonical walks the plan for a
// corpus spanning every shape that keys on the Attributes Map and asserts
// that no projection binds the Attributes alias to an expression that
// reads the raw column without sorting it.
//
// The corpus is the point: the fix lives at one projection precisely so
// that adding a new operator cannot reintroduce the split, and a
// whole-plan walk is what makes that claim checkable.
func TestLoweredPlans_AttributesBindingsAreCanonical(t *testing.T) {
	t.Parallel()

	queries := []string{
		"mko",
		"count(mko)",
		"sum(mko)",
		"sum by (job, dc) (mko)",
		"sum without (dc) (mko)",
		"count by (job) (mko)",
		"topk(3, mko)",
		"increase(mko[5m])",
		"rate(mko[5m])",
		"max_over_time(mko[5m])",
		"avg_over_time(mko[5m])",
		"absent_over_time(mko[5m])",
		`label_replace(mko, "dc", "eu", "", "")`,
		"mko / ignoring(dc) mko_b",
		"mko unless mko_b",
		// The histogram family groups straight off the histogram tables
		// with no selector projection in between, so each of these binds
		// series identity at its own group key. Both storage shapes
		// (classic `_bucket` rows and native exponential rows), both
		// evaluation modes, and the `sum by/without` wrapper that routes
		// through histogramAggGroupBy are represented.
		"histogram_quantile(0.9, mko_hist_bucket)",
		"histogram_quantile(0.9, rate(mko_hist_bucket[5m]))",
		"histogram_quantile(0.9, sum by (le) (rate(mko_hist_bucket[5m])))",
		"histogram_quantile(0.9, sum without (dc) (rate(mko_hist_bucket[5m])))",
		"histogram_quantile(0.9, sum (rate(mko_hist_bucket[5m])))",
		"histogram_quantile(0.9, mko_exp_hist)",
		"histogram_quantile(0.9, rate(mko_exp_hist[5m]))",
		"histogram_count(mko_exp_hist)",
		"histogram_sum(mko_exp_hist)",
		"histogram_avg(mko_exp_hist)",
		"histogram_fraction(0.2, 0.8, mko_exp_hist)",
		`histogram_quantiles(mko_hist_bucket, "phi", 0.5, 0.9)`,
		`histogram_quantiles(mko_hist_bucket, "phi", 0.5)`,
		"mko_hist_bucket",
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	// Instant and range mode take different lowering paths for the whole
	// histogram family (Aggregate collapse vs RangeBucketFanout), so both
	// have to be swept or half the binding sites go unvisited.
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := end.Add(-canonicalCorpusRange)
	for _, mode := range []struct {
		name  string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{"instant", func(e parser.Expr) (chplan.Node, error) {
			return LowerAt(context.Background(), e, s, end, end)
		}},
		{"range", func(e parser.Expr) (chplan.Node, error) {
			return LowerAtRange(context.Background(), e, s, start, end, canonicalCorpusStep)
		}},
	} {
		for _, q := range queries {
			t.Run(mode.name+"/"+q, func(t *testing.T) {
				expr, err := p.ParseExpr(q)
				if err != nil {
					t.Fatalf("parse %q: %v", q, err)
				}
				plan, err := mode.lower(expr)
				if err != nil {
					t.Fatalf("lower %q: %v", q, err)
				}
				assertAttributesBindingsCanonical(t, plan, s)
			})
		}
	}
}

// Eval window for the range-mode corpus sweep. Any window works — the
// invariant is shape, not data — so these are just small round numbers
// that keep the fan-out cheap.
const (
	canonicalCorpusRange = 10 * time.Minute
	canonicalCorpusStep  = time.Minute
)

// assertAttributesBindingsCanonical fails with the offending node for
// every site in plan that binds series identity to an Attributes Map the
// engine has not canonicalised.
func assertAttributesBindingsCanonical(t *testing.T, plan chplan.Node, s schema.Metrics) {
	t.Helper()
	for _, bad := range uncanonicalAttributesBindings(plan, s) {
		t.Errorf("%T binds an un-canonicalised Attributes map: %#v", bad, bad)
	}
}

// uncanonicalAttributesBindings returns every node in plan whose own
// Attributes binding reads the map without canonicalising it and without
// inheriting a canonical one from its input.
//
// Two kinds of site bind Attributes. A Project aliases an expression to
// the Attributes column — that is what the selector paths use. A grouping
// operator (Aggregate, the histogram fan-out, the two histogram-quantile
// nodes) names Attributes in GroupByAliases, which makes its group key the
// binding — that is what the histogram paths use, because they group
// straight off the scan with no selector projection in between.
//
// A binding is fine when its expression sorts every read of the column, or
// when it merely passes through an input that is already canonical; the
// recursion is what tells those two apart, and is why a bare `Attributes`
// group key over a raw Scan is caught while the identical key over an
// already-canonical subplan is not.
func uncanonicalAttributesBindings(plan chplan.Node, s schema.Metrics) []chplan.Node {
	var bad []chplan.Node
	canonicalAttributesOut(plan, s, &bad)
	return bad
}

// canonicalAttributesOut reports whether n's output Attributes column is
// canonical, appending to bad any node that breaks the invariant.
func canonicalAttributesOut(n chplan.Node, s schema.Metrics, bad *[]chplan.Node) bool {
	if n == nil {
		return true
	}
	// A Scan hands back the stored key order verbatim: the OTel-CH
	// exporter preserves OTLP wire order and never sorts.
	if _, isScan := n.(*chplan.Scan); isScan {
		return false
	}

	inputsCanonical := true
	for _, c := range n.Children() {
		if !canonicalAttributesOut(c, s, bad) {
			inputsCanonical = false
		}
	}

	bindings := attributesBindings(n, s)
	if len(bindings) == 0 {
		return inputsCanonical
	}
	for _, b := range bindings {
		// A binding is acceptable when it sorts every read of the column
		// itself, or when it merely passes through an input that is
		// already canonical.
		if !readsAttributesUnderMapSort(b, s.AttributesColumn, false) && !inputsCanonical {
			*bad = append(*bad, n)
			return false
		}
	}
	return true
}

// attributesBindings returns the expressions through which n establishes
// series identity from the Attributes map: the projection aliased to the
// column, plus every group key that carries the whole map.
//
// Group keys are matched on what they READ, not on their alias — the
// histogram `sum by/without` paths alias theirs to `gkey_0` and rebuild
// Attributes from that alias one node up, so an alias-only match would
// walk straight past the binding that actually decides the grouping.
func attributesBindings(n chplan.Node, s schema.Metrics) []chplan.Expr {
	col := s.AttributesColumn
	switch v := n.(type) {
	case *chplan.Project:
		for _, p := range v.Projections {
			if p.Alias == col {
				return []chplan.Expr{p.Expr}
			}
		}
	case *chplan.Aggregate:
		return wholeMapGroupKeys(v.GroupBy, col)
	case *chplan.RangeBucketFanout:
		return wholeMapGroupKeys(v.GroupBy, col)
	case *chplan.HistogramQuantile:
		return wholeMapGroupKeys(v.GroupBy, col)
	case *chplan.HistogramQuantileNative:
		return wholeMapGroupKeys(v.GroupBy, col)
	}
	return nil
}

// wholeMapGroupKeys picks the group keys that carry whole-Map series
// identity.
func wholeMapGroupKeys(keys []chplan.Expr, col string) []chplan.Expr {
	var out []chplan.Expr
	for _, k := range keys {
		if carriesWholeMap(k, col) {
			out = append(out, k)
		}
	}
	return out
}

// carriesWholeMap reports whether expr reads col as a Map rather than
// through a subscript. A subscript (`Attributes['job']`, what `by(...)`
// lowers to) yields a scalar String that compares the same under any key
// order, so it is not an ordering hazard.
func carriesWholeMap(expr chplan.Expr, col string) bool {
	switch v := expr.(type) {
	case *chplan.ColumnRef:
		return v.Name == col
	case *chplan.MapAccess:
		return false
	case *chplan.MapWithoutKeys:
		return carriesWholeMap(v.Map, col)
	case *chplan.FuncCall:
		for _, a := range v.Args {
			if carriesWholeMap(a, col) {
				return true
			}
		}
	}
	return false
}

// readsAttributesUnderMapSort reports whether every read of column inside
// expr sits below a `mapSort`. sorted carries whether an enclosing
// mapSort has already been seen on the current path.
func readsAttributesUnderMapSort(expr chplan.Expr, column string, sorted bool) bool {
	switch v := expr.(type) {
	case *chplan.ColumnRef:
		return sorted || v.Name != column
	case *chplan.MapWithoutKeys:
		// Key removal preserves the order it was handed, so it is
		// canonical only if what it reads already was.
		return readsAttributesUnderMapSort(v.Map, column, sorted)
	case *chplan.FuncCall:
		if v.Name == "mapSort" {
			sorted = true
		}
		for _, a := range v.Args {
			if !readsAttributesUnderMapSort(a, column, sorted) {
				return false
			}
		}
		return true
	default:
		// Anything else cannot itself be the Attributes read; the
		// canonicalisation always roots the binding, so a non-call root
		// that is not a ColumnRef carries no raw map to sort.
		return true
	}
}
