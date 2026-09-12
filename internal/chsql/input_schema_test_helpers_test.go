package chsql_test

import "github.com/tsouza/cerberus/internal/chplan"

func closedTimestampTestScan(table, timestamp string, extra ...string) *chplan.Scan {
	roles := []chplan.Column{{Name: timestamp, Role: chplan.RoleTimestamp}}
	columns := []string{timestamp}
	for _, name := range extra {
		roles = append(roles, chplan.Column{Name: name})
		columns = append(columns, name)
	}
	return &chplan.Scan{Table: table, Columns: columns, Roles: roles}
}

func closedHistogramTestScan(table string, includeZeroThreshold bool) *chplan.Scan {
	roles := []chplan.Column{
		{Name: "Count", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldCount},
		{Name: "Sum", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldSum},
		{Name: "Scale", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldScale},
		{Name: "ZeroCount", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroCount},
		{Name: "PositiveOffset", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveOffset},
		{Name: "PositiveBucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveBucketCounts},
		{Name: "NegativeOffset", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeOffset},
		{Name: "NegativeBucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeBucketCounts},
	}
	if includeZeroThreshold {
		roles = append(roles, chplan.Column{Name: "ZeroThreshold", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroThreshold})
	}
	columns := make([]string, len(roles))
	for i, role := range roles {
		columns[i] = role.Name
	}
	return &chplan.Scan{Table: table, Columns: columns, Roles: roles}
}
