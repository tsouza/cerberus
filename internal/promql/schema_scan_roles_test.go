package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMetricScanRolesStorageContracts(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "custom_metric"
	s.AttributesColumn = "custom_attributes"
	s.TimestampColumn = "custom_timestamp"
	s.ValueColumn = "custom_value"
	s.ZeroThresholdColumn = "custom_zero_threshold"
	identity := []chplan.Column{
		{Name: s.MetricNameColumn, Role: chplan.RoleMetricName},
		{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
	}
	sample := append(append([]chplan.Column{}, identity...),
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		chplan.Column{Name: s.ValueColumn, Role: chplan.RoleValue})
	classic := append(append([]chplan.Column{}, identity...),
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		chplan.Column{Name: s.CountColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldCount},
		chplan.Column{Name: s.SumColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldSum},
		chplan.Column{Name: s.BucketCountsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		chplan.Column{Name: s.ExplicitBoundsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds})
	native := append(append([]chplan.Column{}, identity...),
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		chplan.Column{Name: s.CountColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldCount},
		chplan.Column{Name: s.SumColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldSum},
		chplan.Column{Name: s.ScaleColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldScale},
		chplan.Column{Name: s.ZeroThresholdColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroThreshold},
		chplan.Column{Name: s.ZeroCountColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldZeroCount},
		chplan.Column{Name: s.PositiveOffsetColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveOffset},
		chplan.Column{Name: s.PositiveBucketCountsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveBucketCounts},
		chplan.Column{Name: s.NegativeOffsetColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeOffset},
		chplan.Column{Name: s.NegativeBucketCountsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldNegativeBucketCounts})
	const configuredPrefixTable = "custom_delta_prefix"
	for _, tc := range []struct {
		name   string
		prefix string
		table  string
		want   []chplan.Column
	}{
		{name: "raw_prefix_disabled", table: s.GaugeTable, want: sample},
		{name: "raw_prefix_enabled", prefix: configuredPrefixTable, table: s.GaugeTable, want: sample},
		{name: "configured_prefix", prefix: configuredPrefixTable, table: configuredPrefixTable, want: identity},
		{name: "downsample_prefix_disabled", table: schema.DownsampleTierTable, want: identity},
		{name: "downsample_prefix_enabled", prefix: configuredPrefixTable, table: schema.DownsampleTierTable, want: identity},
		{name: "classic_prefix_disabled", table: s.HistogramTable, want: classic},
		{name: "classic_prefix_enabled", prefix: configuredPrefixTable, table: s.HistogramTable, want: classic},
		{name: "native_prefix_disabled", table: s.ExpHistogramTable, want: native},
		{name: "native_prefix_enabled", prefix: configuredPrefixTable, table: s.ExpHistogramTable, want: native},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configured := s
			configured.DeltaPrefixTable = tc.prefix
			want := chplan.Schema{Columns: tc.want}
			if got := (chplan.Schema{Columns: metricScanRoles(configured, tc.table)}); !got.Equal(want) {
				t.Fatalf("declared roles = %#v, want %#v", got, want)
			}
			// Verify the real scan constructor publishes the same roles; a
			// wildcard scan remains open, without claiming storage column order.
			scan := scanFromTables([]string{tc.table}, configured)
			want.Open = true
			if got := scan.RowType(); !got.Equal(want) {
				t.Fatalf("scan output = %#v, want %#v", got, want)
			}
		})
	}
}
