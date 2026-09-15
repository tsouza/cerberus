package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func classicQuantileInput() *chplan.Scan {
	roles := []chplan.Column{
		{Name: "BucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: "ExplicitBounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}
	return &chplan.Scan{Table: "otel_metrics_histogram", Columns: []string{"BucketCounts", "ExplicitBounds"}, Roles: roles}
}

func nativeQuantileInput(zeroThreshold bool) *chplan.Scan {
	roles := []chplan.Column{
		{Name: "Count", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldCount},
		{Name: "Sum", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldSum},
		{Name: "Scale", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldScale},
		{Name: "ZeroThreshold", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroThreshold},
		{Name: "ZeroCount", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroCount},
		{Name: "PositiveOffset", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveOffset},
		{Name: "PositiveBucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveBucketCounts},
		{Name: "NegativeOffset", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeOffset},
		{Name: "NegativeBucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeBucketCounts},
	}
	columns := make([]string, 0, len(roles))
	filtered := make([]chplan.Column, 0, len(roles))
	for _, role := range roles {
		if role.HistogramField == chplan.HistogramFieldZeroThreshold && !zeroThreshold {
			continue
		}
		columns = append(columns, role.Name)
		filtered = append(filtered, role)
	}
	return &chplan.Scan{Table: "otel_metrics_exponential_histogram", Columns: columns, Roles: filtered}
}

func TestQuantileConsumersResolvePhysicalHistogramFields(t *testing.T) {
	classic := classicQuantileInput()
	classic.Columns = []string{"physical_counts", "physical_bounds"}
	classic.Roles[0].Name, classic.Roles[1].Name = classic.Columns[0], classic.Columns[1]
	sql, _, err := Emit(context.Background(), &chplan.HistogramQuantile{
		Input: classic, Phi: 0.5, BucketCountsColumn: "legacy_counts", ExplicitBoundsColumn: "legacy_bounds",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "`physical_counts`") || !strings.Contains(sql, "`physical_bounds`") {
		t.Fatalf("classic quantile did not resolve physical child fields: %s", sql)
	}

	native := nativeQuantileInput(true)
	for i := range native.Columns {
		native.Columns[i] = "physical_" + native.Columns[i]
		native.Roles[i].Name = native.Columns[i]
	}
	sql, _, err = Emit(context.Background(), &chplan.HistogramQuantileNative{
		Input: native, Phi: 0.5, CountColumn: "legacy_count", SumColumn: "legacy_sum", ScaleColumn: "legacy_scale",
		ZeroThresholdColumn: "legacy_zero_threshold", ZeroCountColumn: "legacy_zero_count",
		PositiveOffsetColumn: "legacy_positive_offset", PositiveBucketCountsColumn: "legacy_positive_buckets",
		NegativeOffsetColumn: "legacy_negative_offset", NegativeBucketCountsColumn: "legacy_negative_buckets",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range native.Columns {
		if !strings.Contains(sql, "`"+name+"`") {
			t.Errorf("native quantile did not read %q", name)
		}
	}
}

func TestQuantileHistogramFieldRejectsMalformedSchemas(t *testing.T) {
	for name, input := range map[string]chplan.Node{
		"open": &chplan.Scan{Roles: classicQuantileInput().Roles},
		"missing": &chplan.Project{
			Input: &chplan.OneRow{}, Roles: []chplan.Column{{Name: "x"}},
			Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "x"}},
		},
		"wrong-role": &chplan.Project{
			Input:       &chplan.OneRow{},
			Roles:       []chplan.Column{{Name: "counts", Role: chplan.RoleOpaque, HistogramField: chplan.HistogramFieldBucketCounts}},
			Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "counts"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := quantileHistogramField("test", input, chplan.HistogramFieldBucketCounts, false); err == nil {
				t.Fatal("malformed schema accepted")
			}
		})
	}
}
