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

	pairShape := []string{"arrayZip(arrayMap(le_idx -> ", "arrayCumSum(`BucketCounts`)"}
	indexShape := []string{
		"arrayEnumerate(`BucketCounts`)) AS `le_idx`",
		"toFloat64(arraySum(arraySlice(`BucketCounts`, ?, le_idx)))",
	}
	cases := []struct {
		name  string
		query string
		shape []string
	}{
		{name: "bare", query: `http_server_request_duration_bucket`, shape: pairShape},
		{name: "with_le_filter", query: `http_server_request_duration_bucket{le="0.5"}`, shape: pairShape},
		{name: "rate_wrapped", query: `rate(http_server_request_duration_bucket[5m])`, shape: indexShape},
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
			for _, want := range append([]string{
				"arrayEnumerate(`BucketCounts`)",
				"mapConcat(`Attributes`, map(",
				"if((le_idx > length(`ExplicitBounds`))",
			}, tc.shape...) {
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
	// recognise it: a range window's selection without the stale-marker
	// layout expands `arrayJoin(arrayEnumerate(BucketCounts)) AS le_idx`.
	inner, ok := canonical.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("canonical Project Input is %T; want *chplan.Project (the fan-out layer)", canonical.Input)
	}
	found := isClassicBucketFanoutProject(inner, s)
	if !found {
		t.Fatalf("fan-out Project has no arrayJoin(arrayEnumerate(BucketCounts)) AS le_idx projection: %+v", inner.Projections)
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
			if !ok || !slices.ContainsFunc(p.Projections, isBucketFanoutProjection) {
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

// TestLower_HistogramBucket_RangeWindowFanoutExpandsCountsOnly pins what
// a range window's `_bucket` selection expands: the arrayJoin of its
// fan-out reads BucketCounts and no other column. The range window's
// fan-out guard counts the same fan-out's rows in a second read of the
// histogram rows, and ClickHouse keeps an arrayJoin's argument in a copy
// of the fan-out that reads none of its columns, so every column the
// argument reads is read again by that count. An instant selection keeps
// the fanned rows free of the bucket arrays instead, since a latest-sample
// pick, not a counted range window, reads them.
func TestLower_HistogramBucket_RangeWindowFanoutExpandsCountsOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		query   string
		flags   bool
		instant bool
	}{
		{name: "rate", query: `rate(http_server_request_duration_bucket{route="/r/1"}[5m])`},
		{name: "rate_stale_markers_dropped", query: `rate(http_server_request_duration_bucket{route="/r/1"}[5m])`, flags: true},
		{name: "sum_by_le", query: `sum by (le) (rate(http_server_request_duration_bucket{route=~"/r/[0-4]"}[5m]))`, flags: true},
		{name: "regex_name", query: `rate({__name__=~"cpu_temp|gpu_temp"}[5m])`, flags: true},
		{name: "subquery", query: `last_over_time(rate({__name__=~"cpu_temp|gpu_temp"}[5m])[10m:1m])`, flags: true},
		{name: "instant", query: `http_server_request_duration_bucket`, instant: true},
		{name: "instant_stale_markers_encoded", query: `http_server_request_duration_bucket`, flags: true, instant: true},
	}
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelMetrics()
			s.FlagsColumnProbed = tc.flags
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := LowerAtRange(context.Background(), expr, s, end.Add(-time.Hour), end, time.Minute)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
			}
			fanouts := 0
			chplan.Walk(plan, func(n chplan.Node) bool {
				p, ok := n.(*chplan.Project)
				if !ok {
					return true
				}
				for _, pr := range p.Projections {
					if !isBucketFanoutProjection(pr) {
						continue
					}
					fanouts++
					if tc.instant {
						if pr.Alias != bucketPairAlias {
							t.Errorf("an instant selection fans %s, want %s", pr.Alias, bucketPairAlias)
						}
						continue
					}
					var read []string
					chplan.InspectExpr(pr.Expr, func(e chplan.Expr) bool {
						if c, ok := e.(*chplan.ColumnRef); ok && !slices.Contains(read, c.Name) {
							read = append(read, c.Name)
						}
						return true
					})
					if !slices.Equal(read, []string{s.BucketCountsColumn}) {
						t.Errorf("the range window's fan-out expands %s, which reads %v, want only %s",
							pr.Alias, read, s.BucketCountsColumn)
					}
				}
				return true
			})
			if fanouts == 0 {
				t.Fatalf("%q lowers to no bucket fan-out, so the check proves nothing", tc.query)
			}
		})
	}
}

// TestLower_HistogramBucket_LeMatcherNarrowsPairsBeforeArrayJoin pins where
// an `le` matcher on a `_bucket` selection is applied: to each histogram
// row's buckets, inside the arrayJoin — its zipped `(le, cumulative count)`
// pairs in the pair shape, its bucket indices in the index shape a range
// window reads — so the fan-out only ever expands the buckets the matcher
// selects. A Filter on the synthesised `Attributes['le']` above the fan-out
// answers the same rows but materialises every bucket of every row first.
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
		{name: "range_window", query: `rate(http_server_request_duration_bucket{le="0.5"}[5m])`, want: []leTest{{chplan.OpEq, "0.5"}}},
		{
			name:  "range_window_stale_markers_dropped",
			query: `rate(http_server_request_duration_bucket{le=~"0.5|1", le!="+Inf"}[5m])`,
			flags: true,
			want:  []leTest{{chplan.OpMatch, "0.5|1"}, {chplan.OpNe, "+Inf"}},
		},
		{name: "range_window_regex_name", query: `rate({__name__=~"http_server_request_duration_bucket", le="0.5"}[5m])`, want: []leTest{{chplan.OpEq, "0.5"}}},
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
						if pr.Alias != bucketPairAlias && pr.Alias != bucketIdxAlias {
							continue
						}
						fanouts++
						join, ok := pr.Expr.(*chplan.FuncCall)
						if !ok || join.Fn != chplan.FnArrayJoin || len(join.Args) != 1 {
							t.Fatalf("%s is not an arrayJoin: %#v", pr.Alias, pr.Expr)
						}
						keep, ok := join.Args[0].(*chplan.FuncCall)
						if !ok || keep.Fn != chplan.FnArrayFilter || len(keep.Args) != 2 {
							t.Fatalf("the arrayJoin expands unfiltered buckets: %#v", join.Args[0])
						}
						narrowed := chplan.FnArrayZip
						if pr.Alias == bucketIdxAlias {
							narrowed = chplan.FnArrayEnumerate
						}
						if inner, ok := keep.Args[1].(*chplan.FuncCall); !ok || inner.Fn != narrowed {
							t.Fatalf("the arrayFilter does not narrow the %s buckets: %#v", pr.Alias, keep.Args[1])
						}
						lambda, ok := keep.Args[0].(*chplan.Lambda)
						if !ok || len(lambda.Params) != 1 {
							t.Fatalf("the bucket filter is not a one-parameter lambda: %#v", keep.Args[0])
						}
						if got := bucketTests(t, lambda.Body, bucketLeOf(pr.Alias, lambda.Params[0], s)); !slices.Equal(got, tc.want) {
							t.Errorf("the bucket filter tests %v, want %v ANDed", got, tc.want)
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
		if p, ok := n.(*chplan.Project); ok && slices.ContainsFunc(p.Projections, isBucketFanoutProjection) {
			found = true
		}
		return !found
	})
	return found
}

// isBucketFanoutProjection reports whether pr is the fan-out's arrayJoin
// projection, in either shape.
func isBucketFanoutProjection(pr chplan.Projection) bool {
	return pr.Alias == bucketPairAlias || pr.Alias == bucketIdxAlias
}

// bucketLeOf is the `le` string the fan-out shape that alias names tests
// for the bucket its filter's lambda parameter param names: the pair's `le`
// element, or the string synthesised from the index into ExplicitBounds.
func bucketLeOf(alias, param string, s schema.Metrics) chplan.Expr {
	if alias == bucketIdxAlias {
		return classicBucketLeStringExpr(&chplan.BareIdent{Name: param}, &chplan.ColumnRef{Name: s.ExplicitBoundsColumn})
	}
	return &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{&chplan.BareIdent{Name: param}, &chplan.LitInt{V: bucketPairLe}}}
}

// leTest is one `le` comparison the fan-out's bucket filter applies.
type leTest struct {
	op    chplan.BinaryOp
	value string
}

// bucketTests flattens an AND fold of comparisons, each of which must test
// le, the bucket's `le` string, against a string literal.
func bucketTests(t *testing.T, body, le chplan.Expr) []leTest {
	t.Helper()
	b, ok := body.(*chplan.Binary)
	if !ok {
		t.Fatalf("the bucket filter body is not a comparison: %#v", body)
	}
	if b.Op == chplan.OpAnd {
		return append(bucketTests(t, b.Left, le), bucketTests(t, b.Right, le)...)
	}
	if !b.Left.Equal(le) {
		t.Errorf("the bucket filter compares %#v, not the bucket's le string %#v", b.Left, le)
	}
	lit, ok := b.Right.(*chplan.LitString)
	if !ok {
		t.Fatalf("the bucket filter compares against %#v, not a string literal", b.Right)
	}
	return []leTest{{b.Op, lit.V}}
}

// TestIsClassicBucketFanoutProject_RejectsNearMisses pins every guard of
// the fan-out recogniser: starting from a real fan-out, each case breaks
// exactly one part of the `arrayJoin([arrayFilter(...,] arrayZip(arrayMap(
// ..., arrayEnumerate(BucketCounts)), ...)[)]) AS le_pair` pair shape or
// the `arrayJoin([arrayFilter(...,] arrayEnumerate(BucketCounts)[)]) AS
// le_idx` index shape a range window reads, and the recogniser must refuse
// it, while the untouched fan-out, with and without an `le` filter, is
// recognised in both shapes.
func TestIsClassicBucketFanoutProject_RejectsNearMisses(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	join := func(p *chplan.Project) *chplan.FuncCall {
		for _, pr := range p.Projections {
			if isBucketFanoutProjection(pr) {
				return pr.Expr.(*chplan.FuncCall)
			}
		}
		t.Fatal("the fan-out has no le_pair or le_idx projection")
		return nil
	}
	fanned := func(p *chplan.Project) *chplan.FuncCall {
		inner := join(p).Args[0].(*chplan.FuncCall)
		if inner.Fn == chplan.FnArrayFilter {
			inner = inner.Args[1].(*chplan.FuncCall)
		}
		return inner
	}
	les := func(p *chplan.Project) *chplan.FuncCall { return fanned(p).Args[0].(*chplan.FuncCall) }
	enumerate := func(p *chplan.Project) *chplan.FuncCall { return les(p).Args[1].(*chplan.FuncCall) }
	const (
		pairs         = `http_server_request_duration_bucket`
		filteredPairs = `http_server_request_duration_bucket{le="2"}`
		indices       = `rate(http_server_request_duration_bucket[5m])`
		filteredIdx   = `rate(http_server_request_duration_bucket{le="2"}[5m])`
	)
	other := &chplan.FuncCall{Fn: chplan.FnArrayConcat}
	cases := []struct {
		name  string
		query string
		edit  func(p *chplan.Project)
		want  bool
	}{
		{name: "pair fan-out", query: pairs, edit: func(*chplan.Project) {}, want: true},
		{name: "le-filtered pair fan-out", query: filteredPairs, edit: func(*chplan.Project) {}, want: true},
		{name: "index fan-out", query: indices, edit: func(*chplan.Project) {}, want: true},
		{name: "le-filtered index fan-out", query: filteredIdx, edit: func(*chplan.Project) {}, want: true},
		{name: "not arrayJoin", query: pairs, edit: func(p *chplan.Project) { join(p).Fn = chplan.FnArrayFlatten }},
		{name: "index not arrayJoin", query: indices, edit: func(p *chplan.Project) { join(p).Fn = chplan.FnArrayFlatten }},
		{name: "arrayJoin with two arguments", query: pairs, edit: func(p *chplan.Project) {
			j := join(p)
			j.Args = append(j.Args, other)
		}},
		{name: "index arrayJoin with two arguments", query: indices, edit: func(p *chplan.Project) {
			j := join(p)
			j.Args = append(j.Args, other)
		}},
		{name: "pairs not zipped", query: pairs, edit: func(p *chplan.Project) { fanned(p).Fn = chplan.FnArrayConcat }},
		{name: "filtered pairs not zipped", query: filteredPairs, edit: func(p *chplan.Project) { fanned(p).Fn = chplan.FnArrayConcat }},
		{name: "le strings not mapped", query: pairs, edit: func(p *chplan.Project) { les(p).Fn = chplan.FnArrayFilter }},
		{name: "le map with three arguments", query: pairs, edit: func(p *chplan.Project) {
			l := les(p)
			l.Args = append(l.Args, other)
		}},
		{name: "indices not enumerated", query: pairs, edit: func(p *chplan.Project) { enumerate(p).Fn = chplan.FnArrayCumSum }},
		{name: "enumerate with two arguments", query: pairs, edit: func(p *chplan.Project) {
			e := enumerate(p)
			e.Args = append(e.Args, other)
		}},
		{name: "enumerates another column", query: pairs, edit: func(p *chplan.Project) {
			enumerate(p).Args[0] = &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}
		}},
		{name: "fanned indices not enumerated", query: indices, edit: func(p *chplan.Project) { fanned(p).Fn = chplan.FnArrayCumSum }},
		{name: "filtered indices not enumerated", query: filteredIdx, edit: func(p *chplan.Project) { fanned(p).Fn = chplan.FnArrayCumSum }},
		{name: "index enumerate with two arguments", query: indices, edit: func(p *chplan.Project) {
			e := fanned(p)
			e.Args = append(e.Args, other)
		}},
		{name: "index enumerates another column", query: indices, edit: func(p *chplan.Project) {
			fanned(p).Args[0] = &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			var fanout *chplan.Project
			chplan.Walk(plan, func(n chplan.Node) bool {
				if p, ok := n.(*chplan.Project); ok && fanout == nil && slices.ContainsFunc(p.Projections, isBucketFanoutProjection) {
					fanout = p
				}
				return fanout == nil
			})
			if fanout == nil {
				t.Fatalf("%q lowers to no bucket fan-out", tc.query)
			}
			shape := bucketPairAlias
			if strings.HasPrefix(tc.query, "rate(") {
				shape = bucketIdxAlias
			}
			if !slices.ContainsFunc(fanout.Projections, func(pr chplan.Projection) bool { return pr.Alias == shape }) {
				t.Fatalf("%q lowers to no %s fan-out, so the case breaks the other shape", tc.query, shape)
			}
			tc.edit(fanout)
			if got := isClassicBucketFanoutProject(fanout, s); got != tc.want {
				t.Errorf("isClassicBucketFanoutProject = %v, want %v", got, tc.want)
			}
		})
	}
}
