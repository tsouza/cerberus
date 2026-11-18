package promql

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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
				"arrayZip(arrayMap(le_idx -> ",
				"arrayEnumerate(`BucketCounts`)",
				"mapConcat(`Attributes`, map(",
				"arrayCumSum(`BucketCounts`)",
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
// <Prometheus spelling of ExplicitBounds[le_idx]>)` and the cumulative count from
// `arrayMap(c -> toFloat64(c), arrayCumSum(BucketCounts))`. The
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
	// The branch guard and the bound it reads are pinned separately
	// because the finite arm is no longer a bare `toString`: it is
	// promFixedFloatStringExpr, whose spelling matches Prometheus's own
	// OTLP `le` stamping and is pinned end-to-end against
	// strconv.FormatFloat by TestPromFixedFloatLabelsMatchGo.
	want := "if((le_idx > length(`ExplicitBounds`)), ?, "
	if !strings.Contains(sql, want) {
		t.Errorf("emitted SQL missing +Inf branch %q\nSQL: %s", want, sql)
	}
	if !strings.Contains(sql, "array(`ExplicitBounds`[le_idx])") {
		t.Errorf("emitted SQL missing the finite arm's bound read\nSQL: %s", sql)
	}
	if strings.Contains(sql, "toString(`ExplicitBounds`[le_idx])") {
		t.Errorf("emitted SQL spells `le` with ClickHouse's toString; Prometheus stamps it with strconv.FormatFloat(bound, 'f', -1, 64)\nSQL: %s", sql)
	}
	// Pin the cumulative-count Value expression. Element n of
	// `arrayCumSum(arr)` is the sum of the first n elements: the
	// cumulative count for observations with value ≤ the bucket's upper
	// edge — matching Prom's `_bucket{le=X}` cumulative-counter shape.
	if !strings.Contains(sql, "arrayMap(c -> toFloat64(c), arrayCumSum(`BucketCounts`))") {
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
	// arrayJoin fan-out layer), and isClassicBucketFanoutProject must
	// recognise it: its `arrayJoin(arrayZip(...)) AS le_pair` projection
	// zips the per-bucket `le` strings with the cumulative counts.
	inner, ok := canonical.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("canonical Project Input is %T; want *chplan.Project (the fan-out layer)", canonical.Input)
	}
	found := isClassicBucketFanoutProject(inner, s)
	if !found {
		t.Fatalf("fan-out Project has no arrayJoin(arrayZip(...)) AS le_pair projection over BucketCounts: %+v", inner.Projections)
	}
}

// TestLower_HistogramBucket_InstantSelectionIsNotRowSorted pins the memory
// shape of an instant `_bucket` selection, the input histogram_quantile's
// float-domain reassembly reads under arithmetic such as `m_bucket * 2`.
// Two things there grow with every histogram row the selection reads:
//
//   - the stale-marker layout's per-series bound union, which must come
//     from an aggregate rather than a window function, because ClickHouse
//     evaluates a window by sorting every input row with all its columns;
//   - the fan-out's arrayJoin, which copies every column of the row it
//     expands, so the fanned rows must not carry the bucket arrays.
//
// Both are checked in instant and range mode.
func TestLower_HistogramBucket_InstantSelectionIsNotRowSorted(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	// An established Flags column is what turns the stale-marker layout on.
	s.FlagsColumnProbed = true
	if s.StaleMarkerFlagsColumn() == "" {
		t.Fatal("the default schema names no Flags column, so the stale-marker layout cannot be built")
	}
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`histogram_quantile(0.9, http_server_request_duration_bucket * 2)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	instant, err := LowerAt(context.Background(), expr, s, end, end)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	ranged, err := LowerAtRange(context.Background(), expr, s, end.Add(-6*time.Hour), end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	bucketArrays := []string{s.ExplicitBoundsColumn, s.BucketCountsColumn, staleLayoutBoundsAlias, staleLayoutCountsAlias}
	for mode, plan := range map[string]chplan.Node{"instant": instant, "range": ranged} {
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("%s: Emit: %v", mode, err)
		}
		if strings.Contains(sql, " OVER (") {
			t.Errorf("%s: emitted SQL evaluates a window function:\n%s", mode, sql)
		}
		if !strings.Contains(sql, staleLayoutCountsAlias) {
			t.Fatalf("%s: emitted SQL builds no stale-marker layout, so the check below proves nothing:\n%s", mode, sql)
		}
		fanouts := 0
		chplan.Walk(plan, func(n chplan.Node) bool {
			p, ok := n.(*chplan.Project)
			if !ok || !slices.ContainsFunc(p.Projections, func(pr chplan.Projection) bool { return pr.Alias == bucketPairAlias }) {
				return true
			}
			fanouts++
			for _, pr := range p.Projections {
				if slices.Contains(bucketArrays, pr.Alias) {
					t.Errorf("%s: the bucket fan-out carries %s through its arrayJoin", mode, pr.Alias)
				}
			}
			return true
		})
		if fanouts != 1 {
			t.Errorf("%s: found %d bucket fan-outs, want 1", mode, fanouts)
		}
	}
}

// TestLower_HistogramBucket_LeMatcherNarrowsPairsBeforeArrayJoin pins where
// an `le` matcher on a `_bucket` selection is applied: to each histogram
// row's zipped `(le, cumulative count)` pairs, inside the arrayJoin, so the
// fan-out only ever expands the buckets the matcher selects. A Filter on the
// synthesised `Attributes['le']` above the fan-out answers the same rows but
// materialises every bucket of every row first.
func TestLower_HistogramBucket_LeMatcherNarrowsPairsBeforeArrayJoin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		flags bool
		want  []leTest
	}{
		{name: "equality", query: `http_server_request_duration_bucket{le="0.5"}`, want: []leTest{{chplan.OpEq, "0.5"}}},
		{
			name:  "regex_and_negations",
			query: `http_server_request_duration_bucket{le=~"0.5|1|2", le!="+Inf", le!~"2"}`,
			want:  []leTest{{chplan.OpMatch, "0.5|1|2"}, {chplan.OpNe, "+Inf"}, {chplan.OpNotMatch, "2"}},
		},
		{name: "stale_marker_layout", query: `http_server_request_duration_bucket{le="2"}`, flags: true, want: []leTest{{chplan.OpEq, "2"}}},
		{name: "regex_name", query: `{__name__=~"http_server_request_duration_bucket", le="0.5"}`, want: []leTest{{chplan.OpEq, "0.5"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelMetrics()
			s.FlagsColumnProbed = tc.flags
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			fanouts := 0
			chplan.Walk(plan, func(n chplan.Node) bool {
				switch n := n.(type) {
				case *chplan.Filter:
					// The regex-name union's gauge / sum arm reads a stored
					// `le` attribute off its own scan, which is correct; only a
					// Filter over the fan-out reads the synthesised one.
					if !containsBucketFanout(n.Input) {
						return true
					}
					chplan.InspectExpr(n.Predicate, func(e chplan.Expr) bool {
						if m, ok := e.(*chplan.MapAccess); ok {
							if key, ok := m.Key.(*chplan.LitString); ok && key.V == bucketBoundLabel {
								t.Errorf("a Filter reads the synthesised %q label after the fan-out", bucketBoundLabel)
							}
						}
						return true
					})
				case *chplan.Project:
					for _, pr := range n.Projections {
						if pr.Alias != bucketPairAlias {
							continue
						}
						fanouts++
						join, ok := pr.Expr.(*chplan.FuncCall)
						if !ok || join.Fn != chplan.FnArrayJoin || len(join.Args) != 1 {
							t.Fatalf("%s is not an arrayJoin: %#v", bucketPairAlias, pr.Expr)
						}
						keep, ok := join.Args[0].(*chplan.FuncCall)
						if !ok || keep.Fn != chplan.FnArrayFilter || len(keep.Args) != 2 {
							t.Fatalf("the arrayJoin expands unfiltered pairs: %#v", join.Args[0])
						}
						if zip, ok := keep.Args[1].(*chplan.FuncCall); !ok || zip.Fn != chplan.FnArrayZip {
							t.Fatalf("the arrayFilter does not narrow the zipped pairs: %#v", keep.Args[1])
						}
						lambda, ok := keep.Args[0].(*chplan.Lambda)
						if !ok || len(lambda.Params) != 1 {
							t.Fatalf("the pair filter is not a one-parameter lambda: %#v", keep.Args[0])
						}
						if got := pairTests(t, lambda.Body, lambda.Params[0]); !slices.Equal(got, tc.want) {
							t.Errorf("the pair filter tests %v, want %v ANDed", got, tc.want)
						}
						if !tc.flags && !isClassicBucketFanoutProject(n, s) {
							t.Errorf("isClassicBucketFanoutProject does not recognise the narrowed fan-out")
						}
					}
				}
				return true
			})
			if fanouts != 1 {
				t.Errorf("found %d bucket fan-outs, want 1", fanouts)
			}
		})
	}
}

// containsBucketFanout reports whether the plan below n holds the bucket
// fan-out Project.
func containsBucketFanout(n chplan.Node) bool {
	found := false
	chplan.Walk(n, func(n chplan.Node) bool {
		if p, ok := n.(*chplan.Project); ok && slices.ContainsFunc(p.Projections, func(pr chplan.Projection) bool { return pr.Alias == bucketPairAlias }) {
			found = true
		}
		return !found
	})
	return found
}

// leTest is one `le` comparison the fan-out's pair filter applies.
type leTest struct {
	op    chplan.BinaryOp
	value string
}

// pairTests flattens an AND fold of comparisons, each of which must test
// the `le` element of the lambda parameter param against a string literal.
func pairTests(t *testing.T, body chplan.Expr, param string) []leTest {
	t.Helper()
	b, ok := body.(*chplan.Binary)
	if !ok {
		t.Fatalf("the pair filter body is not a comparison: %#v", body)
	}
	if b.Op == chplan.OpAnd {
		return append(pairTests(t, b.Left, param), pairTests(t, b.Right, param)...)
	}
	elem, ok := b.Left.(*chplan.FuncCall)
	if !ok || elem.Fn != chplan.FnTupleElement || len(elem.Args) != 2 {
		t.Fatalf("the pair filter compares %#v, not a pair element", b.Left)
	}
	if id, ok := elem.Args[0].(*chplan.BareIdent); !ok || id.Name != param {
		t.Errorf("the pair filter reads %#v, not its parameter %q", elem.Args[0], param)
	}
	if pos, ok := elem.Args[1].(*chplan.LitInt); !ok || pos.V != bucketPairLe {
		t.Errorf("the pair filter reads pair element %#v, want the le element %d", elem.Args[1], bucketPairLe)
	}
	lit, ok := b.Right.(*chplan.LitString)
	if !ok {
		t.Fatalf("the pair filter compares against %#v, not a string literal", b.Right)
	}
	return []leTest{{b.Op, lit.V}}
}
