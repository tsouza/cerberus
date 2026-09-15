package chsql_test

import "github.com/tsouza/cerberus/internal/chplan"

func classicQuantileTestInput() *chplan.Scan {
	roles := []chplan.Column{
		{Name: "BucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: "ExplicitBounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}
	return &chplan.Scan{Table: "otel_metrics_histogram", Columns: []string{"BucketCounts", "ExplicitBounds"}, Roles: roles}
}

func nativeQuantileTestInput(zeroThreshold bool) *chplan.Scan {
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
