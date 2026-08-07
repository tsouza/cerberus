package testsql

import (
	"strings"
	"testing"
)

// TestBackfillMetricsColumns_TableScoping pins the per-table scoping of
// [backfilledColumns]. The registry serves two different needs and the
// difference is load-bearing:
//
//   - ResourceAttributes / ServiceName are unscoped — the read path projects
//     them off EVERY metric table, so every table must grow them.
//   - Count / Sum are scoped to the two histogram tables — production stores
//     them only there. Injecting them into the gauge or sum table would make
//     the fixture lie about the physical layout and would mask a real bug in
//     which a query projects a companion column off a table that cannot have
//     one.
//
// Without the scoping this test would still pass on the histogram rows and
// silently fail the gauge/sum ones, which is why both directions are asserted.
func TestBackfillMetricsColumns_TableScoping(t *testing.T) {
	cases := []struct {
		name      string
		table     string
		wantCount bool
	}{
		{name: "gauge omits Count/Sum", table: "otel_metrics_gauge", wantCount: false},
		{name: "sum omits Count/Sum", table: "otel_metrics_sum", wantCount: false},
		{name: "histogram gets Count/Sum", table: "otel_metrics_histogram", wantCount: true},
		{name: "exp histogram gets Count/Sum", table: "otel_metrics_exponential_histogram", wantCount: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ddl := "CREATE TABLE " + tc.table + " (\n" +
				"    MetricName String,\n" +
				"    Attributes Map(String, String),\n" +
				"    TimeUnix DateTime64(9)\n" +
				") ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix)"
			got := strings.Join(BackfillMetricsColumns([]string{ddl}), "")

			// The unscoped columns land on every table.
			for _, col := range []string{"ResourceAttributes", "ServiceName"} {
				if !strings.Contains(got, col) {
					t.Errorf("%s: missing unscoped column %s\nddl=%s", tc.table, col, got)
				}
			}
			for _, col := range []string{"Count UInt64", "Sum Float64"} {
				if has := strings.Contains(got, col); has != tc.wantCount {
					t.Errorf("%s: %q present=%v, want %v\nddl=%s", tc.table, col, has, tc.wantCount, got)
				}
			}
		})
	}
}

// TestBackfillMetricsColumns_DeclaredColumnsUntouched pins that the backfill
// never duplicates a column the seed already declares — a second `Count`
// definition would make the CREATE invalid rather than merely redundant.
func TestBackfillMetricsColumns_DeclaredColumnsUntouched(t *testing.T) {
	ddl := "CREATE TABLE otel_metrics_histogram (\n" +
		"    MetricName String,\n" +
		"    Attributes Map(String, String),\n" +
		"    Count UInt64,\n" +
		"    Sum Float64\n" +
		") ENGINE = MergeTree() ORDER BY (MetricName)"
	got := strings.Join(BackfillMetricsColumns([]string{ddl}), "")

	if n := strings.Count(got, "Count"); n != 1 {
		t.Errorf("Count appears %d times, want 1\nddl=%s", n, got)
	}
	if n := strings.Count(got, "Sum "); n != 1 {
		t.Errorf("Sum appears %d times, want 1\nddl=%s", n, got)
	}
}
