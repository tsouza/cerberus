package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultRules returns SettingsRules with both flags ON and the default OTel
// schema wired, so the eligibility + shape tests exercise the live rules.
func defaultRules() SettingsRules {
	return SettingsRules{
		OptimizeAggregationInOrder: true,
		LogCommentShape:            true,
		Metrics:                    schema.DefaultOTelMetrics(),
		Traces:                     schema.DefaultOTelTraces(),
		Logs:                       schema.DefaultOTelLogs(),
	}
}

// aggOverScan builds Aggregate(GROUP BY cols) over Scan(table).
func aggOverScan(table string, cols ...string) *chplan.Aggregate {
	groupBy := make([]chplan.Expr, len(cols))
	for i, c := range cols {
		groupBy[i] = &chplan.ColumnRef{Name: c}
	}
	return &chplan.Aggregate{
		Input:    &chplan.Scan{Table: table},
		GroupBy:  groupBy,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "v"}},
	}
}

func TestEligibleForAggregationInOrder_Positive(t *testing.T) {
	r := defaultRules()
	cases := []struct {
		name  string
		table string
		cols  []string
	}{
		{"metrics single key", "otel_metrics_sum", []string{"MetricName"}},
		{"metrics two-key prefix", "otel_metrics_gauge", []string{"MetricName", "Attributes"}},
		{"metrics full bare prefix", "otel_metrics_histogram", []string{"MetricName", "Attributes", "ServiceName"}},
		{"traces single key", "otel_traces", []string{"ServiceName"}},
		{"traces two-key prefix", "otel_traces", []string{"ServiceName", "SpanName"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := aggOverScan(tc.table, tc.cols...)
			if !r.eligibleForAggregationInOrder(plan) {
				t.Errorf("GROUP BY %v on %s: want eligible", tc.cols, tc.table)
			}
		})
	}
}

func TestEligibleForAggregationInOrder_Negative(t *testing.T) {
	r := defaultRules()
	cases := []struct {
		name string
		plan chplan.Node
	}{
		{"non-prefix key", aggOverScan("otel_metrics_sum", "Attributes")},
		{"reordered keys", aggOverScan("otel_metrics_sum", "Attributes", "MetricName")},
		{"superset past prefix", aggOverScan("otel_metrics_sum", "MetricName", "Attributes", "ServiceName", "TimeUnix")},
		{"gap in prefix", aggOverScan("otel_metrics_sum", "MetricName", "ServiceName")},
		{"empty group by", aggOverScan("otel_metrics_sum")},
		{"logs never eligible (fn-wrapped lead key)", aggOverScan("otel_logs", "ServiceName")},
		{"unknown table", aggOverScan("some_other_table", "MetricName")},
		{"no aggregate", &chplan.Scan{Table: "otel_metrics_sum"}},
		{
			"union scan has no single sort key",
			&chplan.Aggregate{
				Input:   &chplan.Scan{UnionTables: []string{"otel_metrics_sum", "otel_metrics_gauge"}},
				GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}},
			},
		},
		{
			"qualified group key is not bare",
			&chplan.Aggregate{
				Input:   &chplan.Scan{Table: "otel_metrics_sum"},
				GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName", Qualifier: "L"}},
			},
		},
		{
			"non-column group key",
			&chplan.Aggregate{
				Input:   &chplan.Scan{Table: "otel_metrics_sum"},
				GroupBy: []chplan.Expr{&chplan.FuncCall{Name: "lower", Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r.eligibleForAggregationInOrder(tc.plan) {
				t.Errorf("%s: want NOT eligible", tc.name)
			}
		})
	}
}

// TestApply_StampsOptimizeAggregationInOrder — when eligible AND the flag is
// on, apply stamps the setting; when the flag is off it never does (DARK).
func TestApply_StampsOptimizeAggregationInOrder(t *testing.T) {
	plan := aggOverScan("otel_metrics_sum", "MetricName")

	on := defaultRules().apply(context.Background(), plan)
	if got := settingValue(on, settingOptimizeAggregationInOrder); got != 1 {
		t.Errorf("flag on + eligible: setting = %v; want 1", got)
	}

	off := SettingsRules{Metrics: schema.DefaultOTelMetrics()}.apply(context.Background(), plan)
	if got := settingValue(off, settingOptimizeAggregationInOrder); got != nil {
		t.Errorf("flag off: setting = %v; want absent (DARK default)", got)
	}
}

func TestPlanShapeID_CompactAndLiteralFree(t *testing.T) {
	// A realistic metrics shape: Project over Aggregate(3 keys) over RangeWindow
	// over Scan, with literal-bearing nodes (metric name, label value) that must
	// NOT appear in the id.
	plan := &chplan.Project{
		Input: &chplan.Aggregate{
			GroupBy: []chplan.Expr{
				&chplan.ColumnRef{Name: "MetricName"},
				&chplan.ColumnRef{Name: "Attributes"},
				&chplan.ColumnRef{Name: "ServiceName"},
			},
			Input: &chplan.RangeWindow{
				Input: &chplan.Filter{
					Input:     &chplan.Scan{Table: "otel_metrics_sum"},
					Predicate: &chplan.LitString{V: "http_requests_total"},
				},
			},
		},
	}

	id := planShapeID(plan)
	if !strings.HasPrefix(id, shapeIDPrefix) {
		t.Fatalf("shape id %q missing prefix %q", id, shapeIDPrefix)
	}
	if id != "cerb:project;agg=3;rw" {
		t.Errorf("shape id = %q; want %q", id, "cerb:project;agg=3;rw")
	}

	// Literal-freeness: no metric name, label value, or group-by column name
	// may leak into the id.
	for _, literal := range []string{"http_requests_total", "MetricName", "Attributes", "ServiceName", "otel_metrics_sum"} {
		if strings.Contains(id, literal) {
			t.Errorf("shape id %q leaked literal %q", id, literal)
		}
	}

	if planShapeID(nil) != "" {
		t.Errorf("planShapeID(nil) = %q; want empty", planShapeID(nil))
	}
}

// settingValue reads name off a per-request settings ctx via chclient's
// public carrier reader, returning nil when absent.
func settingValue(ctx context.Context, name string) any {
	return chclient.QuerySettingsFromContext(ctx)[name]
}

// TestAggregationInOrder_ResultShapingSQLUnchanged proves optimize_aggregation_
// in_order is RESULT-EQUIVALENT at the cerberus layer: stamping it changes only
// the per-request ClickHouse SETTINGS, never the result-shaping SQL (SELECT /
// GROUP BY / args). chsql.Emit is byte-identical for the plan whether or not
// the setting rides the ctx, and the ONLY delta in the dispatch context is the
// optimize_aggregation_in_order entry on the settings map.
//
// This is the unit-level parity proof. A FULL row-for-row parity check (run
// the SAME query with the setting on vs off against a real ClickHouse and diff
// the rows) needs a live server: optimize_aggregation_in_order is a server-side
// execution-strategy knob, so chDB / the unit layer cannot observe its effect,
// only its result-equivalence by construction. The differential roundtrip lanes
// (test/property, compatibility/*) own that server-backed check; here we pin the
// invariant that cerberus emits identical SQL + args and differs only in
// settings.
func TestAggregationInOrder_ResultShapingSQLUnchanged(t *testing.T) {
	plan := aggOverScan("otel_metrics_sum", "MetricName")

	sqlOff, argsOff, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (rule off): %v", err)
	}

	// Stamp the setting on the ctx the same way execContext would, then emit.
	onCtx := defaultRules().apply(context.Background(), plan)
	sqlOn, argsOn, err := chsql.Emit(onCtx, plan)
	if err != nil {
		t.Fatalf("emit (rule on): %v", err)
	}

	if sqlOn != sqlOff {
		t.Errorf("result-shaping SQL changed with the setting:\n off: %s\n on:  %s", sqlOff, sqlOn)
	}
	if fmt.Sprint(argsOn) != fmt.Sprint(argsOff) {
		t.Errorf("bound args changed with the setting:\n off: %v\n on: %v", argsOff, argsOn)
	}

	// The only observable delta is the per-request setting on the ctx.
	if settingValue(onCtx, settingOptimizeAggregationInOrder) != 1 {
		t.Errorf("expected optimize_aggregation_in_order=1 on the rule-on ctx")
	}
	if settingValue(context.Background(), settingOptimizeAggregationInOrder) != nil {
		t.Errorf("baseline ctx unexpectedly carries the setting")
	}
}
