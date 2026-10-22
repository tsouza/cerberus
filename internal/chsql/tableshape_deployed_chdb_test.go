//go:build chdb

package chsql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestPrewhere_DeployedKeyMatchesClickHouse_ChDB closes the loop the unit
// tests leave open: it creates a metrics table under each sorting key, reads
// the key back from system.tables exactly as the requirements check does,
// emits a filter over the table under the shape that key resolves to, and
// asks ClickHouse (EXPLAIN indexes=1) which key columns its primary-key
// condition uses. The PREWHERE must lead with the conjunct on the key's first
// column, and ClickHouse must list that column first.
func TestPrewhere_DeployedKeyMatchesClickHouse_ChDB(t *testing.T) {
	for _, tc := range []struct {
		name, orderBy, lead string
	}{
		{"cerberus key", cerberusMetricsSortingKey, "MetricName"},
		{"stock key before v0.157", stockMetricsSortingKeyPreV0157, "ServiceName"},
		{"stock key from v0.157", stockMetricsSortingKeyFromV0157, "ServiceName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := chsqltest.OpenIsolatedChDB(t)
			const table = "metrics_sum_v2"
			if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (
				MetricName String, Attributes Map(String, String), ServiceName String,
				ResourceAttributes Map(String, String), ScopeAttributes Map(String, String),
				AggregationTemporality Int32, TimeUnix DateTime64(9), Value Float64
			) ENGINE = MergeTree ORDER BY (%s)`, table, tc.orderBy)); err != nil {
				t.Fatalf("create: %v", err)
			}
			// An empty table plans as ReadNothing, with no index to explain.
			if _, err := db.Exec(fmt.Sprintf(
				"INSERT INTO %s VALUES ('up', map('job', 'api'), 'svc', map(), map(), 1, now64(9), 1)", table,
			)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			var sortingKey string
			if err := db.QueryRow(
				"SELECT sorting_key FROM system.tables WHERE database = currentDatabase() AND name = ?", table,
			).Scan(&sortingKey); err != nil {
				t.Fatalf("read sorting_key: %v", err)
			}
			strategies := SchemaStrategies{TableShapes: map[string]TableShape{
				table: MetricsTableShape(schema.DefaultOTelMetrics(), schema.SortingKeyColumnsOf(sortingKey)),
			}}

			sql, args, err := Emit(WithSchemaStrategies(context.Background(), strategies), deployedShapeFilter(table))
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if want := "(`" + tc.lead + "` = ?)"; !strings.HasPrefix(prewhereOf(t, sql), want) {
				t.Errorf("PREWHERE does not lead with %s: %s", want, sql)
			}

			rows, err := db.Query("EXPLAIN indexes=1 "+sql, args...)
			if err != nil {
				t.Fatalf("EXPLAIN: %v\n%s", err, sql)
			}
			defer func() { _ = rows.Close() }()
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("scan: %v", err)
				}
				plan = append(plan, strings.TrimSpace(line))
			}
			if got := primaryKeyFirstColumn(plan); got != tc.lead {
				t.Errorf("ClickHouse's primary-key condition leads with %q, want %q:\n%s",
					got, tc.lead, strings.Join(plan, "\n"))
			}
		})
	}
}

// primaryKeyFirstColumn is the first column EXPLAIN indexes=1 lists under
// the PrimaryKey index's `Keys:`, or "" when the plan lists none.
func primaryKeyFirstColumn(plan []string) string {
	for i, line := range plan {
		if line != "PrimaryKey" {
			continue
		}
		for j := i + 1; j+1 < len(plan); j++ {
			if plan[j] == "Keys:" {
				return plan[j+1]
			}
		}
	}
	return ""
}
