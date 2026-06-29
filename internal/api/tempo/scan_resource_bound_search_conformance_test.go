package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// This is the layer-gap regression test for the spans-scan resource-bound
// chokepoint: it drives real TraceQL search queries through the REAL handler
// pipeline (parse -> lower -> ProjectSamples -> optimize -> emit) with a window
// + search limit, exactly as handleSearch does, and asserts NO query trips the
// chokepoint (chplan.ScanResourceBoundViolation). compose-smoke caught a
// false-rejection here that the unit suite missed; this test closes that gap.
//
// Root cause it guards: the trace-scoped / per-event intrinsics OTel-CH does
// not materialise (rootName / rootServiceName / traceDuration / span:childCount
// / event:timeSinceStart / instrumentation-scoped attributes) lower to a
// StaticNil constant-false predicate (matching reference Tempo's empty result),
// which ConstantFold collapses `false AND <window>` to a bare `false`. A
// `WHERE false` scan reads zero rows — the chokepoint must classify it bounded.

func searchEngineErr(t *testing.T, h *Handler, query string, start, end time.Time) error {
	t.Helper()
	ctx := tql.WithSearchTraceLimit(context.Background(), DefaultSearchLimit)
	ctx = tql.WithSearchWindow(ctx, start, end)
	_, err := h.Engine.Query(ctx, h.Lang(), query)
	return err
}

func assertNoUnboundedSpansScan(t *testing.T, h *Handler, query string, start, end time.Time) {
	t.Helper()
	err := searchEngineErr(t, h, query, start, end)
	var v *chplan.ScanResourceBoundViolation
	if errors.As(err, &v) {
		t.Errorf("search query must not trip the spans-scan chokepoint (500): %s\n  %v", query, err)
	}
}

// TestSearchTraceScopedIntrinsicsBounded pins the exact family compose-smoke
// caught: trace-scoped / aggregate intrinsics whose span-level lowering is
// constant-false must lower+emit without a ScanResourceBoundViolation.
func TestSearchTraceScopedIntrinsicsBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	h := New(&capturingQuerier{}, s, "v-test", nil)
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	family := []string{
		`{ rootName = "GET /api/checkout" }`,
		`{ rootServiceName = "checkout" }`,
		`{ traceDuration > 100ms }`,
		`{ trace:rootName = "x" }`,
		`{ trace:rootService = "x" }`,
		`{ trace:duration > 100ms }`,
		`{ span:childCount > 0 }`,
		`{ event:timeSinceStart > 1ms }`,
		`{ instrumentation:name = "otel" }`,
		`{ instrumentation:version = "1.0" }`,
		`{ resource.service.name != nil }`,
		`{ .never.set.attr = nil }`,
	}
	for _, q := range family {
		assertNoUnboundedSpansScan(t, h, q, start, end)
	}
}

// TestShowcaseTraceQLSearchAllBounded drives every parseable query in the
// showcase-traceql compose dashboard through the search pipeline and asserts
// none trips the chokepoint — so a future dashboard addition that hits a new
// unbounded shape fails here, not only in compose-smoke.
func TestShowcaseTraceQLSearchAllBounded(t *testing.T) {
	t.Parallel()
	const dashboard = "../../../test/e2e/grafana/compose/dashboards/showcase-traceql.json"
	raw, err := os.ReadFile(dashboard)
	if err != nil {
		t.Fatalf("read showcase dashboard: %v", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse showcase dashboard: %v", err)
	}
	queries := map[string]struct{}{}
	collectDashboardQueries(doc, queries)
	sorted := make([]string, 0, len(queries))
	for q := range queries {
		sorted = append(sorted, q)
	}
	sort.Strings(sorted)
	if len(sorted) == 0 {
		t.Fatal("no queries extracted from showcase dashboard — extraction broke")
	}

	s := tracesSchema()
	h := New(&capturingQuerier{}, s, "v-test", nil)
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	checked := 0
	for _, q := range sorted {
		if _, perr := ast.Parse(q); perr != nil {
			continue // templated / truncated panel exprs are not real queries
		}
		checked++
		assertNoUnboundedSpansScan(t, h, q, start, end)
	}
	t.Logf("showcase-traceql: %d parseable queries, all pass the chokepoint", checked)
}

func collectDashboardQueries(o any, out map[string]struct{}) {
	switch v := o.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "query" || k == "expr" || k == "queryText" {
				if s, ok := val.(string); ok && s != "" {
					out[s] = struct{}{}
				}
			}
			collectDashboardQueries(val, out)
		}
	case []any:
		for _, x := range v {
			collectDashboardQueries(x, out)
		}
	}
}
