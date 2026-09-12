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
	const aggregateStorageIdentityColumns = 2
	roles := metricRoles(s)
	if (s.DeltaPrefixTable != "" && table == s.DeltaPrefixTable) || table == schema.DownsampleTierTable {
		// These storage relations contain bucket boundaries and aggregate
		// states, not the raw sample timestamp/value columns.
		return roles[:aggregateStorageIdentityColumns]
	}
	if table == s.HistogramTable || table == s.ExpHistogramTable {
		// Histogram storage has no float Value; the histogram lowering
		// explicitly synthesizes any scalar placeholder in a later Project.
		roles = roles[:len(roles)-1]
		fields := []struct {
			name     string
			identity chplan.HistogramField
		}{{s.CountColumn, chplan.HistogramFieldCount}, {s.SumColumn, chplan.HistogramFieldSum}}
		if table == s.ExpHistogramTable {
			fields = append(
				fields,
				struct {
					name     string
					identity chplan.HistogramField
				}{s.ScaleColumn, chplan.HistogramFieldScale},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.ZeroThresholdColumn, chplan.HistogramFieldZeroThreshold},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.ZeroCountColumn, chplan.HistogramFieldZeroCount},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.PositiveOffsetColumn, chplan.HistogramFieldPositiveOffset},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.PositiveBucketCountsColumn, chplan.HistogramFieldPositiveBucketCounts},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.NegativeOffsetColumn, chplan.HistogramFieldNegativeOffset},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.NegativeBucketCountsColumn, chplan.HistogramFieldNegativeBucketCounts},
			)
		} else {
			fields = append(
				fields,
				struct {
					name     string
					identity chplan.HistogramField
				}{s.BucketCountsColumn, chplan.HistogramFieldBucketCounts},
				struct {
					name     string
					identity chplan.HistogramField
				}{s.ExplicitBoundsColumn, chplan.HistogramFieldExplicitBounds},
			)
		}
		for _, field := range fields {
			if field.name != "" {
				roles = append(roles, chplan.Column{Name: field.name, Role: chplan.RoleHistogramField, HistogramField: field.identity})
			}
		}
	}
	if s.AggregationTemporalityColumn != "" &&
		(table == s.SumTable || table == s.HistogramTable || table == s.ExpHistogramTable) {
		roles = append(roles, chplan.Column{Name: s.AggregationTemporalityColumn, Role: chplan.RoleTemporality})
	}
	return roles
}
