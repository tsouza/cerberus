package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestRegroupSampleTsSource_BindsOnlyTheShadowedColumn pins when the
// direct-matrix regroup rebinds the sample timestamp: exactly when its
// aggregate reads the column the regroup projects the anchor under
// ([anchorTimestampAlias]), which a same-SELECT alias would shadow. A
// window named `anchor_ts` projects the anchor as the schema timestamp
// column, so that is the column it shadows.
func TestRegroupSampleTsSource_BindsOnlyTheShadowedColumn(t *testing.T) {
	t.Parallel()
	inner := Col("x")
	cases := []struct {
		name     string
		windowTs string
		tsCol    string
		reads    bool
		want     string
	}{
		{"anchor-named window shadows the schema column", RangeWindowAnchorAlias, rangeWindowSchemaTimestampColumn, true, sampleTsAlias},
		{"anchor-named window leaves anchor_ts alone", RangeWindowAnchorAlias, RangeWindowAnchorAlias, true, RangeWindowAnchorAlias},
		{"schema-named window shadows its own column", rangeWindowSchemaTimestampColumn, rangeWindowSchemaTimestampColumn, true, sampleTsAlias},
		{"another input column is not shadowed", rangeWindowSchemaTimestampColumn, "other_ts", true, "other_ts"},
		{"an aggregate that reads no timestamp needs no rebinding", RangeWindowAnchorAlias, rangeWindowSchemaTimestampColumn, false, rangeWindowSchemaTimestampColumn},
		{"a window projecting no anchor shadows nothing", "", rangeWindowSchemaTimestampColumn, true, rangeWindowSchemaTimestampColumn},
	}
	for _, tc := range cases {
		r := &chplan.RangeWindow{TimestampColumn: tc.windowTs}
		_, got := regroupSampleTsSource(inner, tc.tsCol, r, overTimeDirectAgg{ReadsTimestamp: tc.reads})
		if got != tc.want {
			t.Errorf("%s: regroupSampleTsSource(tsCol=%q, window ts=%q, reads=%v) column = %q, want %q",
				tc.name, tc.tsCol, tc.windowTs, tc.reads, got, tc.want)
		}
	}
}
