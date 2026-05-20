package schema

import (
	"os"
	"strings"
)

// Env var names recognised by the FromEnv factories. Listed as exported
// constants so docs / tests can reference them without re-typing the
// string and risking drift.
const (
	// EnvMetricsGaugeTable overrides Metrics.GaugeTable.
	EnvMetricsGaugeTable = "CERBERUS_SCHEMA_METRICS_GAUGE_TABLE"
	// EnvMetricsSumTable overrides Metrics.SumTable.
	EnvMetricsSumTable = "CERBERUS_SCHEMA_METRICS_SUM_TABLE"
	// EnvMetricsHistogramTable overrides Metrics.HistogramTable.
	EnvMetricsHistogramTable = "CERBERUS_SCHEMA_METRICS_HISTOGRAM_TABLE"
	// EnvMetricsExpHistogramTable overrides Metrics.ExpHistogramTable.
	EnvMetricsExpHistogramTable = "CERBERUS_SCHEMA_METRICS_EXP_HISTOGRAM_TABLE"
	// EnvMetricsSummaryTable overrides Metrics.SummaryTable.
	EnvMetricsSummaryTable = "CERBERUS_SCHEMA_METRICS_SUMMARY_TABLE"
	// EnvLogsTable overrides Logs.LogsTable.
	EnvLogsTable = "CERBERUS_SCHEMA_LOGS_TABLE"
	// EnvTracesTable overrides Traces.SpansTable.
	EnvTracesTable = "CERBERUS_SCHEMA_TRACES_TABLE"
)

// envOverride returns the trimmed value of key when set to a non-empty
// string, else def. An env var set to whitespace-only is treated as
// unset — operators paste values with stray newlines often enough that
// silently honouring them would produce table names like
// "otel_metrics_sum\n" that fail at query time with cryptic CH errors.
func envOverride(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// DefaultOTelMetricsFromEnv returns DefaultOTelMetrics() with any
// CERBERUS_SCHEMA_METRICS_*_TABLE env overrides applied. Unset or
// whitespace-only values leave the corresponding field at its default.
// Non-table fields (column names, rollups, suffixes) are not exposed
// as overrides — extend the surface here if a deployment demonstrates
// the need.
func DefaultOTelMetricsFromEnv() Metrics {
	m := DefaultOTelMetrics()
	m.GaugeTable = envOverride(EnvMetricsGaugeTable, m.GaugeTable)
	m.SumTable = envOverride(EnvMetricsSumTable, m.SumTable)
	m.HistogramTable = envOverride(EnvMetricsHistogramTable, m.HistogramTable)
	m.ExpHistogramTable = envOverride(EnvMetricsExpHistogramTable, m.ExpHistogramTable)
	m.SummaryTable = envOverride(EnvMetricsSummaryTable, m.SummaryTable)
	return m
}

// DefaultOTelLogsFromEnv returns DefaultOTelLogs() with the
// CERBERUS_SCHEMA_LOGS_TABLE override applied (if set).
func DefaultOTelLogsFromEnv() Logs {
	l := DefaultOTelLogs()
	l.LogsTable = envOverride(EnvLogsTable, l.LogsTable)
	return l
}

// DefaultOTelTracesFromEnv returns DefaultOTelTraces() with the
// CERBERUS_SCHEMA_TRACES_TABLE override applied (if set).
func DefaultOTelTracesFromEnv() Traces {
	t := DefaultOTelTraces()
	t.SpansTable = envOverride(EnvTracesTable, t.SpansTable)
	return t
}
