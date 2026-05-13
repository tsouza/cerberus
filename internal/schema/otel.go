package schema

// Metrics describes how cerberus reads metrics from ClickHouse. The default
// (returned by DefaultOTelMetrics) matches the OpenTelemetry ClickHouse
// Exporter v0.x schema; users with custom layouts override individual
// fields via Config.
//
// The struct is a flat surface of every column the upstream
// `clickhouseexporter` writes across the five metrics tables
// (gauge / sum / histogram / exponential histogram / summary). Each
// PromQL slice reads the subset relevant to its target table. Adding a
// field here is cheap: emitters and the future `internal/schema/ddl/`
// package both look up names by struct field rather than by string
// constants scattered through the codebase.
type Metrics struct {
	// GaugeTable is the table holding gauge metrics.
	GaugeTable string
	// SumTable is the table holding sum / counter metrics.
	SumTable string
	// HistogramTable is the table holding classic (explicit-bucket)
	// histogram metrics.
	HistogramTable string
	// ExpHistogramTable is the table holding exponential / native
	// histogram metrics.
	ExpHistogramTable string
	// SummaryTable is the table holding summary metrics
	// (Prometheus-style quantile samples).
	SummaryTable string

	// MetricNameColumn names the column holding the metric name.
	MetricNameColumn string
	// AttributesColumn names the column holding metric labels (Map(String, String)).
	AttributesColumn string
	// ResourceAttributesColumn names the column holding resource labels.
	ResourceAttributesColumn string
	// TimestampColumn names the timestamp column (DateTime64).
	TimestampColumn string
	// StartTimeColumn names the per-series start-timestamp column
	// (DateTime64). Used by cumulative-to-rate conversions.
	StartTimeColumn string
	// ValueColumn names the numeric value column (Float64). Only the
	// gauge + sum tables have a Value column; on histogram /
	// exp-histogram the per-sample value is decomposed into
	// Count / Sum / BucketCounts / etc.
	ValueColumn string

	// MetricDescriptionColumn names the column carrying the OTel
	// metric description text (free-form help string).
	MetricDescriptionColumn string
	// MetricUnitColumn names the column carrying the OTel metric unit.
	MetricUnitColumn string

	// ServiceNameColumn names the dedicated LowCardinality service.name
	// column written by the upstream exporter (separate from the
	// ResourceAttributes map).
	ServiceNameColumn string
	// ScopeNameColumn names the instrumentation-scope name column.
	ScopeNameColumn string
	// ScopeVersionColumn names the instrumentation-scope version column.
	ScopeVersionColumn string
	// ScopeAttributesColumn names the instrumentation-scope attribute map.
	ScopeAttributesColumn string

	// FlagsColumn names the OTel data-point Flags column (UInt32 bitfield).
	FlagsColumn string

	// Classic histogram + summary columns.

	// CountColumn names the per-sample observation count
	// (UInt64; histogram, exp_histogram, summary).
	CountColumn string
	// SumColumn names the per-sample observation sum
	// (Float64; histogram, exp_histogram, summary).
	SumColumn string
	// MinColumn names the per-sample observation minimum
	// (Float64; histogram, exp_histogram).
	MinColumn string
	// MaxColumn names the per-sample observation maximum
	// (Float64; histogram, exp_histogram).
	MaxColumn string
	// BucketCountsColumn names the explicit-bucket counts array
	// (Array(UInt64); classic histogram).
	BucketCountsColumn string
	// ExplicitBoundsColumn names the explicit-bucket upper-bound array
	// (Array(Float64); classic histogram).
	ExplicitBoundsColumn string
	// AggregationTemporalityColumn names the AggregationTemporality
	// enum column (Int32; sum, histogram, exp_histogram).
	AggregationTemporalityColumn string
	// IsMonotonicColumn names the IsMonotonic boolean column
	// (Boolean; sum table only).
	IsMonotonicColumn string

	// Exponential-histogram columns.

	// ScaleColumn names the OTel exp-histogram scale column (Int32).
	ScaleColumn string
	// ZeroCountColumn names the count of observations that landed in
	// the zero bucket (UInt64).
	ZeroCountColumn string
	// PositiveOffsetColumn names the bucket-index offset for the
	// positive-range buckets (Int32).
	PositiveOffsetColumn string
	// PositiveBucketCountsColumn names the positive-range bucket
	// counts array (Array(UInt64)).
	PositiveBucketCountsColumn string
	// NegativeOffsetColumn names the bucket-index offset for the
	// negative-range buckets (Int32).
	NegativeOffsetColumn string
	// NegativeBucketCountsColumn names the negative-range bucket
	// counts array (Array(UInt64)).
	NegativeBucketCountsColumn string

	// Summary-specific column.

	// ValueAtQuantilesColumn names the Nested column carrying
	// (Quantile, Value) pairs for summary metrics.
	ValueAtQuantilesColumn string

	// Exemplars (Nested) — present on gauge / sum / histogram /
	// exp_histogram tables. ExemplarsColumn is the Nested column name;
	// individual sub-fields (FilteredAttributes / TimeUnix / Value /
	// SpanId / TraceId) follow Nested-access conventions
	// (`Exemplars.SpanId`, etc.).
	ExemplarsColumn string
}

// DefaultOTelMetrics returns the schema produced by the upstream OTel
// ClickHouse Exporter. Column names mirror the upstream
// `metrics_{gauge,sum,histogram,exp_histogram,summary}_table.sql`
// templates verbatim.
func DefaultOTelMetrics() Metrics {
	return Metrics{
		GaugeTable:                   "otel_metrics_gauge",
		SumTable:                     "otel_metrics_sum",
		HistogramTable:               "otel_metrics_histogram",
		ExpHistogramTable:            "otel_metrics_exp_histogram",
		SummaryTable:                 "otel_metrics_summary",
		MetricNameColumn:             "MetricName",
		AttributesColumn:             "Attributes",
		ResourceAttributesColumn:     "ResourceAttributes",
		TimestampColumn:              "TimeUnix",
		StartTimeColumn:              "StartTimeUnix",
		ValueColumn:                  "Value",
		MetricDescriptionColumn:      "MetricDescription",
		MetricUnitColumn:             "MetricUnit",
		ServiceNameColumn:            "ServiceName",
		ScopeNameColumn:              "ScopeName",
		ScopeVersionColumn:           "ScopeVersion",
		ScopeAttributesColumn:        "ScopeAttributes",
		FlagsColumn:                  "Flags",
		CountColumn:                  "Count",
		SumColumn:                    "Sum",
		MinColumn:                    "Min",
		MaxColumn:                    "Max",
		BucketCountsColumn:           "BucketCounts",
		ExplicitBoundsColumn:         "ExplicitBounds",
		AggregationTemporalityColumn: "AggregationTemporality",
		IsMonotonicColumn:            "IsMonotonic",
		ScaleColumn:                  "Scale",
		ZeroCountColumn:              "ZeroCount",
		PositiveOffsetColumn:         "PositiveOffset",
		PositiveBucketCountsColumn:   "PositiveBucketCounts",
		NegativeOffsetColumn:         "NegativeOffset",
		NegativeBucketCountsColumn:   "NegativeBucketCounts",
		ValueAtQuantilesColumn:       "ValueAtQuantiles",
		ExemplarsColumn:              "Exemplars",
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
