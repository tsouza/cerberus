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

// TestFatalAbsentMetricTablesErr pins #1905's core boundary in isolation
// from any ClickHouse connection: given the reachable-and-absent table
// names preflight.Run has already computed, exactly the configured
// Gauge/Sum/Histogram tables that appear there turn into a fatal error
// naming both the table and the schema.metrics.* config key that set it;
// everything else (a present table, or an unconfigured/empty-string one)
// leaves boot alone.
func TestFatalAbsentMetricTablesErr(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()

	t.Run("table present: no fatal error", func(t *testing.T) {
		t.Parallel()
		// Empty absentTables is exactly what preflight reports once every
		// configured table round-trips a non-zero system.columns row count.
		if err := fatalAbsentMetricTablesErr(m, nil); err != nil {
			t.Fatalf("fatalAbsentMetricTablesErr(present) = %v, want nil", err)
		}
	})

	t.Run("table absent: fatal error names table and config key", func(t *testing.T) {
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

	t.Run("empty configured name: absence of an unrelated table is not fatal", func(t *testing.T) {
		t.Parallel()
		empty := m
		empty.HistogramTable = ""
		// A table name preflight would never even probe (checkSchema skips
		// empty names) cannot appear in absentTables for the empty field, so
		// this reports some OTHER absent table to prove the empty binding
		// specifically never matches.
		err := fatalAbsentMetricTablesErr(empty, []string{"some_unrelated_table"})
		if err != nil {
			t.Fatalf("fatalAbsentMetricTablesErr(empty HistogramTable) = %v, want nil", err)
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

// TestDecideRequirementsOutcome exercises the four #1905 boot cells against
// decideRequirementsOutcome directly, with no ClickHouse connection: it
// constructs the preflight.Result values a real probe would have produced
// in each scenario and asserts the resulting boot decision. The
// unreachable cell is the one most likely to regress silently — a naive
// implementation that fails fast on "any absent table" without checking
// res.Unreachable first would turn a ClickHouse that simply has not
// started yet into a boot crash, exactly the crash-loop #1905's fix must
// not reintroduce.
func TestDecideRequirementsOutcome(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	const database = "otel"

	t.Run("table present: proceeds ready", func(t *testing.T) {
		t.Parallel()
		res := preflight.Result{}
		got := decideRequirementsOutcome(res, m, database)
		if got.fatalErr != nil || got.notReadyReason != "" {
			t.Fatalf("decideRequirementsOutcome(present) = %+v, want a clean pass", got)
		}
	})

	t.Run("table absent, server reachable: fails fast naming table and config key", func(t *testing.T) {
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

	t.Run("empty configured table name: proceeds even though other tables are absent", func(t *testing.T) {
		t.Parallel()
		empty := m
		empty.HistogramTable = ""
		// HistogramTable is unconfigured; ExpHistogramTable being absent must
		// not fail boot through the metrics gate at all — ConfiguredMetricTables
		// (and therefore fatalAbsentMetricTablesErr) never look at
		// ExpHistogramTable, and the empty HistogramTable can never match.
		res := preflight.Result{AbsentTables: []string{m.ExpHistogramTable}}
		got := decideRequirementsOutcome(res, empty, database)
		if got.fatalErr != nil {
			t.Fatalf("decideRequirementsOutcome(empty configured name) fatalErr = %v, want nil", got.fatalErr)
		}
		// The absent ExpHistogramTable still falls through to the general
		// transient AbsentTables tolerance (SchemaProvisioned() is false),
		// which is unchanged, pre-existing preflight behaviour, not a #1905
		// fatal.
		if got.notReadyReason == "" {
			t.Error("decideRequirementsOutcome(empty configured name) notReadyReason = \"\", want the pre-existing transient AbsentTables reason")
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
