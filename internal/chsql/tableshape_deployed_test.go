package chsql

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The sorting keys a metrics table is deployed with, as system.tables
// reports them: the one cerberus provisions, and a stock exporter's before
// and from v0.157.
const (
	cerberusMetricsSortingKey       = "MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix)"
	stockMetricsSortingKeyPreV0157  = "ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix)"
	stockMetricsSortingKeyFromV0157 = "ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix"
)

// deployedShapeFilter is a metrics Filter(Scan) of table, with a SELECT *
// (so PREWHERE promotion engages) and one cheap conjunct per sort column
// the stock and provisioned keys disagree on, written in an order no key
// ranks them in.
func deployedShapeFilter(table string) chplan.Node {
	eq := func(col, v string) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: col}, Right: &chplan.LitString{V: v}}
	}
	ts := &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: "TimeUnix"}, Right: &chplan.LitInt{V: 1}}
	attr := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: "job"}},
		Right: &chplan.LitString{V: "api"},
	}
	pred := chplan.Expr(ts)
	for _, c := range []chplan.Expr{attr, eq("ServiceName", "svc"), eq("MetricName", "up")} {
		pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: c}
	}
	return &chplan.Filter{Predicate: pred, Input: &chplan.Scan{Table: table}}
}

// deployedShapeStrategies is the metrics strategies the requirements check
// resolves for table deployed with sortingKey.
func deployedShapeStrategies(table, sortingKey string) SchemaStrategies {
	return SchemaStrategies{TableShapes: map[string]TableShape{
		table: MetricsTableShape(schema.DefaultOTelMetrics(), schema.SortingKeyColumnsOf(sortingKey)),
	}}
}

func emitUnder(t *testing.T, s SchemaStrategies, plan chplan.Node) string {
	t.Helper()
	sql, _, err := Emit(WithSchemaStrategies(context.Background(), s), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sql
}

// prewhereOf is the PREWHERE clause of sql, up to its WHERE.
func prewhereOf(t *testing.T, sql string) string {
	t.Helper()
	_, after, ok := strings.Cut(sql, " PREWHERE ")
	if !ok {
		t.Fatalf("no PREWHERE in %s", sql)
	}
	clause, _, _ := strings.Cut(after, " WHERE ")
	return clause
}

// TestPrewhere_FollowsDeployedSortingKey pins that the PREWHERE conjunct
// order follows the sorting key the scanned table is deployed with
// (SchemaStrategies.TableShapes), not the one cerberus provisions: a stock
// exporter table ranks ServiceName ahead of MetricName. The emitted SQL for
// a table deployed with cerberus's own key is the SQL the provisioned
// fallback renders.
func TestPrewhere_FollowsDeployedSortingKey(t *testing.T) {
	t.Parallel()
	const (
		svc    = "(`ServiceName` = ?)"
		metric = "(`MetricName` = ?)"
		attr   = "(`Attributes`[?] = ?)"
		ts     = "(`TimeUnix` >= ?)"
	)
	plan := deployedShapeFilter("otel_metrics_sum")
	provisioned := emitUnder(t, SchemaStrategies{}, plan)

	cases := []struct {
		name, key, prewhere string
	}{
		{"cerberus key", cerberusMetricsSortingKey, metric + " AND " + attr + " AND " + svc},
		{"stock key before v0.157", stockMetricsSortingKeyPreV0157, svc + " AND " + metric + " AND " + attr},
		{"stock key from v0.157", stockMetricsSortingKeyFromV0157, svc + " AND " + metric + " AND " + ts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := emitUnder(t, deployedShapeStrategies("otel_metrics_sum", tc.key), plan)
			if got := prewhereOf(t, sql); got != tc.prewhere {
				t.Errorf("PREWHERE = %s\nwant      %s\nSQL: %s", got, tc.prewhere, sql)
			}
		})
	}
	if got := emitUnder(t, deployedShapeStrategies("otel_metrics_sum", cerberusMetricsSortingKey), plan); got != provisioned {
		t.Errorf("cerberus's deployed key renders\n%s\nthe provisioned fallback renders\n%s", got, provisioned)
	}
}

// TestPrewhere_RenamedTableTakesItsDeployedShape pins that a metrics table
// renamed through CERBERUS_SCHEMA_* is ranked by the shape the requirements
// check resolved for it, exactly as the default-named table deployed with the
// same key is, and that without one it gets no PREWHERE at all (no default
// shape names it).
func TestPrewhere_RenamedTableTakesItsDeployedShape(t *testing.T) {
	t.Parallel()
	const renamed = "metrics_sum_v2"
	renamedSQL := emitUnder(t, deployedShapeStrategies(renamed, stockMetricsSortingKeyPreV0157), deployedShapeFilter(renamed))
	defaultSQL := emitUnder(t,
		deployedShapeStrategies("otel_metrics_sum", stockMetricsSortingKeyPreV0157), deployedShapeFilter("otel_metrics_sum"))
	if want := strings.ReplaceAll(defaultSQL, "`otel_metrics_sum`", "`"+renamed+"`"); renamedSQL != want {
		t.Errorf("renamed table renders\n%s\nwant\n%s", renamedSQL, want)
	}
	if bare := emitUnder(t, SchemaStrategies{}, deployedShapeFilter(renamed)); strings.Contains(bare, "PREWHERE") {
		t.Errorf("a renamed table with no resolved shape got PREWHERE promotion: %s", bare)
	}
}

// TestProvisionedShapes_MatchTheProvisionedSortingKeys pins that the shape a
// table falls back to when its deployed key was not read sorts by exactly the
// columns of the key cerberus provisions it with — parsed out of the pinned
// exporter templates internal/schema/ddl renders the tables from, and reduced
// by schema.SortingKeyColumnsOf as a deployed key is. A provisioned table
// whose key the requirements check reads therefore renders the same SQL as
// the fallback.
func TestProvisionedShapes_MatchTheProvisionedSortingKeys(t *testing.T) {
	t.Parallel()
	m, l, tr := schema.DefaultOTelMetrics(), schema.DefaultOTelLogs(), schema.DefaultOTelTraces()
	cases := []struct {
		name  string
		ddl   string
		shape TableShape
	}{
		{"logs", sqltemplates.LogsCreateTable, LogsTableShape(l, nil)},
		{"traces", sqltemplates.TracesCreateTable, TracesTableShape(tr, nil)},
	}
	for _, p := range []schema.MetricsTimestampPrecision{schema.MetricsTimestampNanos, schema.MetricsTimestampSeconds} {
		set := sqltemplates.MetricsCreateTables(p.SQLTemplatePrecision())
		for _, ddl := range []string{set.Gauge, set.Sum, set.Histogram, set.ExpHistogram, set.Summary} {
			cases = append(cases, struct {
				name  string
				ddl   string
				shape TableShape
			}{"metrics " + string(p), ddl, MetricsTableShape(m, nil)})
		}
	}
	for _, tc := range cases {
		key := orderByKey(t, tc.ddl)
		if want := schema.SortingKeyColumnsOf(key); len(want) == 0 || !slices.Equal(tc.shape.SortColumns, want) {
			t.Errorf("%s: provisioned SortColumns = %v; the provisioned ORDER BY (%s) sorts by %v",
				tc.name, tc.shape.SortColumns, key, want)
		}
	}
}

// orderByKey is the element list of a template's `ORDER BY (…)` tuple, as
// system.tables reports a sorting key.
func orderByKey(t *testing.T, ddl string) string {
	t.Helper()
	_, after, ok := strings.Cut(ddl, "ORDER BY (")
	if !ok {
		t.Fatalf("no `ORDER BY (` in:\n%s", ddl)
	}
	depth := 0
	for i, c := range after {
		switch c {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return after[:i]
			}
			depth--
		}
	}
	t.Fatalf("unterminated ORDER BY tuple in:\n%s", ddl)
	return ""
}
