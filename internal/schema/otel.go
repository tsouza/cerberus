package schema

// Metrics describes how cerberus reads metrics from ClickHouse. The default
// (returned by DefaultOTelMetrics) matches the OpenTelemetry ClickHouse
// Exporter v0.x schema; users with custom layouts override individual
// fields via Config.
type Metrics struct {
	// GaugeTable is the table holding gauge metrics.
	GaugeTable string
	// SumTable is the table holding sum / counter metrics.
	SumTable string
	// HistogramTable is the table holding histogram metrics.
	HistogramTable string

	// MetricNameColumn names the column holding the metric name.
	MetricNameColumn string
	// AttributesColumn names the column holding metric labels (Map(String, String)).
	AttributesColumn string
	// ResourceAttributesColumn names the column holding resource labels.
	ResourceAttributesColumn string
	// TimestampColumn names the timestamp column (DateTime64).
	TimestampColumn string
	// ValueColumn names the numeric value column (Float64).
	ValueColumn string
}

// DefaultOTelMetrics returns the schema produced by the upstream OTel
// ClickHouse Exporter.
func DefaultOTelMetrics() Metrics {
	return Metrics{
		GaugeTable:               "otel_metrics_gauge",
		SumTable:                 "otel_metrics_sum",
		HistogramTable:           "otel_metrics_histogram",
		MetricNameColumn:         "MetricName",
		AttributesColumn:         "Attributes",
		ResourceAttributesColumn: "ResourceAttributes",
		TimestampColumn:          "TimeUnix",
		ValueColumn:              "Value",
	}
}

// TableFor picks which metrics table a PromQL metric name belongs in. For
// the v0.1 seed we use a Prom-naming heuristic — `_count`, `_total`, `_sum`,
// `_bucket` suffixes are treated as cumulative (Sum table); everything else
// goes to the Gauge table. This is the same convention the Prometheus
// remote-write integration uses for OTel metrics.
func (m Metrics) TableFor(metricName string) string {
	for _, suf := range []string{"_count", "_total", "_sum", "_bucket"} {
		if hasSuffix(metricName, suf) {
			return m.SumTable
		}
	}
	return m.GaugeTable
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
