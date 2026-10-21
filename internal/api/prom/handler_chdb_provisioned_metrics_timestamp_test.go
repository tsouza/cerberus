//go:build chdb

// chDB-backed end-to-end coverage for the metric tables cerberus provisions
// itself: the tables are created from internal/schema/ddl's RenderAll output
// at each schema.MetricsTimestampPrecision, so the physical shape the
// emitted SQL meets — the fork's templates, the MetricName-first sorting
// key, the curated projections and indexes — is the one
// CERBERUS_AUTO_CREATE_SCHEMA creates. Each table set is served at the
// precision this file states for its timestamp type — independently of the
// boot requirements check's classifier, which it asserts agrees — and
// answers the same PromQL as the upstream DateTime schema
// (dtRunPromQLCases).

package prom_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// provisionedMetricsDDL returns a renderDDL for newChDBMetricsTimestampServer
// that renders the metrics signal's DDL at precision into the database.
func provisionedMetricsDDL(precision schema.MetricsTimestampPrecision) func(database string) ([]string, error) {
	return func(database string) ([]string, error) {
		return ddl.RenderAll(ddl.Config{
			Database:                  database,
			MetricsTimestampPrecision: precision,
		}, []ddl.Signal{ddl.Metrics})
	}
}

// retypedMetricsDDL returns a renderDDL that renders the metrics DDL
// provisioned at precision, with every timestamp column declared typ
// instead — TimeUnix, StartTimeUnix and the Nested Exemplars.TimeUnix
// alike — failing when the rendered DDL declares no timestamp column to
// retype.
func retypedMetricsDDL(typ string, precision schema.MetricsTimestampPrecision) func(database string) ([]string, error) {
	return func(database string) ([]string, error) {
		stmts, err := provisionedMetricsDDL(precision)(database)
		if err != nil {
			return nil, err
		}
		m := schema.DefaultOTelMetrics()
		declared := m.TimestampColumn + " " + precision.ColumnType()
		retyped := 0
		for i, stmt := range stmts {
			retyped += strings.Count(stmt, declared)
			stmts[i] = strings.ReplaceAll(stmt, declared, m.TimestampColumn+" "+typ)
		}
		if retyped == 0 {
			return nil, fmt.Errorf("no %q declaration in the provisioned metrics DDL to retype", declared)
		}
		return stmts, nil
	}
}

// TestProvisionedSchema_PromQL_ChDB runs the PromQL cases over tables
// provisioned at each timestamp precision.
func TestProvisionedSchema_PromQL_ChDB(t *testing.T) {
	for _, precision := range []schema.MetricsTimestampPrecision{schema.MetricsTimestampNanos, schema.MetricsTimestampSeconds} {
		t.Run(precision.String(), func(t *testing.T) {
			dtRunPromQLCases(t, newChDBMetricsTimestampServer(t, provisionedMetricsDDL(precision), precision))
		})
	}
}

// TestProvisionedSchema_TimestampTypes_PromQL_ChDB runs the PromQL cases
// over provisioned tables whose timestamp columns carry each further type
// the boot requirements check accepts — a DateTime64 scale below 9, and a
// timezone on DateTime64(9) and on DateTime — served at the precision that
// type needs. Each answers exactly what the nanosecond-provisioned tables
// answer: a timezone changes how a column displays, never the instant it
// holds, and a coarser scale reads through an exact widening.
func TestProvisionedSchema_TimestampTypes_PromQL_ChDB(t *testing.T) {
	var reference map[string]float64
	t.Run("reference "+schema.MetricsTimestampNanos.String(), func(t *testing.T) {
		reference = dtRunPromQLCases(t, newChDBMetricsTimestampServer(t,
			provisionedMetricsDDL(schema.MetricsTimestampNanos), schema.MetricsTimestampNanos))
	})
	for _, tc := range []struct {
		typ       string
		precision schema.MetricsTimestampPrecision
	}{
		{"DateTime64(3)", schema.MetricsTimestampSeconds},
		{"DateTime64(9, 'UTC')", schema.MetricsTimestampNanos},
		{"DateTime('UTC')", schema.MetricsTimestampSeconds},
		{"DateTime('America/Sao_Paulo')", schema.MetricsTimestampSeconds},
		{"DateTime64(9, 'America/Sao_Paulo')", schema.MetricsTimestampNanos},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			if got, ok := schema.MetricsTimestampPrecisionOfType(tc.typ); !ok || got != tc.precision {
				t.Errorf("%s classifies as %v (ok %v), want %v", tc.typ, got, ok, tc.precision)
			}
			answers := dtRunPromQLCases(t, newChDBMetricsTimestampServer(t, retypedMetricsDDL(tc.typ, tc.precision), tc.precision))
			if len(answers) != len(reference) {
				t.Fatalf("answers %v, reference %v", answers, reference)
			}
			for name, want := range reference {
				if got := answers[name]; got != want {
					t.Errorf("%s = %v over %s, %v over DateTime64(9)", name, got, tc.typ, want)
				}
			}
		})
	}
}

// TestProvisionedSecondsSchema_NanosPrecisionFails_ChDB is the
// known-positive control for the seconds case above: rendering for
// nanosecond precision over the seconds-provisioned tables must fail, which
// proves those tables really carry DateTime timestamps rather than passing
// on DateTime64(9) columns the precision failed to change.
func TestProvisionedSecondsSchema_NanosPrecisionFails_ChDB(t *testing.T) {
	srv := newChDBMetricsTimestampServer(t, provisionedMetricsDDL(schema.MetricsTimestampSeconds), schema.MetricsTimestampNanos)
	dtAssertTimestampQueryRejected(t, srv)
}
