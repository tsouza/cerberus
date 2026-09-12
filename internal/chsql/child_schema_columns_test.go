package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func closedRoleProject(columns ...chplan.Column) chplan.Node {
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{Expr: &chplan.LitFloat{V: 1}, Alias: column.Name}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Roles: columns, Projections: projections}
}

func TestInputOnlyChildColumnsResolvePhysicalNames(t *testing.T) {
	t.Parallel()
	const timestamp = "physical_timestamp"
	child := closedRoleProject(chplan.Column{Name: timestamp, Role: chplan.RoleTimestamp})
	got, err := timestampChildColumn("test", child)
	if err != nil {
		t.Fatalf("timestampChildColumn: %v", err)
	}
	if got != timestamp {
		t.Fatalf("timestamp = %q, want %q", got, timestamp)
	}
}

func TestInputOnlyChildColumnsRejectMalformedSchemas(t *testing.T) {
	t.Parallel()
	tests := map[string]chplan.Node{
		"open":    &chplan.Scan{Roles: []chplan.Column{{Name: "ts", Role: chplan.RoleTimestamp}}},
		"missing": closedRoleProject(chplan.Column{Name: "v", Role: chplan.RoleValue}),
		"duplicate": closedRoleProject(
			chplan.Column{Name: "ts1", Role: chplan.RoleTimestamp},
			chplan.Column{Name: "ts2", Role: chplan.RoleTimestamp},
		),
		"unnamed": &chplan.Project{
			Input:       &chplan.OneRow{},
			Roles:       []chplan.Column{{Role: chplan.RoleTimestamp}},
			Projections: []chplan.Projection{{Expr: &chplan.LitFloat{V: 1}}},
		},
		"incompatible": closedRoleProject(
			chplan.Column{Name: "shared", Role: chplan.RoleTimestamp},
			chplan.Column{Name: "shared", Role: chplan.RoleValue},
		),
	}
	for name, child := range tests {
		child := child
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := timestampChildColumn("test", child); err == nil {
				t.Fatal("malformed child schema was accepted")
			}
		})
	}
}

func TestHistogramProjectionResolvesPhysicalPayloadNames(t *testing.T) {
	t.Parallel()
	fields := []struct {
		name  string
		field chplan.HistogramField
	}{
		{"physical_count", chplan.HistogramFieldCount},
		{"physical_sum", chplan.HistogramFieldSum},
		{"physical_scale", chplan.HistogramFieldScale},
		{"physical_zero_threshold", chplan.HistogramFieldZeroThreshold},
		{"physical_zero_count", chplan.HistogramFieldZeroCount},
		{"physical_positive_offset", chplan.HistogramFieldPositiveOffset},
		{"physical_positive_buckets", chplan.HistogramFieldPositiveBucketCounts},
		{"physical_negative_offset", chplan.HistogramFieldNegativeOffset},
		{"physical_negative_buckets", chplan.HistogramFieldNegativeBucketCounts},
	}
	roles := make([]chplan.Column, len(fields))
	for i, field := range fields {
		roles[i] = chplan.Column{Name: field.name, Role: chplan.RoleHistogramField, HistogramField: field.field}
	}
	plan := &chplan.HistogramProjection{
		Input:                      closedRoleProject(roles...),
		CountColumn:                "legacy_count",
		SumColumn:                  "legacy_sum",
		ScaleColumn:                "legacy_scale",
		ZeroThresholdColumn:        "legacy_zero_threshold",
		ZeroCountColumn:            "legacy_zero_count",
		PositiveOffsetColumn:       "legacy_positive_offset",
		PositiveBucketCountsColumn: "legacy_positive_buckets",
		NegativeOffsetColumn:       "legacy_negative_offset",
		NegativeBucketCountsColumn: "legacy_negative_buckets",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, field := range fields {
		if !strings.Contains(sql, "`"+field.name+"`") {
			t.Errorf("SQL does not read resolved child column %q: %s", field.name, sql)
		}
	}
}

func TestAbsentOverTimeResolvesInputTimestampAndPreservesOutputAlias(t *testing.T) {
	t.Parallel()
	const inputTimestamp = "physical_sample_time"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.AbsentOverTime{
		Input:            closedRoleProject(chplan.Column{Name: inputTimestamp, Role: chplan.RoleTimestamp}),
		Range:            time.Minute,
		Start:            at,
		End:              at,
		TimestampColumn:  "TimeUnix",
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		ValueColumn:      "Value",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "groupArray(`"+inputTimestamp+"`)") {
		t.Fatalf("SQL does not read resolved input timestamp: %s", sql)
	}
	if !strings.Contains(sql, "AS `TimeUnix`") {
		t.Fatalf("SQL changed the public output timestamp alias: %s", sql)
	}
}

func TestHistogramChildColumnRejectsIncompatibleIdentityRole(t *testing.T) {
	t.Parallel()
	child := closedRoleProject(chplan.Column{
		Name: "count", Role: chplan.RoleValue, HistogramField: chplan.HistogramFieldCount,
	})
	if _, err := histogramChildColumn(child, chplan.HistogramFieldCount, "histogram count"); err == nil {
		t.Fatal("histogram identity carried by an incompatible role was accepted")
	}
}
