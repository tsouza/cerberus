package chopt

import (
	"slices"
	"testing"
)

// TestCancellationGaps pins the gaps reported for the builds the real-server
// cancellation test runs against, plus the supported floor.
func TestCancellationGaps(t *testing.T) {
	cases := []struct {
		build string
		want  []string
	}{
		{"24.8.14.39", []string{"ClickHouse#108192", "ClickHouse#112483"}},
		{"26.6.1.1193", []string{"ClickHouse#108192", "ClickHouse#112483"}},
		{"26.7.1.445", []string{"ClickHouse#108192", "ClickHouse#112483"}},
		{"26.7.13.12", []string{"ClickHouse#112483"}},
		{"26.8.1.580", []string{"ClickHouse#112483"}},
		{"26.8.10.6", nil},
		// Vendor builds are judged by line: a gap stays until the line is
		// past the fix's line.
		{"26.7.9.10001.altinitystable", []string{"ClickHouse#108192", "ClickHouse#112483"}},
		{"26.8.9.10001.altinitystable", []string{"ClickHouse#112483"}},
		{"26.9.1.10001.altinitystable", nil},
	}
	for _, tc := range cases {
		server, ok := ParseVersion(tc.build)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", tc.build)
		}
		var got []string
		for _, g := range CancellationGaps(server) {
			got = append(got, g.Defect)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("CancellationGaps(%s) = %v; want %v", tc.build, got, tc.want)
		}
	}
}
