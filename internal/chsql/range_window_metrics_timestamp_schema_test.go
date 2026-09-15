package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func metricsNestedTimestampScan(columns []string, roles []chplan.Column) *chplan.Scan {
	return &chplan.Scan{Table: "otel_traces", Columns: columns, Roles: roles}
}

func metricsTimestampTestScan(table, timestamp string) *chplan.Scan {
	return &chplan.Scan{Table: table, Roles: []chplan.Column{{Name: timestamp, Role: chplan.RoleTimestamp}}}
}

func metricsTimestampWindow(input chplan.Node) *chplan.RangeWindow {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input: input, Start: start, End: start.Add(time.Minute),
		Step: time.Minute, Range: time.Minute, TimestampColumn: "public_timestamp",
	}
}

func TestRangeWindowMetricsResolveNestedPhysicalTimestamp(t *testing.T) {
	t.Parallel()
	inner := metricsNestedTimestampScan(
		[]string{"physical_timestamp", "Duration"},
		[]chplan.Column{{Name: "physical_timestamp", Role: chplan.RoleTimestamp}},
	)
	tests := map[string]chplan.Node{
		"aggregate": &chplan.MetricsAggregate{
			Op: chplan.MetricsOpRate, ValueAlias: "Value", Inner: inner,
		},
		"histogram": &chplan.MetricsHistogramOverTime{
			Attr: &chplan.ColumnRef{Name: "Duration"}, BucketAlias: "__bucket", ValueAlias: "Value", Inner: inner,
		},
	}
	for name, input := range tests {
		input := input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := chsql.Emit(context.Background(), metricsTimestampWindow(input))
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, "dateDiff('nanosecond', `physical_timestamp`") {
				t.Fatalf("SQL does not fan out from nested physical timestamp: %s", sql)
			}
			if strings.Contains(sql, "dateDiff('nanosecond', `public_timestamp`") ||
				strings.Contains(sql, "WHERE `public_timestamp`") {
				t.Fatalf("SQL used RangeWindow output alias as nested input: %s", sql)
			}
		})
	}
}

func TestRangeWindowMetricsRejectMalformedNestedTimestampSchema(t *testing.T) {
	t.Parallel()
	tests := map[string]*chplan.Scan{
		"missing": metricsNestedTimestampScan([]string{"Duration"}, nil),
		"duplicate": metricsNestedTimestampScan(
			[]string{"time_a", "time_b", "Duration"},
			[]chplan.Column{
				{Name: "time_a", Role: chplan.RoleTimestamp},
				{Name: "time_b", Role: chplan.RoleTimestamp},
			},
		),
	}
	for name, inner := range tests {
		inner := inner
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inputs := []chplan.Node{
				&chplan.MetricsAggregate{Op: chplan.MetricsOpRate, ValueAlias: "Value", Inner: inner},
				&chplan.MetricsHistogramOverTime{
					Attr: &chplan.ColumnRef{Name: "Duration"}, ValueAlias: "Value", Inner: inner,
				},
			}
			for _, input := range inputs {
				if _, _, err := chsql.Emit(context.Background(), metricsTimestampWindow(input)); err == nil {
					t.Fatalf("Emit accepted malformed nested timestamp schema for %T", input)
				}
			}
		})
	}
}
