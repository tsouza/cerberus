package schema

import (
	"slices"
	"testing"
)

// The bare-column sorting-key prefixes pin the OTel-CH ORDER BY tuples the
// optimize_aggregation_in_order eligibility check relies on. If an upstream
// exporter bump reorders a sort key, this test fails loudly rather than the
// eligibility check silently stamping (or missing) the setting.

func TestMetricsSortingKeyPrefix_Default(t *testing.T) {
	got := DefaultOTelMetrics().SortingKeyPrefix()
	want := []string{"MetricName", "Attributes", "ServiceName"}
	if !slices.Equal(got, want) {
		t.Errorf("metrics SortingKeyPrefix = %v; want %v", got, want)
	}
}

func TestTracesSortingKeyPrefix_Default(t *testing.T) {
	got := DefaultOTelTraces().SortingKeyPrefix()
	want := []string{"ServiceName", "SpanName"}
	if !slices.Equal(got, want) {
		t.Errorf("traces SortingKeyPrefix = %v; want %v", got, want)
	}
}

// Logs lead with a function-wrapped key element, so there is no bare-column
// prefix: optimize_aggregation_in_order is never eligible for logs.
func TestLogsSortingKeyPrefix_Empty(t *testing.T) {
	if got := DefaultOTelLogs().SortingKeyPrefix(); len(got) != 0 {
		t.Errorf("logs SortingKeyPrefix = %v; want empty", got)
	}
}

// SortingKeyPrefix honours overridden column names so a custom schema's sort
// key is reported with the operator's column names, not the OTel defaults.
func TestMetricsSortingKeyPrefix_HonoursColumnOverrides(t *testing.T) {
	m := DefaultOTelMetrics()
	m.MetricNameColumn = "Name"
	m.AttributesColumn = "Labels"
	m.ServiceNameColumn = "Svc"
	got := m.SortingKeyPrefix()
	want := []string{"Name", "Labels", "Svc"}
	if !slices.Equal(got, want) {
		t.Errorf("overridden metrics SortingKeyPrefix = %v; want %v", got, want)
	}
}
