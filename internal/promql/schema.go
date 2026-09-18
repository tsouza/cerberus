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
		// histogramStorageField pairs a configured storage column with the
		// canonical histogram field it holds; an empty name means the
		// schema does not persist that field.
		type histogramStorageField struct {
			name     string
			identity chplan.HistogramField
		}
		fields := []histogramStorageField{
			{s.CountColumn, chplan.HistogramFieldCount},
			{s.SumColumn, chplan.HistogramFieldSum},
		}
		if table == s.ExpHistogramTable {
			fields = append(
				fields,
				histogramStorageField{s.ScaleColumn, chplan.HistogramFieldScale},
				histogramStorageField{s.ZeroThresholdColumn, chplan.HistogramFieldZeroThreshold},
				histogramStorageField{s.ZeroCountColumn, chplan.HistogramFieldZeroCount},
				histogramStorageField{s.PositiveOffsetColumn, chplan.HistogramFieldPositiveOffset},
				histogramStorageField{s.PositiveBucketCountsColumn, chplan.HistogramFieldPositiveBucketCounts},
				histogramStorageField{s.NegativeOffsetColumn, chplan.HistogramFieldNegativeOffset},
				histogramStorageField{s.NegativeBucketCountsColumn, chplan.HistogramFieldNegativeBucketCounts},
			)
		} else {
			fields = append(
				fields,
				histogramStorageField{s.BucketCountsColumn, chplan.HistogramFieldBucketCounts},
				histogramStorageField{s.ExplicitBoundsColumn, chplan.HistogramFieldExplicitBounds},
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

// expHistogramRoles declares both the public sample columns and the physical
// exponential-histogram payload consumed by downstream plan nodes. Unlike a
// Scan declaration, it is also suitable for reshaping Project and Aggregate
// nodes whose output retains those physical fields.
func expHistogramRoles(s schema.Metrics) []chplan.Column {
	return metricScanRoles(s, s.ExpHistogramTable)
}

func roleNames(columns []chplan.Column) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		if column.Name != "" {
			names = append(names, column.Name)
		}
	}
	return names
}
