package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerMetricsPipeline exercises the MetricsPipeline lowering
// directly: parse a TraceQL metrics query, lower it, and walk the
// resulting chplan tree to confirm the expected MetricsAggregate(Scan)
// shape, group-by labels, and chplan MetricsOp.
//
// Range / step intentionally aren't part of the lowered tree —
// the /api/metrics/query_range handler wraps with chplan.RangeWindow
// at request time (see docs/upstream-forks.md).
func TestLowerMetricsPipeline(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name      string
		query     string
		wantOp    chplan.MetricsOp
		wantAttr  bool // whether MetricsAggregate.Attr is non-nil
		wantGroup int  // number of GroupBy expressions
		hasFilter bool // whether the spanset selector produces a Filter
	}{
		{
			name:   "rate_no_selector",
			query:  "{} | rate()",
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "count_over_time_with_selector",
			query:  `{ resource.service.name = "frontend" } | count_over_time()`,
			wantOp: chplan.MetricsOpCountOverTime, wantAttr: false, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "sum_over_time_attr",
			query:  `{} | sum_over_time(duration)`,
			wantOp: chplan.MetricsOpSumOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "min_over_time_attr",
			query:  `{} | min_over_time(duration)`,
			wantOp: chplan.MetricsOpMinOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "max_over_time_attr",
			query:  `{} | max_over_time(duration)`,
			wantOp: chplan.MetricsOpMaxOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "rate_by_label",
			query:  `{} | rate() by (resource.service.name)`,
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 1, hasFilter: true,
		},
		{
			// Multi-attribute `by (...)` — every element of the
			// upstream MetricsAggregate.GroupBy() list must survive
			// into chplan.MetricsAggregate.GroupBy (and the parallel
			// GroupByAliases). Locks in the contract the chDB
			// roundtrip fixtures `by_multi_attribute`,
			// `by_three_attributes`, `by_mixed_scopes`, and
			// `by_intrinsic_and_attr` rely on.
			name:   "rate_by_two_labels",
			query:  `{} | rate() by (resource.service.name, span.http.status_code)`,
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 2, hasFilter: true,
		},
		{
			name:   "rate_by_three_labels",
			query:  `{} | rate() by (resource.service.name, span.kind, span.http.method)`,
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 3, hasFilter: true,
		},
		{
			name:   "count_over_time_by_intrinsic_and_attr",
			query:  `{} | count_over_time() by (kind, resource.service.name)`,
			wantOp: chplan.MetricsOpCountOverTime, wantAttr: false, wantGroup: 2, hasFilter: true,
		},
		{
			name:   "avg_over_time_attr",
			query:  `{} | avg_over_time(duration)`,
			wantOp: chplan.MetricsOpAvgOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "avg_over_time_by_label",
			query:  `{} | avg_over_time(duration) by (resource.service.name)`,
			wantOp: chplan.MetricsOpAvgOverTime, wantAttr: true, wantGroup: 1, hasFilter: true,
		},
		{
			name:   "quantile_over_time_single",
			query:  `{} | quantile_over_time(duration, 0.95)`,
			wantOp: chplan.MetricsOpQuantileOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			ma, ok := plan.(*chplan.MetricsAggregate)
			if !ok {
				t.Fatalf("expected outermost node to be *chplan.MetricsAggregate, got %T", plan)
			}
			if ma.Op != tc.wantOp {
				t.Errorf("MetricsAggregate.Op = %v, want %v", ma.Op, tc.wantOp)
			}
			if ma.ValueAlias != "Value" {
				t.Errorf("MetricsAggregate.ValueAlias = %q, want %q", ma.ValueAlias, "Value")
			}
			if (ma.Attr != nil) != tc.wantAttr {
				t.Errorf("MetricsAggregate.Attr non-nil = %v, want %v", ma.Attr != nil, tc.wantAttr)
			}
			if len(ma.GroupBy) != tc.wantGroup {
				t.Errorf("len(MetricsAggregate.GroupBy) = %d, want %d", len(ma.GroupBy), tc.wantGroup)
			}
			if len(ma.GroupBy) != len(ma.GroupByAliases) {
				t.Errorf("GroupBy/GroupByAliases length mismatch: %d vs %d", len(ma.GroupBy), len(ma.GroupByAliases))
			}
			if len(ma.GroupBy) > 0 && len(ma.GroupBy) != len(ma.GroupByDisplayNames) {
				t.Errorf("GroupBy/GroupByDisplayNames length mismatch: %d vs %d", len(ma.GroupBy), len(ma.GroupByDisplayNames))
			}

			// Walk the inner subtree: Scan, optionally wrapped by Filter.
			inner := ma.Inner
			if tc.hasFilter {
				f, ok := inner.(*chplan.Filter)
				if !ok {
					t.Fatalf("expected MetricsAggregate.Inner to be *chplan.Filter, got %T", inner)
				}
				inner = f.Input
			}
			scan, ok := inner.(*chplan.Scan)
			if !ok {
				t.Fatalf("expected MetricsAggregate.Inner innermost to be *chplan.Scan, got %T", inner)
			}
			if scan.Table != s.SpansTable {
				t.Errorf("Scan.Table = %q, want %q", scan.Table, s.SpansTable)
			}

			// Quantile case: Quantiles carries the literal.
			if tc.wantOp == chplan.MetricsOpQuantileOverTime {
				if len(ma.Quantiles) != 1 {
					t.Fatalf("expected 1 quantile, got %d", len(ma.Quantiles))
				}
				if ma.Quantiles[0] != 0.95 {
					t.Errorf("Quantiles[0] = %v, want 0.95", ma.Quantiles[0])
				}
			}
		})
	}
}

// TestLowerMetricsPipeline_DurationSeconds pins the duration-to-seconds
// unit conversion for the *_over_time aggregations.
//
// The OTel-CH Duration column is Int64 nanoseconds; Tempo's reference
// engine emits metric values in fractional seconds (its
// sumOverTimeAggregator / averageOverTimeAggregator /
// quantileOverTimeAggregator all divide by 1e9 in
// pkg/traceql/engine_metrics.go). Cerberus matches that wire shape by
// wrapping the lowered Duration expression in `<expr> / 1e9` at
// lowering time. The wrap applies to every duration-aware *_over_time
// aggregation (sum / avg / min / max / quantile) — count_over_time and
// rate take no operand and fall through the early return in
// metricsAggregateAttr, so this test does not exercise them.
//
// Without the wrap, the Tempo compat differ flagged
// `metrics_avg_over_time_instant` with a ~1e9 ratio between cerberus
// (raw ns) and Tempo (seconds); pinning the shape here keeps that bug
// from regressing.
func TestLowerMetricsPipeline_DurationSeconds(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name  string
		query string
	}{
		{"sum_over_time_duration", `{} | sum_over_time(duration)`},
		{"avg_over_time_duration", `{} | avg_over_time(duration)`},
		{"min_over_time_duration", `{} | min_over_time(duration)`},
		{"max_over_time_duration", `{} | max_over_time(duration)`},
		{"quantile_over_time_duration", `{} | quantile_over_time(duration, 0.95)`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			ma, ok := plan.(*chplan.MetricsAggregate)
			if !ok {
				t.Fatalf("expected *chplan.MetricsAggregate, got %T", plan)
			}
			// Operand must be a (Duration / 1e9) Binary node so the CH
			// aggregate (sum / avg / min / max / quantile) reduces over
			// seconds rather than raw nanoseconds.
			bin, ok := ma.Attr.(*chplan.Binary)
			if !ok {
				t.Fatalf("expected MetricsAggregate.Attr to be *chplan.Binary, got %T", ma.Attr)
			}
			if bin.Op != chplan.OpDiv {
				t.Errorf("Binary.Op = %v, want %v", bin.Op, chplan.OpDiv)
			}
			col, ok := bin.Left.(*chplan.ColumnRef)
			if !ok {
				t.Fatalf("expected Binary.Left to be *chplan.ColumnRef, got %T", bin.Left)
			}
			if col.Name != s.DurationColumn {
				t.Errorf("Binary.Left.Name = %q, want %q", col.Name, s.DurationColumn)
			}
			div, ok := bin.Right.(*chplan.LitFloat)
			if !ok {
				t.Fatalf("expected Binary.Right to be *chplan.LitFloat, got %T", bin.Right)
			}
			if div.V != 1e9 {
				t.Errorf("Binary.Right.V = %v, want 1e9", div.V)
			}
		})
	}
}

// TestLowerMetricsPipeline_NonDurationAttrUnwrapped guards the
// negative case: only the `duration` intrinsic gets the ns→s rebase.
// Span-attribute operands (`span.<attr>`) and resource-attribute
// operands carry user-defined units and must NOT be divided by 1e9.
func TestLowerMetricsPipeline_NonDurationAttrUnwrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(`{} | sum_over_time(span.bytes)`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	ma, ok := plan.(*chplan.MetricsAggregate)
	if !ok {
		t.Fatalf("expected *chplan.MetricsAggregate, got %T", plan)
	}
	if _, isBin := ma.Attr.(*chplan.Binary); isBin {
		t.Errorf("non-duration operand must not be wrapped in /1e9 Binary; got %T", ma.Attr)
	}
}

// Every metrics-pipeline form now lowers:
//
//   - `histogram_over_time(...)` → chplan.MetricsHistogramOverTime
//     (TestLowerHistogramOverTime in histogram_over_time_test.go).
//   - `avg_over_time(...)` → chplan.MetricsAggregate{Op:
//     MetricsOpAvgOverTime} via lowerAverageOverTime, which unwraps
//     the Tempo fork's exported AverageOverTimeAggregator type
//     (#430).
//   - `| topk(N)` / `| bottomk(N)` / `| > N` / chained second-stage
//     → chplan.MetricsSecondStage via lowerMetricsSecondStage; see
//     TestLowerMetricsSecondStage below.
//   - `quantile_over_time(attr, q1, q2, ...)` → multi-element
//     chplan.MetricsAggregate.Quantiles; see
//     TestLowerMetricsMultiQuantile below.
//
// TestLowerMetricsPipelineUnsupported has therefore been retired.
// New unsupported PipelineElement kinds should land their own focused
// test rather than reviving a generic "everything that errors" pool.

// TestLowerMetricsSecondStage asserts that the `| topk(N)` /
// `| bottomk(N)` / `| > N` / `| < N` / `| >= N` / `| <= N` /
// `| == N` / `| != N` and chained second-stage forms lower
// successfully into a chplan.MetricsSecondStage wrapping the
// upstream MetricsAggregate.
//
// The wrap order for chained second-stage is bottom-up: each
// successive element in ChainedSecondStage.Elements() wraps the
// previous result, so the rightmost source element ends up as
// the outermost chplan node (matches the chsql inside-out
// subquery wrap).
func TestLowerMetricsSecondStage(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name         string
		query        string
		wantOuterOp  chplan.SecondStageOp
		wantK        int64
		wantThreshOp chplan.BinaryOp
		wantThreshV  float64
		wantDepth    int // total chained MetricsSecondStage wraps
	}{
		{
			name:        "topk",
			query:       `{} | rate() | topk(5)`,
			wantOuterOp: chplan.SecondStageTopK,
			wantK:       5,
			wantDepth:   1,
		},
		{
			name:        "bottomk",
			query:       `{} | rate() | bottomk(3)`,
			wantOuterOp: chplan.SecondStageBottomK,
			wantK:       3,
			wantDepth:   1,
		},
		{
			name:         "threshold_gt",
			query:        `{} | rate() > 10`,
			wantOuterOp:  chplan.SecondStageThreshold,
			wantThreshOp: chplan.OpGt,
			wantThreshV:  10,
			wantDepth:    1,
		},
		{
			name:         "threshold_le",
			query:        `{} | rate() <= 1.5`,
			wantOuterOp:  chplan.SecondStageThreshold,
			wantThreshOp: chplan.OpLe,
			wantThreshV:  1.5,
			wantDepth:    1,
		},
		{
			name:         "chained_topk_threshold",
			query:        `{} | rate() | topk(5) > 10`,
			wantOuterOp:  chplan.SecondStageThreshold,
			wantThreshOp: chplan.OpGt,
			wantThreshV:  10,
			wantDepth:    2,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			ss, ok := plan.(*chplan.MetricsSecondStage)
			if !ok {
				t.Fatalf("expected outermost node to be *chplan.MetricsSecondStage, got %T", plan)
			}
			if ss.Op != tc.wantOuterOp {
				t.Errorf("MetricsSecondStage.Op = %v, want %v", ss.Op, tc.wantOuterOp)
			}
			if tc.wantOuterOp == chplan.SecondStageTopK || tc.wantOuterOp == chplan.SecondStageBottomK {
				if ss.K != tc.wantK {
					t.Errorf("MetricsSecondStage.K = %d, want %d", ss.K, tc.wantK)
				}
			}
			if tc.wantOuterOp == chplan.SecondStageThreshold {
				if ss.ThresholdOp != tc.wantThreshOp {
					t.Errorf("MetricsSecondStage.ThresholdOp = %v, want %v", ss.ThresholdOp, tc.wantThreshOp)
				}
				if ss.ThresholdValue != tc.wantThreshV {
					t.Errorf("MetricsSecondStage.ThresholdValue = %v, want %v", ss.ThresholdValue, tc.wantThreshV)
				}
			}
			if ss.ValueAlias != "Value" {
				t.Errorf("MetricsSecondStage.ValueAlias = %q, want %q", ss.ValueAlias, "Value")
			}

			// Walk the nested chain — count how many
			// MetricsSecondStage wraps stack on top of the
			// MetricsAggregate.
			depth := 0
			cur := chplan.Node(ss)
			for {
				inner, ok := cur.(*chplan.MetricsSecondStage)
				if !ok {
					break
				}
				depth++
				cur = inner.Input
			}
			if depth != tc.wantDepth {
				t.Errorf("chained MetricsSecondStage depth = %d, want %d", depth, tc.wantDepth)
			}
			// Innermost child should be the metrics aggregate.
			if _, ok := cur.(*chplan.MetricsAggregate); !ok {
				t.Errorf("expected innermost wrapped node to be *chplan.MetricsAggregate, got %T", cur)
			}
		})
	}
}

// TestLowerMetricsSecondStageZeroLimit pins the `limit <= 0` guard in
// lowerTopKBottomK: `topk(0)` / `bottomk(0)` parse upstream but a
// zero-row top-K is meaningless, so lowering must reject it rather
// than emit `ORDER BY Value LIMIT 0` (which silently returns no
// series). The boundary matters: a CONDITIONALS_BOUNDARY mutant
// (`limit <= 0` → `limit < 0`) lets exactly limit == 0 through.
func TestLowerMetricsSecondStageZeroLimit(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, query := range []string{
		`{} | rate() | topk(0)`,
		`{} | rate() | bottomk(0)`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", query, err)
			}
			_, err = traceql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q) succeeded; want 'limit must be > 0' error", query)
			}
			if !strings.Contains(err.Error(), "limit must be > 0") {
				t.Errorf("Lower(%q) error %q does not contain %q", query, err, "limit must be > 0")
			}
		})
	}
}

// TestLowerMetricsMultiQuantile asserts that multi-quantile
// `quantile_over_time(attr, q1, q2, q3)` lowers into a single
// chplan.MetricsAggregate whose Quantiles slice carries every phi
// in source order. The chsql emit path for the multi-quantile
// shape (one output series per phi labelled with `__phi__`) lives
// outside this lowering test; this case pins the IR contract.
func TestLowerMetricsMultiQuantile(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(`{} | quantile_over_time(duration, 0.5, 0.9, 0.99)`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	ma, ok := plan.(*chplan.MetricsAggregate)
	if !ok {
		t.Fatalf("expected *chplan.MetricsAggregate, got %T", plan)
	}
	if ma.Op != chplan.MetricsOpQuantileOverTime {
		t.Errorf("MetricsAggregate.Op = %v, want %v", ma.Op, chplan.MetricsOpQuantileOverTime)
	}
	want := []float64{0.5, 0.9, 0.99}
	if len(ma.Quantiles) != len(want) {
		t.Fatalf("len(Quantiles) = %d, want %d", len(ma.Quantiles), len(want))
	}
	for i := range want {
		if ma.Quantiles[i] != want[i] {
			t.Errorf("Quantiles[%d] = %v, want %v", i, ma.Quantiles[i], want[i])
		}
	}
}
