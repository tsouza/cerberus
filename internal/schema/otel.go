package schema

import "time"

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
	// ZeroThresholdColumn names the upper edge of the zero bucket
	// (Float64). Observations whose absolute value is at or below this
	// threshold are counted in ZeroCount.
	ZeroThresholdColumn string
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

	// MetricsRollups declares pre-aggregated rollup tables the
	// operator has provisioned alongside the base metrics tables. The
	// optimizer's MV-substitution rule (RC3 R3.6) reads this list to
	// decide whether a `RangeWindow` over a base table can be rewritten
	// to scan the matching rollup instead. The registry is the
	// operator's contract: cerberus trusts the listed tables exist and
	// carry the declared (Window, AggOp) semantics. Empty means "no
	// rollups available" — the rule will never fire.
	MetricsRollups []Rollup

	// ExpHistogramSuffix is the metric-name suffix used to route a
	// PromQL `histogram_quantile(phi, metric)` call to the exponential
	// (native) histogram table instead of the classic-histogram table.
	// PromQL itself has no naming convention for exp histograms (the
	// upstream Prom distinguishes them by wire-format tag), so cerberus
	// adopts a simple suffix-based heuristic for the v0.1 seed:
	// `foo_exp_hist` reads from `otel_metrics_exp_histogram`, everything
	// else stays on the classic table. Override the suffix via Config
	// for deployments that follow a different convention; an empty
	// string disables the routing entirely.
	ExpHistogramSuffix string
}

// RollupAggOp enumerates the per-bucket reducer the operator
// configured the upstream rollup table to compute. The optimizer uses
// this to check commutativity against the outer query's aggregate (sum
// over sums is total sum; max over maxes is total max; avg over avgs
// is NOT total avg without per-bucket weights, so RollupAggAvg is
// explicitly excluded from the v1 substitution).
type RollupAggOp string

const (
	// RollupAggSum names a rollup whose materialised column holds the
	// per-bucket sum of the base table's value column. Commutes with
	// outer `sum`.
	RollupAggSum RollupAggOp = "sum"
	// RollupAggCount names a rollup whose materialised column holds
	// the per-bucket sample count. Commutes with outer `count` (and
	// with outer `sum` when the per-bucket value is itself a count).
	RollupAggCount RollupAggOp = "count"
	// RollupAggMin names a rollup whose materialised column holds the
	// per-bucket minimum. Commutes with outer `min`.
	RollupAggMin RollupAggOp = "min"
	// RollupAggMax names a rollup whose materialised column holds the
	// per-bucket maximum. Commutes with outer `max`.
	RollupAggMax RollupAggOp = "max"
)

// Rollup describes a single pre-aggregated rollup table in the OTel
// metrics schema. The optimizer's MV-substitution rule (RC3 R3.6)
// rewrites a `RangeWindow(Scan(BaseTable))` to `RangeWindow(Scan(RollupTable))`
// when the query's step + range + aggregate operator are compatible
// with the rollup's window + commuting aggregate.
//
// The rollup table is expected to expose:
//
//   - The same series-identity columns as the base table (the
//     `Attributes` / `ResourceAttributes` / `ServiceName` columns are
//     copied through unchanged by the upstream OTel exporter).
//   - The same `TimestampColumn` aligned to the rollup window's
//     boundary (e.g. `toStartOfFiveMinute(TimeUnix) AS TimeUnix`).
//   - A `ValueColumn` carrying the pre-aggregated per-bucket value
//     (e.g. `Sum` for an AggOp=sum rollup, `Max` for AggOp=max, …).
//     The rule rewrites the `RangeWindow.ValueColumn` to this name
//     before re-emitting SQL.
type Rollup struct {
	// BaseTable names the source `otel_metrics_*` table the rollup
	// summarises (e.g. "otel_metrics_sum").
	BaseTable string
	// RollupTable names the materialised pre-aggregated table the
	// upstream OTel exporter writes (e.g. "otel_metrics_sum_5m").
	RollupTable string
	// Window is the rollup's bucket size — every row in RollupTable
	// represents one bucket of this width over the base table.
	Window time.Duration
	// AggOp is the per-bucket reducer the rollup applies to the base
	// table's value column. Determines which outer aggregates can
	// commute with the rollup.
	AggOp RollupAggOp
	// ValueColumn names the column on RollupTable that carries the
	// pre-aggregated per-bucket value. Almost always upper-cased
	// AggOp ("Sum" / "Count" / "Min" / "Max"). Stored explicitly so
	// custom-named rollups can override the convention.
	ValueColumn string
}

// Rollups returns the configured MetricsRollups list. Returned as a
// dedicated accessor so future filtering (e.g. selecting candidates
// for a given base table) can centralise here without touching every
// caller.
func (m Metrics) Rollups() []Rollup { return m.MetricsRollups }

// RollupsFor returns the rollups whose BaseTable equals base. Order
// is preserved from MetricsRollups; the rule walks the slice in this
// order and picks the first applicable candidate (the v1
// `firstApplicable` CostModel — see internal/optimizer/mv_substitution.go).
// Operators who care about candidate ordering should list the longest
// (coarsest) window first so the rule prefers the rollup that strips
// the most data.
func (m Metrics) RollupsFor(base string) []Rollup {
	var out []Rollup
	for _, r := range m.MetricsRollups {
		if r.BaseTable == base {
			out = append(out, r)
		}
	}
	return out
}

// defaultOTelRollups returns the canonical OTel CH exporter rollups.
// The upstream exporter only writes these tables when the operator
// explicitly enables the rollup feature, so the default schema
// advertises them — operators who haven't enabled rollups will simply
// never have the rule fire (the substitution checks for the rollup
// table existing logically via this registry; cerberus does not probe
// CH's `system.tables`).
//
// Two canonical rollups ship in the default schema:
//
//   - `otel_metrics_sum_5m` — five-minute sum buckets. Suits PromQL
//     `query_range` with 5-minute step.
//   - `otel_metrics_sum_1h` — one-hour sum buckets. Suits long-range
//     queries (24h, 7d) where five-minute resolution is overkill.
//
// Longest window first so `firstApplicable` prefers the coarsest
// rollup that still satisfies the query's step.
func defaultOTelRollups() []Rollup {
	return []Rollup{
		{
			BaseTable:   "otel_metrics_sum",
			RollupTable: "otel_metrics_sum_1h",
			Window:      time.Hour,
			AggOp:       RollupAggSum,
			ValueColumn: "Sum",
		},
		{
			BaseTable:   "otel_metrics_sum",
			RollupTable: "otel_metrics_sum_5m",
			Window:      5 * time.Minute,
			AggOp:       RollupAggSum,
			ValueColumn: "Sum",
		},
	}
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
		ZeroThresholdColumn:          "ZeroThreshold",
		PositiveOffsetColumn:         "PositiveOffset",
		PositiveBucketCountsColumn:   "PositiveBucketCounts",
		NegativeOffsetColumn:         "NegativeOffset",
		NegativeBucketCountsColumn:   "NegativeBucketCounts",
		ValueAtQuantilesColumn:       "ValueAtQuantiles",
		ExemplarsColumn:              "Exemplars",
		ExpHistogramSuffix:           "_exp_hist",
		MetricsRollups:               defaultOTelRollups(),
	}
}

// IsExpHistogramMetric reports whether the given metric name should be
// routed to the exponential / native histogram table. Returns false if
// ExpHistogramSuffix is empty (routing disabled).
func (m Metrics) IsExpHistogramMetric(metricName string) bool {
	if m.ExpHistogramSuffix == "" {
		return false
	}
	return hasSuffix(metricName, m.ExpHistogramSuffix)
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
