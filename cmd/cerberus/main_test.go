package main

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestIsVersionFlag pins the argv shapes recognized by the
// `--version` pre-flight. The cerberus container in
// compatibility/prometheus/docker-compose.yml uses this exact path as
// its docker healthcheck because the distroless runtime image has no
// shell / wget / curl; a regression here would silently re-break the
// compatibility lane's "container ... is unhealthy" failure mode
// (see PR #297 + follow-up).
func TestIsVersionFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty argv (nil)", nil, false},
		{"empty argv (slice)", []string{}, false},
		{"binary only — server mode", []string{"cerberus"}, false},
		{"long form --version", []string{"cerberus", "--version"}, true},
		{"short form -v", []string{"cerberus", "-v"}, true},
		{"subcommand-style version", []string{"cerberus", "version"}, true},
		{"unrelated flag falls through", []string{"cerberus", "--help"}, false},
		{"unrelated subcommand falls through", []string{"cerberus", "serve"}, false},
		{"version with trailing junk still recognized", []string{"cerberus", "--version", "extra"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isVersionFlag(tc.args); got != tc.want {
				t.Fatalf("isVersionFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// metricsFrom resolves a schema.Metrics through the real configuration
// resolver over the given settings, so a test exercises the same
// default-vs-configured provenance a deployment gets from its environment or
// its cerberus.yaml (both lower to these CERBERUS_SCHEMA_METRICS_*_TABLE
// keys — see internal/config/nested.go). Passing no settings yields the
// stock schema a deployment that configured no metric table gets.
func metricsFrom(settings map[string]string) schema.Metrics {
	return schema.DefaultOTelMetricsFrom(func(key string) string { return settings[key] })
}

// configuredMetrics is the schema of a deployment that spelled out all three
// metric tables. The names are the stock ones deliberately: an operator who
// types the default spelling has still ASSERTED the table exists, so the
// value alone can never stand in for the provenance.
func configuredMetrics() schema.Metrics {
	def := schema.DefaultOTelMetrics()
	return metricsFrom(map[string]string{
		schema.EnvMetricsGaugeTable:     def.GaugeTable,
		schema.EnvMetricsSumTable:       def.SumTable,
		schema.EnvMetricsHistogramTable: def.HistogramTable,
	})
}

// TestFatalAbsentMetricTablesErr pins #1905's core boundary in isolation
// from any ClickHouse connection: given the reachable-and-absent table
// names preflight.Run has already computed, exactly the EXPLICITLY
// CONFIGURED Gauge/Sum/Histogram tables that appear there turn into a fatal
// error naming both the table and the schema.metrics.* config key that set
// it. Everything else leaves boot alone — a present table, an
// empty-string one, and above all a table whose name cerberus DEFAULTED,
// which is an assumption about where a stock OTel-CH deployment writes
// metrics rather than an operator assertion that it exists.
func TestFatalAbsentMetricTablesErr(t *testing.T) {
	t.Parallel()
	m := configuredMetrics()

	t.Run("table present: no fatal error", func(t *testing.T) {
		t.Parallel()
		// Empty absentTables is exactly what preflight reports once every
		// configured table round-trips a non-zero system.columns row count.
		if err := fatalAbsentMetricTablesErr(m, nil); err != nil {
			t.Fatalf("fatalAbsentMetricTablesErr(present) = %v, want nil", err)
		}
	})

	t.Run("configured table absent: fatal error names table and config key", func(t *testing.T) {
		t.Parallel()
		err := fatalAbsentMetricTablesErr(m, []string{m.HistogramTable})
		if err == nil {
			t.Fatal("fatalAbsentMetricTablesErr(absent) = nil, want an error")
		}
		if !strings.Contains(err.Error(), m.HistogramTable) {
			t.Errorf("error %q does not name the missing table %q", err.Error(), m.HistogramTable)
		}
		if !strings.Contains(err.Error(), "schema.metrics.histogramTable") {
			t.Errorf("error %q does not name the config key schema.metrics.histogramTable", err.Error())
		}
	})

	t.Run("defaulted tables absent: not fatal", func(t *testing.T) {
		t.Parallel()
		// A deployment that never configured a metric table: the names are
		// cerberus's built-in defaults, and a ClickHouse carrying only logs
		// and traces (or one whose collector has not written its first
		// metric yet) simply does not have them. That is the ordinary
		// not-yet-provisioned schema race, so boot must survive it and let
		// the transient path re-probe.
		def := metricsFrom(nil)
		err := fatalAbsentMetricTablesErr(def, []string{def.GaugeTable, def.SumTable, def.HistogramTable})
		if err != nil {
			t.Fatalf("fatalAbsentMetricTablesErr(all three defaulted and absent) = %v, want nil", err)
		}
	})

	t.Run("configured and defaulted mixed: only the configured one is fatal", func(t *testing.T) {
		t.Parallel()
		def := schema.DefaultOTelMetrics()
		mixed := metricsFrom(map[string]string{schema.EnvMetricsGaugeTable: "custom_gauge"})
		err := fatalAbsentMetricTablesErr(mixed, []string{mixed.GaugeTable, mixed.SumTable})
		if err == nil {
			t.Fatal("fatalAbsentMetricTablesErr(configured gauge absent) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "custom_gauge") {
			t.Errorf("error %q does not name the configured absent table", err.Error())
		}
		if strings.Contains(err.Error(), def.SumTable) {
			t.Errorf("error %q names the DEFAULTED absent table %q; only a configured name is an assertion", err.Error(), def.SumTable)
		}
	})

	t.Run("unrelated absent table is not fatal", func(t *testing.T) {
		t.Parallel()
		// Only the three tables the metrics metadata surface unions across
		// are decided here; an absent table outside that set is preflight's
		// ordinary transient finding.
		err := fatalAbsentMetricTablesErr(m, []string{m.ExpHistogramTable, "some_unrelated_table"})
		if err != nil {
			t.Fatalf("fatalAbsentMetricTablesErr(unrelated absent) = %v, want nil", err)
		}
	})

	t.Run("multiple absent tables all named", func(t *testing.T) {
		t.Parallel()
		err := fatalAbsentMetricTablesErr(m, []string{m.GaugeTable, m.SumTable})
		if err == nil {
			t.Fatal("fatalAbsentMetricTablesErr(multiple absent) = nil, want an error")
		}
		for _, want := range []string{m.GaugeTable, m.SumTable, "schema.metrics.gaugeTable", "schema.metrics.sumTable"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err.Error(), want)
			}
		}
	})
}

// TestDecideRequirementsOutcome exercises each #1905 boot cell against
// decideRequirementsOutcome directly, with no ClickHouse connection: it
// constructs the preflight.Result values a real probe would have produced
// in each scenario and asserts the resulting boot decision. Two cells carry
// the crash-loop history. The unreachable one is the most likely to regress
// silently — a naive implementation that fails fast on "any absent table"
// without checking res.Unreachable first would turn a ClickHouse that
// simply has not started yet into a boot crash. The defaulted-tables one is
// the same failure reached from the other side: a gate that reads the
// resolved table NAME instead of its provenance cannot tell a metrics-less
// deployment apart from a typo, and crash-loops every deployment that never
// asked for metrics.
func TestDecideRequirementsOutcome(t *testing.T) {
	t.Parallel()
	// Every cell below that must NOT be fatal is exercised against an
	// explicitly configured schema, so it is the branch under test that
	// keeps boot alive rather than the defaulted-name tolerance.
	m := configuredMetrics()
	const database = "otel"

	t.Run("table present: proceeds ready", func(t *testing.T) {
		t.Parallel()
		res := preflight.Result{}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr != nil || got.notReadyReason != "" {
			t.Fatalf("decideRequirementsOutcome(present) = %+v, want a clean pass", got)
		}
	})

	t.Run("defaulted metric tables absent, server reachable: boots NOT READY", func(t *testing.T) {
		t.Parallel()
		// The metrics-less deployment: a reachable ClickHouse that carries
		// no metric tables, and a cerberus that was never told to expect
		// any. Boot must survive on the transient re-probe path — /healthz
		// answers 200 and only /readyz gates — exactly as it does for an
		// absent logs or traces table.
		def := metricsFrom(nil)
		res := preflight.Result{AbsentTables: []string{def.GaugeTable, def.SumTable, def.HistogramTable}}
		got := decideRequirementsOutcome(res, def, database)
		if got.fatalErr != nil {
			t.Fatalf("decideRequirementsOutcome(defaulted absent) fatalErr = %v, want nil (must not block boot)", got.fatalErr)
		}
		if got.notReadyReason == "" {
			t.Error("decideRequirementsOutcome(defaulted absent) notReadyReason = \"\", want the transient AbsentTables reason")
		}
	})

	t.Run("configured table absent, server reachable: fails fast naming table and config key", func(t *testing.T) {
		t.Parallel()
		res := preflight.Result{AbsentTables: []string{m.HistogramTable}}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr == nil {
			t.Fatal("decideRequirementsOutcome(absent, reachable) fatalErr = nil, want an error")
		}
		if got.notReadyReason != "" {
			t.Errorf("decideRequirementsOutcome(absent, reachable) notReadyReason = %q, want empty (must not ALSO report transient)", got.notReadyReason)
		}
		if !strings.Contains(got.fatalErr.Error(), m.HistogramTable) {
			t.Errorf("fatalErr %q does not name the missing table", got.fatalErr.Error())
		}
	})

	t.Run("clickhouse unreachable: proceeds, does not fail boot even with absent tables", func(t *testing.T) {
		t.Parallel()
		// A real preflight.Run never populates AbsentTables alongside
		// Unreachable (checkSchema abandons introspection on the first
		// transport error), but this test sets both anyway: the ordering
		// inside decideRequirementsOutcome — Unreachable checked BEFORE
		// fatalAbsentMetricTablesErr — must be the thing that keeps boot
		// alive, not an accident of what preflight happens to populate.
		res := preflight.Result{
			Unreachable:    true,
			UnreachableErr: errUnreachableProbe,
			AbsentTables:   []string{m.GaugeTable, m.SumTable, m.HistogramTable},
		}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr != nil {
			t.Fatalf("decideRequirementsOutcome(unreachable) fatalErr = %v, want nil (must not block boot)", got.fatalErr)
		}
		if got.notReadyReason == "" {
			t.Error("decideRequirementsOutcome(unreachable) notReadyReason = \"\", want a not-ready reason")
		}
	})

	t.Run("table outside the metrics gate absent: proceeds NOT READY", func(t *testing.T) {
		t.Parallel()
		// ExpHistogramTable is probed by preflight but is not part of the
		// set the metrics metadata surface unions across, so its absence
		// must not fail boot through the metrics gate at all — it falls
		// through to the general transient AbsentTables tolerance
		// (SchemaProvisioned() is false), which is unchanged, pre-existing
		// preflight behaviour, not a #1905 fatal.
		res := preflight.Result{AbsentTables: []string{m.ExpHistogramTable}}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr != nil {
			t.Fatalf("decideRequirementsOutcome(non-gated table absent) fatalErr = %v, want nil", got.fatalErr)
		}
		if got.notReadyReason == "" {
			t.Error("decideRequirementsOutcome(non-gated table absent) notReadyReason = \"\", want the transient AbsentTables reason")
		}
	})

	t.Run("database absent: proceeds, does not fail boot even with absent tables", func(t *testing.T) {
		t.Parallel()
		res := preflight.Result{
			DatabaseAbsent:    true,
			DatabaseAbsentErr: errUnreachableProbe,
			AbsentTables:      []string{m.GaugeTable},
		}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr != nil {
			t.Fatalf("decideRequirementsOutcome(database absent) fatalErr = %v, want nil (must not block boot)", got.fatalErr)
		}
		if got.notReadyReason == "" {
			t.Error("decideRequirementsOutcome(database absent) notReadyReason = \"\", want a not-ready reason")
		}
	})

	t.Run("fatal shape problem still short-circuits everything else", func(t *testing.T) {
		t.Parallel()
		res := preflight.Result{
			Fatal:        errUnreachableProbe,
			AbsentTables: []string{m.GaugeTable},
		}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr == nil {
			t.Fatal("decideRequirementsOutcome(fatal shape) fatalErr = nil, want the wrapped Fatal error")
		}
	})
}

// errUnreachableProbe is a stand-in probe error for tests that only need a
// non-nil error value, never its text.
var errUnreachableProbe = errTestPlaceholder("simulated probe failure")

type errTestPlaceholder string

func (e errTestPlaceholder) Error() string { return string(e) }
