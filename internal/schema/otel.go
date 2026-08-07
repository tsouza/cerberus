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

	// TableOverrides records which of the five table names above an
	// operator SET, as opposed to inheriting from DefaultOTelMetrics.
	// Only the configuration-reading resolvers populate it
	// (DefaultOTelMetricsFrom and its FromEnv wrapper); a Metrics built by
	// DefaultOTelMetrics alone leaves it zero, which reads correctly as
	// "every table name here is a built-in default".
	TableOverrides MetricTableOverrides

	// MetricNameColumn names the column holding the metric name.
	MetricNameColumn string
	// AttributesColumn names the column holding metric labels (Map(String, String)).
	AttributesColumn string
	// ResourceAttributesColumn names the column holding resource labels.
	ResourceAttributesColumn string
	// PromResourceLabels is the allowlist of OTel ResourceAttributes keys
	// (in their original dotted form, e.g. "k8s.namespace.name",
	// "deployment.environment.name") projected as Prometheus labels on the
	// PromQL read path (bare selector / aggregation / /series) and on the
	// metadata surface (/labels, /label/<name>/values). Keys are matched
	// against the ORIGINAL dotted OTel key; the wire emits the dot ->
	// underscore sanitized form (k8s.namespace.name -> k8s_namespace_name).
	//
	// nil / empty promotes EVERY ResourceAttributes key (the default — the
	// allowlist is opt-IN narrowing, not opt-in feature-enable). A custom
	// schema that clears ResourceAttributesColumn disables the resource arm
	// entirely regardless of this list. Wired from
	// CERBERUS_PROM_RESOURCE_LABELS (comma-separated).
	//
	// Caveat: keys containing characters OTHER than dots that sanitize to
	// underscore (e.g. '-', '/', ':') are surfaced on /labels and the
	// bare-selector projection but are NOT addressable by an underscored
	// matcher or /label/<name>/values, because the candidate chain only
	// reverses underscore -> dot (mirrors the leading-digit caveat).
	PromResourceLabels []string
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
	//
	// Empty means the physical schema does not persist the OTLP
	// zero_threshold field — the upstream OTel-CH exporter's
	// exp-histogram DDL (sqltemplates/metrics_exp_histogram_table.sql)
	// has no such column — and the native-quantile emitter uses a
	// constant 0 zero-bucket width instead (every zero-bucket
	// observation interpolates to exactly 0).
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
	// registry is the operator's contract: the listed tables are trusted
	// to exist and carry the declared (Window, AggOp) semantics.
	//
	// NOTE: as of 2026-06 no optimizer rule consumes this registry — the
	// MV-substitution rule that read it was retired (no rollup roadmap;
	// it was a guaranteed no-op against the shipped schemas). The type
	// and the default entries are retained as the schema-side contract a
	// future rollup-substitution rule would re-consume.
	MetricsRollups []Rollup

	// ExpHistogramSuffix is the metric-name suffix used to route a
	// PromQL `histogram_quantile(phi, metric)` call to the exponential
	// (native) histogram table instead of the classic-histogram table.
	// PromQL itself has no naming convention for exp histograms (the
	// upstream Prom distinguishes them by wire-format tag), so cerberus
	// adopts a simple suffix-based heuristic for the v0.1 seed:
	// `foo_exp_hist` reads from `otel_metrics_exponential_histogram`, everything
	// else stays on the classic table. Override the suffix via Config
	// for deployments that follow a different convention; an empty
	// string disables the routing entirely.
	ExpHistogramSuffix string
}

// RollupAggOp enumerates the per-bucket reducer the operator
// configured the upstream rollup table to compute. A rollup-substitution
// rule would use this to check commutativity against the outer query's
// aggregate (sum over sums is total sum; max over maxes is total max;
// avg over avgs is NOT total avg without per-bucket weights, so an avg
// rollup would be excluded). No optimizer rule consumes this today (see
// MetricsRollups).
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
// metrics schema. A rollup-substitution rule would rewrite a
// `RangeWindow(Scan(BaseTable))` to `RangeWindow(Scan(RollupTable))`
// when the query's step + range + aggregate operator are compatible
// with the rollup's window + commuting aggregate. No optimizer rule
// consumes this today — the MV-substitution rule that did was retired in
// 2026-06 (see MetricsRollups); the type stays as the schema-side
// contract a future rule would re-consume.
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
// is preserved from MetricsRollups. No optimizer rule consumes this
// today (the MV-substitution rule that did was retired in 2026-06); a
// future rollup-substitution rule that walks the slice in order should
// have operators list the longest (coarsest) window first so it prefers
// the rollup that strips the most data.
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
// Longest window first so a rollup-substitution rule walking the slice
// in order (see RollupsFor) prefers the coarsest rollup that still
// satisfies the query's step.
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
		ExpHistogramTable:            "otel_metrics_exponential_histogram",
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
		// The upstream OTel-CH exp-histogram DDL does NOT persist the
		// OTLP zero_threshold field (no ZeroThreshold column exists in
		// sqltemplates/metrics_exp_histogram_table.sql, any released
		// version). Referencing one made every native histogram_quantile
		// fail at execution with "Unknown expression identifier
		// 'ZeroThreshold'" against a real OTel-CH stack — surfaced by
		// the showcase-promql native-quantile panel. Empty opts into
		// the constant-0 zero-bucket width in the chsql emitter.
		ZeroThresholdColumn:        "",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		ValueAtQuantilesColumn:     "ValueAtQuantiles",
		ExemplarsColumn:            "Exemplars",
		ExpHistogramSuffix:         "_exp_hist",
		MetricsRollups:             defaultOTelRollups(),
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

// HistogramCompanionColumn reports whether the given PromQL metric name
// is a classic-histogram companion series — i.e. a `<base>_count` or
// `<base>_sum` reference that, in the OTel-CH layout, lives as a column
// on a single row written under the bare `<base>` name in the
// histogram table rather than as its own MetricName.
//
// Returns:
//
//   - `bareName`  — `<base>` (suffix stripped) when applicable, or the
//     unchanged input when the suffix doesn't match.
//   - `valueColumn` — the OTel-CH column to project as the
//     PromQL `Value` for this companion: [Metrics.CountColumn] for
//     `_count`, [Metrics.SumColumn] for `_sum`. Empty when not applicable.
//   - `ok` — true when the rewrite applies.
//
// Rationale: the Prometheus convention exposes classic-histogram series
// under three companion names — `<base>_bucket`, `<base>_count`,
// `<base>_sum`. The OTel-CH exporter stores all three pieces of
// information on a single row written under the bare metric name
// (`<base>`), with `BucketCounts` / `Count` / `Sum` as columns. So a
// PromQL `rate(<base>_count[5m])` against the OTel-CH layout must
// translate to `rate(toFloat64(Count)[5m])` over rows filtered by
// `MetricName='<base>'` against the histogram table — anything else
// silently returns "No data".
//
// This is the `_count` / `_sum` analogue of the `_bucket` suffix strip
// done by `stripBucketSuffix` (PR #637 / #182). All three suffixes are
// classic-histogram companion conventions; the OTel-CH histogram
// exporter never writes data under any of them.
//
// Disambiguation: in principle a deployment could expose a counter
// under a name that happens to end in `_count` or `_sum`. The OTel-CH
// counter convention uses `_total`, so the collision is rare in
// practice. Both read paths resolve the ambiguity by fanning across
// every physical layout that can carry the name rather than by
// committing to one: [Metrics.TablesFor] for samples,
// [Metrics.ExemplarSources] for exemplars.
func (m Metrics) HistogramCompanionColumn(metricName string) (bareName, valueColumn string, ok bool) {
	for _, c := range m.histogramCompanions() {
		if hasSuffix(metricName, c.suffix) {
			return metricName[:len(metricName)-len(c.suffix)], c.column, true
		}
	}
	return metricName, "", false
}

// histogramCompanion pairs a Prom-wire classic-histogram companion suffix
// with the OTel-CH histogram-row column that serves it.
type histogramCompanion struct {
	suffix string
	column string
}

// histogramCompanions is the single list of column-backed classic-histogram
// companion suffixes. Every site that needs to know which `<base>_<suffix>`
// names the histogram row serves — the suffix strip in
// [Metrics.HistogramCompanionColumn], the candidate-table fan-out in
// [Metrics.TablesFor], and the synthetic-name enumeration the metadata
// surfaces drive off [Metrics.HistogramCompanionSuffixes] — reads it from
// here, so the three can never drift apart.
//
// `_bucket` is deliberately absent: it is served by fanning the
// BucketCounts × ExplicitBounds arrays into one row per boundary rather than
// by projecting a single column, so its routing lives with the fan-out
// lowering instead of in this pair list.
func (m Metrics) histogramCompanions() []histogramCompanion {
	return []histogramCompanion{
		{suffix: "_count", column: m.CountColumn},
		{suffix: "_sum", column: m.SumColumn},
	}
}

// HistogramCompanionSuffixes returns the column-backed classic-histogram
// companion suffixes in stable order — the `<base>_<suffix>` names a caller
// must offer to [Metrics.HistogramCompanionColumn] to enumerate everything the
// histogram row can serve through a single-column projection.
func (m Metrics) HistogramCompanionSuffixes() []string {
	companions := m.histogramCompanions()
	out := make([]string, 0, len(companions))
	for _, c := range companions {
		out = append(out, c.suffix)
	}
	return out
}

// TableFor picks which metrics table a PromQL metric name belongs in. For
// the v0.1 seed we use a Prom-naming heuristic — `_count`, `_total`, `_sum`,
// `_bucket` suffixes are treated as cumulative (Sum table); everything else
// goes to the Gauge table. This is the same convention the Prometheus
// remote-write integration uses for OTel metrics.
//
// The single-table return is preserved for byte-stable SQL on the cases
// where the suffix heuristic is reliable. For ambiguous unsuffixed names —
// OTel hostmetrics receiver emits cumulative sums under bare names like
// `system_cpu_time` / `system_disk_io`, and the sqlquery receiver emits
// `clickhouse_event` — callers should consult [Metrics.TablesFor] which
// returns the (Gauge, Sum) pair so the matcher resolves against either
// physical layout. See TablesFor for the read-side rationale.
func (m Metrics) TableFor(metricName string) string {
	for _, suf := range []string{"_count", "_total", "_sum", "_bucket"} {
		if hasSuffix(metricName, suf) {
			return m.SumTable
		}
	}
	return m.GaugeTable
}

// TablesFor returns the metric tables a PromQL `__name__` matcher may
// resolve against. Where [Metrics.TableFor] commits to a single table
// based on a Prom-naming suffix, TablesFor admits the OTel reality:
// upstream emitters (hostmetrics, sqlquery, prometheus/self) ship
// cumulative sums under bare names that the Prom convention reserves
// for gauges. The catalog endpoints (`/api/v1/series`,
// `/api/v1/label/...`) already UNION across all metric tables; the
// matcher-resolve side must do the same or the dashboard surface
// returns "Unable to fetch labels" for every system_/clickhouse_*
// metric whose data lives in `otel_metrics_sum` but whose name doesn't
// trip the suffix check.
//
// The return slice is the candidate set in stable order:
//
//   - Unsuffixed names — `[Gauge, Sum]` (Gauge first for byte-stable
//     SQL on the simple-gauge case, then Sum for the hostmetrics-shaped
//     fallback).
//   - `_count` / `_sum` suffixed names — `[Histogram, Sum]` (Histogram
//     first to preserve the existing classic-histogram-companion
//     emit shape; Sum second to catch the OTel-hostmetrics shape where
//     `system_cpu_logical_count` / `system_processes_count` /
//     `system_filesystem_inodes_count` / `system_processes_created_count`
//     etc. ship as cumulative Sums under the suffixed name rather than
//     as histogram companions). Sum is dropped from the slice when no
//     Sum table is configured or when Sum equals Histogram.
//   - `_total` / `_bucket` suffixed names — single-element
//     `[SumTable]` (PromQL counters / classic-histogram bucket
//     selectors don't have the Sum-as-emitter-fallback ambiguity).
//
// Sum-without-suffix is the case the v0.1 heuristic missed; TablesFor
// fans the lookup across (Gauge, Sum) so the matcher finds rows
// regardless of which side actually stored them. The same fan-out
// applies to `_count` / `_sum` suffixes — the OTel-hostmetrics
// emitter writes these as cumulative Sums under the suffixed name,
// while the OTel-CH histogram exporter writes them as columns on the
// bare-name histogram row. The PromQL lowering produces a UnionAll of
// two per-arm Projects (one per physical layout) because the two
// tables have disjoint value-column shapes (histogram has Count/Sum
// but no Value; sum has Value but no Count) — the existing
// `merge(currentDatabase(), '<regex>')` table-function path can't fan
// disjoint-schema tables, so the per-arm UnionAll lowering shape is
// the only way to address both physical layouts in a single query.
//
// The empty arm contributes zero rows under the MetricName PREWHERE —
// the union is a no-op for any metric whose name is unambiguous after
// the per-arm scan-side filter narrows the candidate rows.
func (m Metrics) TablesFor(metricName string) []string {
	// `_count` and `_sum` are ambiguous between histogram companion
	// (Count/Sum on the histogram row keyed by the bare name) and
	// hostmetrics-style Sum under the suffixed name. Fan across both
	// physical layouts so the matcher finds rows regardless.
	for _, suf := range m.HistogramCompanionSuffixes() {
		if hasSuffix(metricName, suf) {
			// Three physical layouts can carry a `_count`/`_sum`-suffixed
			// name, so fan across all configured-and-distinct candidates:
			//   1. Histogram table, BARE name — the classic-histogram
			//      companion (Count/Sum columns on the `<base>` row).
			//   2. Sum table, SUFFIXED name — OTel-hostmetrics cumulative
			//      sums emitted under the suffixed name.
			//   3. Gauge table, SUFFIXED name — a STANDALONE gauge literally
			//      named `<x>_sum`/`<x>_count` (e.g. yace emits each
			//      CloudWatch statistic as a name suffix: `*_sum`, `*_average`
			//      — all plain gauges). Without this arm such a gauge returns
			//      0 series even though its rows sit in the gauge table.
			// Empty arms are cost-free under the MetricName PREWHERE, so a
			// genuine histogram companion is unaffected by the extra arms.
			return distinctTables(m.HistogramTable, m.SumTable, m.GaugeTable)
		}
	}
	// `_total` and `_bucket` are unambiguous: `_total` is the OTel-CH
	// counter convention (Sum table); `_bucket` is the classic-histogram
	// bucket-companion suffix the bucket-fan-out path rewrites at the
	// lowering layer (which overrides the table to HistogramTable
	// before emit; see isClassicBucketSelector).
	for _, suf := range []string{totalSuffix, bucketSuffix} {
		if hasSuffix(metricName, suf) {
			return []string{m.SumTable}
		}
	}
	// Unsuffixed name: could be Gauge (the v0.1 default) OR Sum (the
	// OTel-hostmetrics / sqlquery shape). Fan the scan across both so
	// the matcher finds rows wherever the upstream emitter dropped them.
	return m.TablesForUnknownName()
}

// TablesForUnknownName returns the candidate tables for a selector whose
// `__name__` cannot be pinned to a literal at plan time — a regex
// matcher (`{__name__=~".*inflight.*"}`), a negated matcher, or no
// `__name__` matcher at all. The suffix heuristics in [Metrics.TablesFor]
// need a concrete name to dispatch on; without one, the only safe
// plain-Sample candidate set is the same (Gauge, Sum) pair the
// unsuffixed arm returns: Gauge first for byte-stable SQL on
// gauge-only deployments, Sum second so metrics the OTel emitters
// store as cumulative sums under bare names (`cerberus_query_inflight`,
// `system_*`, `otelcol_*`) resolve too. Grafana's Metrics Drilldown
// breakdown tab sends `match[]={__name__=~".*<metric>.*"}` for every
// metric, so a gauge-only fallback here silently blanks the breakdown
// surface for every sum-stored metric.
//
// The histogram / exp-histogram tables cannot join this candidate set:
// their row shape (Count / Sum / BucketCounts, no Value column) is
// disjoint from the Sample contract the `merge()` fan-out requires.
// Classic-histogram series under a regex name matcher are surfaced by a
// separate per-arm UnionAll lowering instead
// (lowerRegexHistogramSelector in internal/promql/regex_histogram_lower.go,
// wired from lowerVectorSelector for any selector whose __name__ is not
// pinned to a literal — a regex matcher, a negated matcher, or no
// __name__ matcher at all): it unions this unknown-name candidate set with a
// companion arm per `_count`/`_sum` suffix and a `_bucket` arm, PLUS a
// `_count`/`_sum` companion arm per suffix reading ExpHistogramTable
// (#1549 residue 1) — the same pair, mirroring
// expHistogramSelectorRouting's pinned-path policy in
// internal/promql/lower.go. There is no exp-histogram `_bucket` arm: an
// exp-histogram row has no ExplicitBounds ladder to fan (Scale /
// PositiveBucketCounts / NegativeBucketCounts instead), so
// `<base>_exp_hist_bucket` is not a wire-reachable series either way.
func (m Metrics) TablesForUnknownName() []string {
	if m.SumTable != "" && m.SumTable != m.GaugeTable {
		return []string{m.GaugeTable, m.SumTable}
	}
	return []string{m.GaugeTable}
}

// ConfiguredMetricTables returns the distinct, non-empty plain-Sample and
// classic-histogram table names this schema is configured to read — Gauge,
// Sum, and Histogram, in that order, once duplicates collapse (a schema may
// point GaugeTable and SumTable at the same physical table). It is the same
// "which tables back this metric surface" question metricTables()
// (internal/api/prom/metadata.go) and cerberus's boot-time existence check
// (cmd/cerberus's runRequirementsCheck, #1905) both need answered; both
// consume this method instead of re-deriving the set, so the answer stays
// in exactly one place. ExpHistogramTable is deliberately excluded: the
// UNION-construction callers this serves project the plain-Sample /
// classic-histogram row shapes only, matching TableFor/TablesFor above.
func (m Metrics) ConfiguredMetricTables() []string {
	return distinctTables(m.GaugeTable, m.SumTable, m.HistogramTable)
}

// MetricTableOverrides records, per metrics table, whether the name carried
// by the corresponding Metrics field was SET by configuration — a
// CERBERUS_SCHEMA_METRICS_*_TABLE variable, or the `schema.metrics.*` path
// of a cerberus.yaml, which lowers to the same variable — rather than
// inherited from DefaultOTelMetrics.
//
// The distinction is not cosmetic: it is the difference between an
// ASSERTION and an ASSUMPTION. A name an operator typed is a claim that the
// table exists under exactly that name, so a reachable ClickHouse that does
// not have it is a misconfiguration (a typo, or a table nobody provisioned)
// that never self-heals. A name nobody typed is only cerberus's built-in
// guess at where a stock OTel-CH deployment writes metrics; a deployment
// that ingests logs and traces alone never provisions those tables and is
// not misconfigured for it, so their absence is the ordinary
// not-yet-provisioned schema race. Resolving both to the same string makes
// them indistinguishable at the point of decision, which is why the
// provenance travels with the value.
//
// cmd/cerberus's fatalAbsentMetricTablesErr is the only consumer, and it
// decides on Gauge / Sum / Histogram alone — the three tables the Prom
// metadata surface unions across (see ConfiguredMetricTables): configured
// and absent exits the boot, defaulted and absent boots NOT READY and
// re-probes. ExpHistogram and Summary are recorded for uniformity across
// the five names the resolver manages, so that whichever surface needs them
// next reads the same provenance rather than re-deriving a second, subtly
// different answer; nothing consumes them today.
//
// A whitespace-only configured value counts as UNSET, matching how the
// resolvers treat it: the field keeps its default and the flag stays false.
type MetricTableOverrides struct {
	// Gauge reports whether Metrics.GaugeTable was set by configuration.
	Gauge bool
	// Sum reports whether Metrics.SumTable was set by configuration.
	Sum bool
	// Histogram reports whether Metrics.HistogramTable was set by
	// configuration.
	Histogram bool
	// ExpHistogram reports whether Metrics.ExpHistogramTable was set by
	// configuration.
	ExpHistogram bool
	// Summary reports whether Metrics.SummaryTable was set by
	// configuration.
	Summary bool
}

// The Prom-convention metric-name suffixes this package routes on.
// `_count` / `_sum` live in [Metrics.histogramCompanions] instead,
// because each of those pairs a suffix with the histogram-row column
// that serves it; `_total` and `_bucket` carry no column pairing.
const (
	totalSuffix  = "_total"
	bucketSuffix = "_bucket"
)

// ExemplarSource is one candidate physical source for an exemplar lookup:
// the table whose `Exemplars` Nested column may carry the rows, paired
// with the MetricName those rows are keyed by IN THAT table.
//
// MetricName is not always the name the caller queried. The OTel-CH
// histogram exporter writes a classic histogram as ONE row under the bare
// `<base>` name and serves the Prom-convention `<base>_bucket` /
// `<base>_count` / `<base>_sum` companion series off that row's columns —
// so on a histogram-shaped table the row key is the bare name while on a
// Sum or Gauge table it is the suffixed name the caller typed. A caller
// that filters on `MetricName` must use the per-source value or the arm
// matches nothing.
//
// MetricName is empty when the selector pinned no `__name__` equality
// matcher; there is no name to rewrite to, and the caller's matchers pass
// through to every source unchanged.
type ExemplarSource struct {
	Table      string
	MetricName string
}

// ExemplarSources returns every (table, row key) pair an exemplar lookup
// for metricName must read. Pass "" for a selector that pins no
// `__name__` equality matcher — a regex name matcher, a negated one, or
// no name matcher at all.
//
// This is the exemplar counterpart of [Metrics.TablesFor] /
// [Metrics.TablesForUnknownName], and it delegates to them for the shared
// suffix heuristics so the two read paths cannot drift. It differs in two
// ways, both forced by what an exemplar row is:
//
//   - The histogram and exp-histogram tables join every non-bucket
//     candidate set. They are absent from the plain-Sample candidate set
//     because their row shape has no Value column, which the `merge()`
//     fan-out requires — a constraint the exemplar projection does not
//     share, since it reads the `Exemplars` Nested column that every
//     non-summary table carries identically. Exemplars are predominantly
//     a histogram concept and the histogram row is keyed by the BARE
//     metric name, so leaving those tables out is what makes an
//     unsuffixed histogram name (`http_server_duration`) answer empty.
//   - The summary table is dropped: the upstream OTel-CH summary table
//     has no `Exemplars` column at all, so a summary-only deployment
//     yields an empty candidate set and the caller answers `data:[]`
//     without a ClickHouse round trip.
//
// Two name shapes resolve to exactly one physical layout and skip the
// fan-out: an exp-histogram name (only the exp-histogram table stores a
// row under it) and a `<base>_bucket` name (buckets exist only on the
// classic-histogram row, which carries the exemplars for every bucket of
// the series).
func (m Metrics) ExemplarSources(metricName string) []ExemplarSource {
	tables := m.exemplarTablesFor(metricName)
	out := make([]ExemplarSource, 0, len(tables))
	for _, t := range tables {
		out = append(out, ExemplarSource{Table: t, MetricName: m.exemplarRowName(metricName, t)})
	}
	return out
}

// exemplarTablesFor resolves the candidate table set behind
// [Metrics.ExemplarSources]; see that method's doc for the rationale of
// each branch.
func (m Metrics) exemplarTablesFor(metricName string) []string {
	if metricName != "" && m.IsExpHistogramMetric(metricName) {
		return m.exemplarCarrying(m.ExpHistogramTable)
	}
	if metricName != "" && hasSuffix(metricName, bucketSuffix) {
		return m.exemplarCarrying(m.HistogramTable)
	}
	sampleTables := m.TablesForUnknownName()
	if metricName != "" {
		sampleTables = m.TablesFor(metricName)
	}
	candidates := make([]string, 0, len(sampleTables)+2)
	candidates = append(candidates, sampleTables...)
	candidates = append(candidates, m.HistogramTable, m.ExpHistogramTable)
	return m.exemplarCarrying(candidates...)
}

// exemplarCarrying keeps the configured, distinct, exemplar-carrying
// tables among its arguments, in argument order. The summary table is
// filtered out wherever it appears: the upstream OTel-CH summary DDL has
// no `Exemplars` column, so reading exemplars from it is not a thin
// result but a SQL error against a missing column.
func (m Metrics) exemplarCarrying(tables ...string) []string {
	candidates := distinctTables(tables...)
	if m.SummaryTable == "" {
		return candidates
	}
	out := make([]string, 0, len(candidates))
	for _, t := range candidates {
		if t == m.SummaryTable {
			continue
		}
		out = append(out, t)
	}
	return out
}

// exemplarRowName returns the MetricName that rows for the queried
// metricName are keyed by in table — the bare base name on the
// histogram-shaped tables for a Prom-convention companion name, the
// queried name everywhere else. Returns "" for an unpinned name, which
// leaves the caller's matchers untouched.
func (m Metrics) exemplarRowName(metricName, table string) string {
	if metricName == "" {
		return ""
	}
	if table != m.HistogramTable && table != m.ExpHistogramTable {
		return metricName
	}
	if bare, _, ok := m.HistogramCompanionColumn(metricName); ok {
		return bare
	}
	if table == m.HistogramTable && hasSuffix(metricName, bucketSuffix) {
		return metricName[:len(metricName)-len(bucketSuffix)]
	}
	return metricName
}

// distinctTables returns the non-empty table names in argument order with
// duplicates removed — the candidate set for a selector whose name may live
// in more than one physical layout. First occurrence wins, so callers pass
// the byte-stable-preferred table first.
func distinctTables(tables ...string) []string {
	out := make([]string, 0, len(tables))
	seen := make(map[string]struct{}, len(tables))
	for _, t := range tables {
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
