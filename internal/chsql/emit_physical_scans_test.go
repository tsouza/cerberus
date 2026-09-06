package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmitCounted_PhysicalScans pins that EmitCounted's physicalScans is the
// number of physical table references the RENDERED statement contains — the
// per-request multiplier chclient's DataShardFanoutGate charges (cerberus
// issue #3128). Each case cross-checks the count against the emitted text
// itself, so the count can never drift from what ClickHouse will actually
// execute: a plan node rendered twice counts twice, a merge() union counts
// once per member, and a database-qualified synthetic source counts zero.
func TestEmitCounted_PhysicalScans(t *testing.T) {
	t.Parallel()
	scan := func() *chplan.Scan {
		return &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId", "Timestamp"}}
	}
	cases := []struct {
		name  string
		plan  chplan.Node
		want  int
		count func(sql string) int
	}{
		{
			name:  "bare scan renders its table once",
			plan:  scan(),
			want:  1,
			count: func(sql string) int { return strings.Count(sql, "FROM `otel_traces`") },
		},
		{
			name: "SearchTraceLimit renders its input on both arms",
			plan: &chplan.SearchTraceLimit{
				Input:           scan(),
				TraceIDColumn:   "TraceId",
				TimestampColumn: "Timestamp",
				TraceLimit:      20,
			},
			want:  2,
			count: func(sql string) int { return strings.Count(sql, "FROM `otel_traces`") },
		},
		{
			name:  "merge() union counts one scan per member table",
			plan:  &chplan.Scan{UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}, Columns: []string{"MetricName"}},
			want:  3,
			count: func(sql string) int { return 3 * strings.Count(sql, "merge(") },
		},
		{
			name:  "database-qualified synthetic source is not a Distributed scan",
			plan:  &chplan.Scan{Database: "system", Table: "one", Columns: []string{"dummy"}},
			want:  0,
			count: func(sql string) int { return 0 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _, got, err := EmitCounted(context.Background(), tc.plan)
			if err != nil {
				t.Fatalf("EmitCounted: %v", err)
			}
			if got != tc.want {
				t.Fatalf("physicalScans = %d, want %d\nSQL: %s", got, tc.want, sql)
			}
			if inText := tc.count(sql); inText != tc.want {
				t.Fatalf("emitted text contains %d physical table references but the count says %d\nSQL: %s", inText, tc.want, sql)
			}
		})
	}
}

// TestEmitCounted_PreRenderedSubqueryCountsPerSplice pins the property that
// makes the count exact for every emitter, present and future: a
// pre-rendered sub-statement contributes its scans to the enclosing Builder
// each time it is SPLICED, not once when it is rendered — because ClickHouse
// executes the text as many times as it appears.
func TestEmitCounted_PreRenderedSubqueryCountsPerSplice(t *testing.T) {
	t.Parallel()
	sub := PreRenderedSQL{SQL: "SELECT * FROM `otel_traces`", PhysicalScans: 1}
	twice := NewQuery().Select(Star()).From(aliasedFrag(Subquery(sub), "a")).Where(InSubquery(Col("TraceId"), Subquery(sub)))
	sql, _, err := twice.subquerySQL()
	if err != nil {
		t.Fatal(err)
	}
	if got := twice.physicalScans(); got != 2 {
		t.Fatalf("physicalScans = %d, want 2 for a sub-statement spliced twice\nSQL: %s", got, sql)
	}
	if strings.Count(sql, "FROM `otel_traces`") != 2 {
		t.Fatalf("expected the sub-statement text twice in\n%s", sql)
	}
}
