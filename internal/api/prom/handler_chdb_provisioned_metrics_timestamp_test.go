//go:build chdb

// chDB-backed end-to-end coverage for the metric tables cerberus provisions
// itself: the tables are created from internal/schema/ddl's RenderAll output
// at each schema.MetricsTimestampPrecision, so the physical shape the
// emitted SQL meets — the fork's templates, the MetricName-first sorting
// key, the curated projections and indexes — is the one
// CERBERUS_AUTO_CREATE_SCHEMA creates. Each precision is served with the
// metrics timestamp strategy the boot requirements check resolves for the
// type it provisions, and answers the same PromQL as the upstream DateTime
// schema (dtRunPromQLCases).

package prom_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
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

// TestProvisionedSchema_PromQL_ChDB runs the PromQL cases over tables
// provisioned at each timestamp precision.
func TestProvisionedSchema_PromQL_ChDB(t *testing.T) {
	cases := []struct {
		precision schema.MetricsTimestampPrecision
		strategy  chsql.MetricsTimestampStrategy
	}{
		{schema.MetricsTimestampNanos, chsql.MetricsTimestampDateTime64},
		{schema.MetricsTimestampSeconds, chsql.MetricsTimestampDateTime},
	}
	for _, c := range cases {
		t.Run(string(c.precision), func(t *testing.T) {
			dtRunPromQLCases(t, newChDBMetricsTimestampServer(t, provisionedMetricsDDL(c.precision), c.strategy))
		})
	}
}

// TestProvisionedSecondsSchema_DateTime64StrategyFails_ChDB is the
// known-positive control for the seconds case above: the DateTime64
// strategy over the seconds-provisioned tables must fail, which proves
// those tables really carry DateTime timestamps rather than passing on
// DateTime64(9) columns the precision failed to change.
func TestProvisionedSecondsSchema_DateTime64StrategyFails_ChDB(t *testing.T) {
	srv := newChDBMetricsTimestampServer(t, provisionedMetricsDDL(schema.MetricsTimestampSeconds), chsql.MetricsTimestampDateTime64)
	dtAssertTimestampQueryRejected(t, srv)
}
