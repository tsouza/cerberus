package schema

import "testing"

// TestDefaultOTelMetricsFromEnv_Unset confirms that with no env vars
// set, the env-aware factory returns the exact same value as the
// defaults-only factory. The override mechanism is additive: a deploy
// that sets nothing must see the upstream OTel CH Exporter layout.
func TestDefaultOTelMetricsFromEnv_Unset(t *testing.T) {
	for _, key := range []string{
		EnvMetricsGaugeTable,
		EnvMetricsSumTable,
		EnvMetricsHistogramTable,
		EnvMetricsExpHistogramTable,
		EnvMetricsSummaryTable,
	} {
		t.Setenv(key, "")
	}
	got := DefaultOTelMetricsFromEnv()
	want := DefaultOTelMetrics()
	if got.GaugeTable != want.GaugeTable ||
		got.SumTable != want.SumTable ||
		got.HistogramTable != want.HistogramTable ||
		got.ExpHistogramTable != want.ExpHistogramTable ||
		got.SummaryTable != want.SummaryTable {
		t.Errorf("FromEnv() with no overrides should equal Default(); got %+v, want %+v", got, want)
	}
	// Non-overridable fields must also pass through.
	if got.MetricNameColumn != want.MetricNameColumn {
		t.Errorf("MetricNameColumn drifted: got %q, want %q", got.MetricNameColumn, want.MetricNameColumn)
	}
	if len(got.MetricsRollups) != len(want.MetricsRollups) {
		t.Errorf("MetricsRollups length drifted: got %d, want %d", len(got.MetricsRollups), len(want.MetricsRollups))
	}
}

// TestDefaultOTelMetricsFromEnv_Overrides walks every overridable
// table field, sets the env var, and confirms only that field changes.
func TestDefaultOTelMetricsFromEnv_Overrides(t *testing.T) {
	cases := []struct {
		env   string
		value string
		pick  func(Metrics) string
	}{
		{EnvMetricsGaugeTable, "custom_gauge", func(m Metrics) string { return m.GaugeTable }},
		{EnvMetricsSumTable, "custom_sum", func(m Metrics) string { return m.SumTable }},
		{EnvMetricsHistogramTable, "custom_hist", func(m Metrics) string { return m.HistogramTable }},
		{EnvMetricsExpHistogramTable, "custom_exp", func(m Metrics) string { return m.ExpHistogramTable }},
		{EnvMetricsSummaryTable, "custom_summary", func(m Metrics) string { return m.SummaryTable }},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			got := DefaultOTelMetricsFromEnv()
			if g := tc.pick(got); g != tc.value {
				t.Errorf("%s: got %q, want %q", tc.env, g, tc.value)
			}
		})
	}
}

// TestDefaultOTelMetricsFromEnv_WhitespaceTreatedAsUnset confirms that
// whitespace-only values fall back to the default rather than producing
// table names with stray characters. Operators paste values with
// trailing newlines often enough that silently honouring them would
// surface as cryptic CH errors at query time.
func TestDefaultOTelMetricsFromEnv_WhitespaceTreatedAsUnset(t *testing.T) {
	t.Setenv(EnvMetricsSumTable, "   \n\t  ")
	got := DefaultOTelMetricsFromEnv()
	if got.SumTable != "otel_metrics_sum" {
		t.Errorf("whitespace override should fall back to default; got %q", got.SumTable)
	}
}

// TestDefaultOTelMetricsFromEnv_TrimsValue confirms surrounding
// whitespace is stripped from a non-empty override (same operator
// paste-with-newline scenario as the bool parsing).
func TestDefaultOTelMetricsFromEnv_TrimsValue(t *testing.T) {
	t.Setenv(EnvMetricsSumTable, "  custom_sum\n")
	got := DefaultOTelMetricsFromEnv()
	if got.SumTable != "custom_sum" {
		t.Errorf("trimmed override: got %q, want %q", got.SumTable, "custom_sum")
	}
}

// TestDefaultOTelLogsFromEnv_Unset / _Override mirror the metrics
// coverage for the logs surface.
func TestDefaultOTelLogsFromEnv_Unset(t *testing.T) {
	t.Setenv(EnvLogsTable, "")
	got := DefaultOTelLogsFromEnv()
	want := DefaultOTelLogs()
	if got.LogsTable != want.LogsTable {
		t.Errorf("LogsTable drift: got %q, want %q", got.LogsTable, want.LogsTable)
	}
	if got.BodyColumn != want.BodyColumn {
		t.Errorf("BodyColumn drift: got %q, want %q", got.BodyColumn, want.BodyColumn)
	}
}

func TestDefaultOTelLogsFromEnv_Override(t *testing.T) {
	t.Setenv(EnvLogsTable, "custom_logs")
	got := DefaultOTelLogsFromEnv()
	if got.LogsTable != "custom_logs" {
		t.Errorf("LogsTable override: got %q, want %q", got.LogsTable, "custom_logs")
	}
	// Column names must pass through unchanged.
	if got.BodyColumn != "Body" {
		t.Errorf("BodyColumn drifted while overriding LogsTable: got %q", got.BodyColumn)
	}
}

func TestDefaultOTelTracesFromEnv_Unset(t *testing.T) {
	t.Setenv(EnvTracesTable, "")
	got := DefaultOTelTracesFromEnv()
	want := DefaultOTelTraces()
	if got.SpansTable != want.SpansTable {
		t.Errorf("SpansTable drift: got %q, want %q", got.SpansTable, want.SpansTable)
	}
}

func TestDefaultOTelTracesFromEnv_Override(t *testing.T) {
	t.Setenv(EnvTracesTable, "custom_spans")
	got := DefaultOTelTracesFromEnv()
	if got.SpansTable != "custom_spans" {
		t.Errorf("SpansTable override: got %q, want %q", got.SpansTable, "custom_spans")
	}
	if got.TraceIDColumn != "TraceId" {
		t.Errorf("TraceIDColumn drifted while overriding SpansTable: got %q", got.TraceIDColumn)
	}
}
