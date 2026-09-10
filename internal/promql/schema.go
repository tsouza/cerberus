package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// metricRoles declares the public sample names at their schema-owning lowering.
// Project/Aggregate use this declaration only for outputs they actually emit.
func metricRoles(s schema.Metrics) []chplan.Column {
	return []chplan.Column{
		{Name: s.MetricNameColumn, Role: chplan.RoleMetricName},
		{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
		{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		{Name: s.ValueColumn, Role: chplan.RoleValue},
	}
}

func metricScanRoles(s schema.Metrics, table string) []chplan.Column {
	roles := metricRoles(s)
	if (s.DeltaPrefixTable != "" && table == s.DeltaPrefixTable) || table == schema.DownsampleTierTable {
		// These storage relations contain bucket boundaries and aggregate
		// states, not the raw sample timestamp/value columns.
		return roles[:2]
	}
	if table == s.HistogramTable || table == s.ExpHistogramTable {
		// Histogram storage has no float Value; the histogram lowering
		// explicitly synthesizes any scalar placeholder in a later Project.
		roles = roles[:len(roles)-1]
		names := []string{s.CountColumn, s.SumColumn}
		if table == s.ExpHistogramTable {
			names = append(names, s.ScaleColumn, s.ZeroThresholdColumn, s.ZeroCountColumn, s.PositiveOffsetColumn,
				s.PositiveBucketCountsColumn, s.NegativeOffsetColumn, s.NegativeBucketCountsColumn)
		} else {
			names = append(names, s.BucketCountsColumn, s.ExplicitBoundsColumn)
		}
		for _, name := range names {
			if name != "" {
				roles = append(roles, chplan.Column{Name: name, Role: chplan.RoleHistogramField})
			}
		}
	}
	return roles
}
