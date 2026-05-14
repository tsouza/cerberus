package schema

import (
	"testing"
	"time"
)

// TestDefaultOTelMetricsPinsUpstreamColumns pins every column name
// DefaultOTelMetrics() returns against the verbatim string the upstream
// `clickhouseexporter` writes in its `metrics_*_table.sql` templates.
// If upstream renames a column we want this test to break loudly — the
// PR B → PR C → PR D chain depends on the column names matching.
func TestDefaultOTelMetricsPinsUpstreamColumns(t *testing.T) {
	t.Parallel()

	m := DefaultOTelMetrics()

	// Table names (a separate axis from column names — they don't
	// appear inside the CREATE TABLE column list, so they aren't part
	// of the pinning map below).
	tables := map[string]string{
		"GaugeTable":        m.GaugeTable,
		"SumTable":          m.SumTable,
		"HistogramTable":    m.HistogramTable,
		"ExpHistogramTable": m.ExpHistogramTable,
		"SummaryTable":      m.SummaryTable,
	}
	wantTables := map[string]string{
		"GaugeTable":        "otel_metrics_gauge",
		"SumTable":          "otel_metrics_sum",
		"HistogramTable":    "otel_metrics_histogram",
		"ExpHistogramTable": "otel_metrics_exp_histogram",
		"SummaryTable":      "otel_metrics_summary",
	}
	for name, want := range wantTables {
		if got := tables[name]; got != want {
			t.Errorf("table %s: got %q, want %q", name, got, want)
		}
	}

	cases := map[string]string{
		// Identity / resource columns (present on every table).
		"MetricName":         m.MetricNameColumn,
		"Attributes":         m.AttributesColumn,
		"ResourceAttributes": m.ResourceAttributesColumn,
		"TimeUnix":           m.TimestampColumn,
		"StartTimeUnix":      m.StartTimeColumn,
		"MetricDescription":  m.MetricDescriptionColumn,
		"MetricUnit":         m.MetricUnitColumn,
		"ServiceName":        m.ServiceNameColumn,
		"ScopeName":          m.ScopeNameColumn,
		"ScopeVersion":       m.ScopeVersionColumn,
		"ScopeAttributes":    m.ScopeAttributesColumn,
		"Flags":              m.FlagsColumn,

		// Gauge / sum specifically — Value column.
		"Value": m.ValueColumn,

		// Classic histogram + summary shared columns.
		"Count":                  m.CountColumn,
		"Sum":                    m.SumColumn,
		"Min":                    m.MinColumn,
		"Max":                    m.MaxColumn,
		"BucketCounts":           m.BucketCountsColumn,
		"ExplicitBounds":         m.ExplicitBoundsColumn,
		"AggregationTemporality": m.AggregationTemporalityColumn,
		"IsMonotonic":            m.IsMonotonicColumn,

		// Exponential-histogram-specific columns.
		"Scale":                m.ScaleColumn,
		"ZeroCount":            m.ZeroCountColumn,
		"PositiveOffset":       m.PositiveOffsetColumn,
		"PositiveBucketCounts": m.PositiveBucketCountsColumn,
		"NegativeOffset":       m.NegativeOffsetColumn,
		"NegativeBucketCounts": m.NegativeBucketCountsColumn,

		// Summary-specific.
		"ValueAtQuantiles": m.ValueAtQuantilesColumn,

		// Exemplars (Nested, present on gauge/sum/histogram/exp_histogram).
		"Exemplars": m.ExemplarsColumn,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("metrics column %q: got %q, want %q (mismatch against upstream OTel CH Exporter template)", want, got, want)
		}
	}

	// Sanity: every advertised column must be non-empty. A blank
	// default means a future PR C / PR D will emit unquoted gaps in
	// the DDL / SELECT list.
	if m.GaugeTable == "" || m.SumTable == "" || m.HistogramTable == "" ||
		m.ExpHistogramTable == "" || m.SummaryTable == "" {
		t.Fatalf("DefaultOTelMetrics(): empty table name in %+v", m)
	}
}

// TestDefaultOTelMetricsTableFor pins the existing PromQL routing
// heuristic so PR C / PR D / PR G refactors can't silently regress it.
func TestDefaultOTelMetricsTableFor(t *testing.T) {
	t.Parallel()
	m := DefaultOTelMetrics()
	cases := map[string]string{
		"up":                                  m.GaugeTable,
		"http_server_request_duration_count":  m.SumTable,
		"http_server_request_duration_sum":    m.SumTable,
		"http_server_request_duration_bucket": m.SumTable,
		"requests_total":                      m.SumTable,
		"latency":                             m.GaugeTable,
	}
	for name, want := range cases {
		if got := m.TableFor(name); got != want {
			t.Errorf("TableFor(%q): got %q, want %q", name, got, want)
		}
	}
}

// TestDefaultOTelMetricsRollups pins the canonical OTel sum rollups
// the upstream exporter writes when rollup tables are enabled. The
// MV-substitution optimizer rule reads this list to find candidates; if the upstream exporter ships a new rollup window we
// add it here so the rule picks it up.
func TestDefaultOTelMetricsRollups(t *testing.T) {
	t.Parallel()
	m := DefaultOTelMetrics()

	rollups := m.Rollups()
	if len(rollups) == 0 {
		t.Fatalf("DefaultOTelMetrics: expected at least the two canonical sum rollups; got none")
	}

	// The 1h rollup must come before 5m so firstApplicable cost-model
	// prefers the coarsest applicable window.
	got1h := rollups[0]
	if got1h.RollupTable != "otel_metrics_sum_1h" {
		t.Errorf("first rollup: got RollupTable=%q, want otel_metrics_sum_1h (coarsest-first ordering broken)", got1h.RollupTable)
	}
	if got1h.Window != time.Hour {
		t.Errorf("1h rollup Window: got %s, want 1h", got1h.Window)
	}
	if got1h.AggOp != RollupAggSum {
		t.Errorf("1h rollup AggOp: got %q, want sum", got1h.AggOp)
	}
	if got1h.ValueColumn != "Sum" {
		t.Errorf("1h rollup ValueColumn: got %q, want Sum", got1h.ValueColumn)
	}
	if got1h.BaseTable != m.SumTable {
		t.Errorf("1h rollup BaseTable: got %q, want %q", got1h.BaseTable, m.SumTable)
	}

	// RollupsFor must filter by base table.
	sumOnly := m.RollupsFor(m.SumTable)
	if len(sumOnly) != 2 {
		t.Errorf("RollupsFor(SumTable): expected 2 rollups, got %d", len(sumOnly))
	}
	gaugeOnly := m.RollupsFor(m.GaugeTable)
	if len(gaugeOnly) != 0 {
		t.Errorf("RollupsFor(GaugeTable): expected 0 rollups (no gauge rollups in default schema), got %d", len(gaugeOnly))
	}
}
