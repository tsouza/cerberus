package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestPinsExpHistogramMetric_NeedsNameTableAndSuffix pins that an
// absence-presence selector reads the exponential-histogram table only when
// all three hold: the selector pins a name, the schema stores
// exponential histograms in a table of their own, and the name carries the
// exponential-histogram suffix. A schema without that table routes the
// suffixed name like any other, even though the suffix still matches.
func TestPinsExpHistogramMetric_NeedsNameTableAndSuffix(t *testing.T) {
	t.Parallel()
	withTable := schema.DefaultOTelMetrics()
	if withTable.ExpHistogramTable == "" || withTable.ExpHistogramSuffix == "" {
		t.Fatal("the default schema routes no exponential histograms, so the cases below prove nothing")
	}
	noTable := withTable
	noTable.ExpHistogramTable = ""
	name := "lat" + withTable.ExpHistogramSuffix
	cases := []struct {
		name   string
		metric string
		s      schema.Metrics
		want   bool
	}{
		{name: "pinned suffixed name with the table", metric: name, s: withTable, want: true},
		{name: "unpinned", metric: "", s: withTable, want: false},
		{name: "name without the suffix", metric: "lat", s: withTable, want: false},
		{name: "no exponential-histogram table", metric: name, s: noTable, want: false},
	}
	for _, tc := range cases {
		if got := pinsExpHistogramMetric(tc.metric, tc.s); got != tc.want {
			t.Errorf("%s: pinsExpHistogramMetric(%q) = %v, want %v", tc.name, tc.metric, got, tc.want)
		}
	}
}
