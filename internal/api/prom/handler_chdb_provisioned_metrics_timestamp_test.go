//go:build chdb

// chDB-backed end-to-end coverage for the metric tables cerberus provisions
// itself: the tables are created from internal/schema/ddl's RenderAll output
// at each schema.MetricsTimestampPrecision, so the physical shape the
// emitted SQL meets — the fork's templates, the MetricName-first sorting
// key, the curated projections and indexes — is the one
// CERBERUS_AUTO_CREATE_SCHEMA creates. Each table set is served at the
// precision the boot requirements check classifies its timestamp type as
// (schema.MetricsTimestampPrecisionOfType), and answers the same PromQL as
// the upstream DateTime schema (dtRunPromQLCases).

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
// provisioned at the precision typ classifies as, with every timestamp
// column declared typ instead — TimeUnix, StartTimeUnix and the Nested
// Exemplars.TimeUnix alike — failing when the rendered DDL declares no
// timestamp column to retype.
func retypedMetricsDDL(typ string) func(database string) ([]string, error) {
	return func(database string) ([]string, error) {
		precision, ok := schema.MetricsTimestampPrecisionOfType(typ)
		if !ok {
			return nil, fmt.Errorf("%s does not classify as a metrics timestamp precision", typ)
		}
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
// timezone on DateTime64(9) and on DateTime — served at the precision it
// classifies the type as. Every one answers exactly as the provisioned
// types do: a timezone changes how a column displays, never the instant
// it holds, and a coarser scale reads through an exact widening.
func TestProvisionedSchema_TimestampTypes_PromQL_ChDB(t *testing.T) {
	for _, typ := range []string{
		"DateTime64(3)",
		"DateTime64(9, 'UTC')",
		"DateTime('UTC')",
		"DateTime('America/Sao_Paulo')",
		"DateTime64(9, 'America/Sao_Paulo')",
	} {
		t.Run(typ, func(t *testing.T) {
			precision, ok := schema.MetricsTimestampPrecisionOfType(typ)
			if !ok {
				t.Fatalf("%s does not classify as a metrics timestamp precision", typ)
			}
			dtRunPromQLCases(t, newChDBMetricsTimestampServer(t, retypedMetricsDDL(typ), precision))
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
