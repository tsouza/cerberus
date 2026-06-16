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

// TestDefaultOTelMetricsTablesFor pins the candidate-set routing
// schema.Metrics.TablesFor uses for the matcher-resolve path. Suffixed
// names route to a single table (preserves byte-stable SQL on the
// fixtures that exercise the existing TableFor heuristic); unsuffixed
// names fan out across (Gauge, Sum) so the PromQL matcher resolves the
// OTel-hostmetrics / sqlquery / prometheus-self case where cumulative
// sums ship under bare names that the Prom convention reserves for
// gauges. See the regression fixture at
// test/spec/promql/scan_unions_gauge_sum_for_unsuffixed_metric.txtar
// for the end-to-end pin against the compose-smoke / dashboard surface.
func TestDefaultOTelMetricsTablesFor(t *testing.T) {
	t.Parallel()
	m := DefaultOTelMetrics()
	cases := map[string][]string{
		// Unsuffixed: matcher could resolve in either Gauge or Sum
		// depending on which OTel-emitter populated the row, so the
		// scan unions both tables. Stable order: Gauge first (the v0.1
		// default), Sum second (the OTel-emitter fallback).
		"up":                     {m.GaugeTable, m.SumTable},
		"latency":                {m.GaugeTable, m.SumTable},
		"system_cpu_time":        {m.GaugeTable, m.SumTable},
		"clickhouse_event":       {m.GaugeTable, m.SumTable},
		"otelcol_process_uptime": {m.GaugeTable, m.SumTable},
		// Suffix-routed: TableFor returns a single table, so does
		// TablesFor — keeps the matcher-resolve path single-table for
		// the fixtures that exercise `_total` / `_count` / `_sum` /
		// `_bucket` shapes.
		// `_count` and `_sum` fan to (Histogram, Sum, Gauge) — three
		// physical layouts can carry the suffixed name: the OTel-CH
		// histogram exporter writes Count/Sum columns on the bare-name
		// histogram row; the OTel-hostmetrics emitter writes cumulative
		// Sums under the suffixed name (`system_cpu_logical_count`,
		// `system_processes_count`, …); AND a STANDALONE gauge can be
		// literally named `<x>_sum`/`<x>_count` (e.g. yace emits each
		// CloudWatch statistic as a name suffix — `*_sum`, `*_average` —
		// all plain gauges in otel_metrics_gauge). The PromQL lowering
		// builds a UnionAll across all three so the matcher finds rows
		// wherever the upstream emitter dropped them.
		"http_server_request_duration_count": {m.HistogramTable, m.SumTable, m.GaugeTable},
		"http_server_request_duration_sum":   {m.HistogramTable, m.SumTable, m.GaugeTable},
		"system_cpu_logical_count":           {m.HistogramTable, m.SumTable, m.GaugeTable},
		"system_processes_count":             {m.HistogramTable, m.SumTable, m.GaugeTable},
		"system_filesystem_inodes_count":     {m.HistogramTable, m.SumTable, m.GaugeTable},
		"system_processes_created_count":     {m.HistogramTable, m.SumTable, m.GaugeTable},
		// A standalone gauge literally named `<x>_sum` (the yace
		// CloudWatch-statistic case) — the gauge arm is what makes it
		// resolve instead of returning 0 series.
		"aws_applicationelb_request_count_sum": {m.HistogramTable, m.SumTable, m.GaugeTable},
		// `_total` and `_bucket` stay single-table — counter naming and
		// classic-histogram bucket-companion fan-out aren't ambiguous
		// between physical layouts.
		"http_server_request_duration_bucket": {m.SumTable},
		"requests_total":                      {m.SumTable},
	}
	for name, want := range cases {
		got := m.TablesFor(name)
		if len(got) != len(want) {
			t.Errorf("TablesFor(%q): len got %d (%v), want %d (%v)", name, len(got), got, len(want), want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("TablesFor(%q)[%d]: got %q, want %q", name, i, got[i], want[i])
			}
		}
	}
}

// TestHistogramCompanionColumn pins the routing of classic-histogram
// `_count` / `_sum` companion suffixes to the OTel-CH histogram-row
// Count / Sum columns under the BARE metric name. The Prometheus
// convention exposes a classic histogram as three companion series
// (`<X>_bucket`, `<X>_count`, `<X>_sum`); OTel-CH stores all three on
// a single row keyed by `<X>` (no suffix), so PromQL queries against
// the suffixed names must reroute to the histogram table + project
// the matching column as `Value`. Without this routing every
// `rate(<X>_count[5m])` / `rate(<X>_sum[5m]) / rate(<X>_count[5m])`
// Grafana panel silently returned "No data" (cerberus task #193).
//
// `_total` (the OTel-CH counter convention) is explicitly NOT routed
// here — counter routing stays on the existing TableFor heuristic.
//
// Mirrors the precedent in
// internal/api/prom/exemplars.go::exemplarsTableFor, which already
// adopts the same `_count`/`_sum` → histogram-table routing for the
// exemplars endpoint.
func TestHistogramCompanionColumn(t *testing.T) {
	t.Parallel()
	m := DefaultOTelMetrics()

	cases := []struct {
		name              string
		input             string
		wantBare          string
		wantValueColumn   string
		wantIsHistCompCol bool
	}{
		{
			name:              "count_suffix_strips_and_routes_to_Count",
			input:             "http_server_request_duration_count",
			wantBare:          "http_server_request_duration",
			wantValueColumn:   m.CountColumn,
			wantIsHistCompCol: true,
		},
		{
			name:              "sum_suffix_strips_and_routes_to_Sum",
			input:             "http_server_request_duration_sum",
			wantBare:          "http_server_request_duration",
			wantValueColumn:   m.SumColumn,
			wantIsHistCompCol: true,
		},
		{
			name:              "total_suffix_is_NOT_a_companion",
			input:             "http_server_requests_total",
			wantBare:          "http_server_requests_total",
			wantValueColumn:   "",
			wantIsHistCompCol: false,
		},
		{
			name:              "bucket_suffix_is_NOT_a_companion_handled_by_stripBucketSuffix",
			input:             "http_server_request_duration_bucket",
			wantBare:          "http_server_request_duration_bucket",
			wantValueColumn:   "",
			wantIsHistCompCol: false,
		},
		{
			name:              "bare_metric_name_unchanged",
			input:             "up",
			wantBare:          "up",
			wantValueColumn:   "",
			wantIsHistCompCol: false,
		},
		{
			name:              "empty_input_unchanged",
			input:             "",
			wantBare:          "",
			wantValueColumn:   "",
			wantIsHistCompCol: false,
		},
		{
			name:              "boundary_just_count_strips_to_empty_bare",
			input:             "_count",
			wantBare:          "",
			wantValueColumn:   m.CountColumn,
			wantIsHistCompCol: true,
		},
		{
			name:              "boundary_just_sum_strips_to_empty_bare",
			input:             "_sum",
			wantBare:          "",
			wantValueColumn:   m.SumColumn,
			wantIsHistCompCol: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bare, col, ok := m.HistogramCompanionColumn(tc.input)
			if ok != tc.wantIsHistCompCol {
				t.Fatalf("HistogramCompanionColumn(%q): ok = %v, want %v",
					tc.input, ok, tc.wantIsHistCompCol)
			}
			if bare != tc.wantBare {
				t.Errorf("HistogramCompanionColumn(%q): bare = %q, want %q",
					tc.input, bare, tc.wantBare)
			}
			if col != tc.wantValueColumn {
				t.Errorf("HistogramCompanionColumn(%q): valueColumn = %q, want %q",
					tc.input, col, tc.wantValueColumn)
			}
		})
	}
}

// TestDefaultOTelMetricsRollups pins the canonical OTel sum rollups
// the upstream exporter writes when rollup tables are enabled. No
// optimizer rule consumes this registry today (the MV-substitution rule
// that did was retired in 2026-06); the pin keeps the schema-side
// contract stable for a future rollup-substitution rule.
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
