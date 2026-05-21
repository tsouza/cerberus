package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramBucket_RoutesToHistogramTable pins the classic-
// histogram `_bucket` companion fan-out at the lowering layer.
//
// PR #645 routed `_count` / `_sum` companions to the histogram table.
// `_bucket` carries different semantics — the data lives in the
// `BucketCounts` × `ExplicitBounds` arrays on the same row — so the
// rewrite arrayJoin's over the array to emit one Sample-shape row per
// bucket boundary, with a synthesized `le=<bound>` label baked into
// the Attributes map and a cumulative count as the Value.
//
// Until this PR a bare `<X>_bucket` selector against the OTel-CH layout
// returned zero series: the suffixed name had no rows on either the
// gauge or sum tables, and `/api/v1/query_exemplars`-style routing was
// scoped to the `_count` / `_sum` shapes. This regression broke every
// classic-histogram bucket panel that Grafana renders against cerberus.
//
// The assertions check the structural pieces of the rewrite — Scan
// targets the histogram table, MetricName matcher resolves against the
// bare base name, and the fan-out Project carries the arrayJoin / mapConcat /
// arraySlice chain that turns one histogram row into N+1 Sample rows.
func TestLower_HistogramBucket_RoutesToHistogramTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name  string
		query string
	}{
		{name: "bare", query: `http_server_request_duration_bucket`},
		{name: "with_le_filter", query: `http_server_request_duration_bucket{le="0.5"}`},
		{name: "rate_wrapped", query: `rate(http_server_request_duration_bucket[5m])`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			scans := collectScans(plan)
			if len(scans) != 1 {
				t.Fatalf("want 1 Scan, got %d", len(scans))
			}
			if scans[0].Table != s.HistogramTable {
				t.Fatalf("Scan.Table = %q; want %q (histogram table)",
					scans[0].Table, s.HistogramTable)
			}

			// The bare metric name (suffix stripped) must drive the
			// scan-side MetricName filter. The suffixed name must NOT
			// appear in any predicate.
			if !planReferencesMetricName(plan, "http_server_request_duration") {
				t.Fatalf("plan does not filter on bare MetricName='http_server_request_duration'")
			}
			if planReferencesMetricName(plan, "http_server_request_duration_bucket") {
				t.Fatalf("plan still references the `_bucket`-suffixed MetricName — rewrite did not strip the suffix")
			}

			// Emit SQL and assert the fan-out pieces are present.
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for _, want := range []string{
				"arrayJoin(arrayEnumerate(`BucketCounts`))",
				"mapConcat(`Attributes`, map(",
				"arraySlice(`BucketCounts`",
				"if((le_idx > length(`ExplicitBounds`))",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("emitted SQL missing fan-out fragment %q\nSQL: %s", want, sql)
				}
			}
		})
	}
}

// TestLower_HistogramBucket_NoHistogramTable verifies the fan-out is a
// no-op when the schema disables the histogram table. Mirrors the
// `_count` / `_sum` companion fallback in
// TestLower_HistogramCompanion_NoHistogramTable.
func TestLower_HistogramBucket_NoHistogramTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.HistogramTable = ""
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`rate(http_server_request_duration_bucket[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	scans := collectScans(plan)
	if len(scans) != 1 {
		t.Fatalf("want 1 Scan, got %d", len(scans))
	}
	// With no histogram table, the `_bucket`-suffixed name falls
	// through to the sum-table routing — TableFor treats `_bucket` as
	// a counter-shape suffix. The Scan must NOT target the (now
	// empty) histogram table.
	if scans[0].Table == "" {
		t.Fatalf("Scan.Table is empty — fallback routing did not pick the sum table")
	}
	// Suffixed name stays as-is on the fallback path; there's no
	// fan-out, and no Project wrapping the Scan with the bucket-fanout
	// arrayJoin shape.
	if !planReferencesMetricName(plan, "http_server_request_duration_bucket") {
		t.Fatalf("fallback plan should still reference the `_bucket`-suffixed MetricName")
	}
}

// TestLower_HistogramBucket_EmittedShape_CumulativeAndPlusInf pins the
// emitted plan's bucket fan-out expressions structurally — the `le`
// label is built from `if(le_idx > length(ExplicitBounds), '+Inf',
// toString(ExplicitBounds[le_idx]))` and the cumulative count is
// `toFloat64(arraySum(arraySlice(BucketCounts, 1, le_idx)))`. The
// Prom wire-shape contract for `_bucket` series requires both: the
// trailing `+Inf` bucket, and CUMULATIVE counts (per Prom's
// classic-histogram convention) — OTel-CH stores per-bucket counts.
func TestLower_HistogramBucket_EmittedShape_CumulativeAndPlusInf(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`http_server_request_duration_bucket`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Pin the precise CH idiom for `+Inf` synthesis on the trailing
	// bucket — the only mechanism that lets a Prom client read the
	// histogram's total observation count through `_bucket{le="+Inf"}`.
	want := "if((le_idx > length(`ExplicitBounds`)), ?, toString(`ExplicitBounds`[le_idx]))"
	if !strings.Contains(sql, want) {
		t.Errorf("emitted SQL missing +Inf branch %q\nSQL: %s", want, sql)
	}
	// Pin the cumulative-count Value expression. `arraySlice(arr, 1, n)`
	// returns the first n elements; `arraySum` of that gives the
	// cumulative count for observations with value ≤ the bucket's upper
	// edge — matching Prom's `_bucket{le=X}` cumulative-counter shape.
	if !strings.Contains(sql, "toFloat64(arraySum(arraySlice(`BucketCounts`, ?, le_idx)))") {
		t.Errorf("emitted SQL missing cumulative-count expression\nSQL: %s", sql)
	}
}

// TestLower_HistogramBucket_PlanShape walks the produced plan tree and
// asserts the structural layering — Scan → Filter → fan-out Project →
// canonical Sample Project. The chplan IR snapshot for the txtar
// fixture pins the same shape; this Go test pins it at the unit-test
// layer so a refactor in lower.go that drops the fan-out shows up
// immediately.
func TestLower_HistogramBucket_PlanShape(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`rate(http_server_request_duration_bucket[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	// Find the canonical (outer) bucket-fanout Project — its
	// MetricName projection is a LitString with the `_bucket` suffix.
	var canonical *chplan.Project
	var walk func(n chplan.Node)
	walk = func(n chplan.Node) {
		if n == nil {
			return
		}
		if p, ok := n.(*chplan.Project); ok {
			for _, proj := range p.Projections {
				if proj.Alias == s.MetricNameColumn {
					if lit, ok := proj.Expr.(*chplan.LitString); ok && lit.V == "http_server_request_duration_bucket" {
						canonical = p
						return
					}
				}
			}
		}
		for _, c := range n.Children() {
			if canonical != nil {
				return
			}
			walk(c)
		}
	}
	walk(plan)
	if canonical == nil {
		t.Fatal("plan has no Project that aliases the `_bucket` suffixed name on MetricName")
	}

	// The canonical Project's Input must itself be a Project (the
	// arrayJoin fan-out layer). That inner Project must reference
	// `arrayJoin(arrayEnumerate(BucketCounts))` aliased as `le_idx`.
	inner, ok := canonical.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("canonical Project Input is %T; want *chplan.Project (the fan-out layer)", canonical.Input)
	}
	found := false
	for _, proj := range inner.Projections {
		if proj.Alias != bucketIdxAlias {
			continue
		}
		fc, ok := proj.Expr.(*chplan.FuncCall)
		if !ok || fc.Name != "arrayJoin" {
			continue
		}
		if len(fc.Args) != 1 {
			continue
		}
		inner2, ok := fc.Args[0].(*chplan.FuncCall)
		if !ok || inner2.Name != "arrayEnumerate" {
			continue
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("fan-out Project has no arrayJoin(arrayEnumerate(BucketCounts)) AS le_idx projection: %+v", inner.Projections)
	}
}
