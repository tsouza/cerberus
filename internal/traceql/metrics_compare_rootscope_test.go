package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerCompareInnerRootScoped pins the end-to-end gate that decides
// whether the emitter may window-prune the compare root-lookup scan: lowering
// must set MetricsCompare.InnerRootScoped iff the selection is confined to root
// spans. The emitter-side effect (the Timestamp bound) is pinned separately in
// chsql's TestEmitRangeWindowCompare_RootScopedEnrichmentTimestampBound; this
// test guards the reachability half — that the real traces-drilldown
// "Comparison" shape (`{ nestedSetParent < 0 }`) actually trips the gate, and
// that non-root selections do not (which would silently drop enrichment).
func TestLowerCompareInnerRootScoped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		query string
		want  bool
	}{
		{"{ nestedSetParent < 0 } | compare({ status = error })", true},          // drilldown Comparison shape
		{"{ } | compare({ status = error })", false},                             // all spans (root not guaranteed)
		{"{ span.http.status_code = 500 } | compare({ status = error })", false}, // non-root selection
	}
	for _, tc := range cases {
		expr, err := tempo.Parse(tc.query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", tc.query, err)
		}
		mc := findMetricsCompare(plan)
		if mc == nil {
			t.Fatalf("%q: no MetricsCompare in plan", tc.query)
			return
		}
		if mc.InnerRootScoped != tc.want {
			t.Errorf("%q: InnerRootScoped=%v, want %v", tc.query, mc.InnerRootScoped, tc.want)
		}
	}
}

// TestLowerCompareNonRootLookupWindow threads the existing trace_id_ts
// readiness gate into compare(). A non-root selection cannot use the request
// window for root enrichment: a matching child can be in-window while its root
// is arbitrarily older. The lookup instead supplies a per-trace timestamp
// envelope that contains both spans, allowing a physical bound without losing
// Tempo's rootName/rootServiceName values.
func TestLowerCompareNonRootLookupWindow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	s.TraceIDTsEnabled = true
	expr, err := tempo.Parse(`{ span.http.status_code = 500 } | compare({ status = error })`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	mc := findMetricsCompare(plan)
	if mc == nil {
		t.Fatal("no MetricsCompare in plan")
	}
	if mc.InnerRootScoped {
		t.Fatal("non-root selection must not be marked root-scoped")
	}
	if mc.RootLookupTraceIDTsTable != s.TraceIDTsTable ||
		mc.RootLookupTraceIDTsStartColumn != s.TraceIDTsStartColumn ||
		mc.RootLookupTraceIDTsEndColumn != s.TraceIDTsEndColumn {
		t.Errorf("trace_id_ts lookup = (%q, %q, %q), want (%q, %q, %q)",
			mc.RootLookupTraceIDTsTable, mc.RootLookupTraceIDTsStartColumn, mc.RootLookupTraceIDTsEndColumn,
			s.TraceIDTsTable, s.TraceIDTsStartColumn, s.TraceIDTsEndColumn)
	}
}

func findMetricsCompare(n chplan.Node) *chplan.MetricsCompare {
	var found *chplan.MetricsCompare
	chplan.Walk(n, func(x chplan.Node) bool {
		if mc, ok := x.(*chplan.MetricsCompare); ok {
			found = mc
			return false
		}
		return true
	})
	return found
}
