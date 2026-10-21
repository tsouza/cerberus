package chsqltest

import (
	"embed"
	"fmt"

	"github.com/tsouza/cerberus/internal/schema"
)

// upstreamMetricsTemplateDir holds the OTel-CH exporter's metrics CREATE
// TABLE templates exactly as open-telemetry/opentelemetry-collector-contrib
// ships them at v0.157.0
// (exporter/clickhouseexporter/internal/sqltemplates/metrics_*_table.sql,
// Apache-2.0) — the first release whose metrics schema types TimeUnix,
// StartTimeUnix and Exemplars.TimeUnix `DateTime` instead of
// `DateTime64(9)`. They are copied verbatim, not rewritten, so a chDB seed
// rendered from them has the physical shape a v0.157+ collector creates.
const upstreamMetricsTemplateDir = "testdata/clickhouseexporter-v0.157.0"

//go:embed testdata/clickhouseexporter-v0.157.0/*.sql
var upstreamMetricsTemplates embed.FS

// upstreamMetricsEngine is the table engine the seeds render the templates
// with; a single-node chDB session has no replication.
const upstreamMetricsEngine = "MergeTree()"

// UpstreamDateTimeMetricsDDL renders CREATE TABLE statements for the four
// metric tables PromQL reads (gauge, sum, histogram, exponential
// histogram), in database, from the exporter's v0.157.0 templates — the
// upstream metrics schema whose timestamp columns are `DateTime`. Each
// template's placeholders take the database, the table name from
// schema.DefaultOTelMetrics, no ON CLUSTER clause, upstreamMetricsEngine
// and no TTL, the same argument order internal/schema/ddl renders the
// fork's templates with.
func UpstreamDateTimeMetricsDDL(database string) ([]string, error) {
	m := schema.DefaultOTelMetrics()
	tables := []struct{ template, table string }{
		{"metrics_gauge_table.sql", m.GaugeTable},
		{"metrics_sum_table.sql", m.SumTable},
		{"metrics_histogram_table.sql", m.HistogramTable},
		{"metrics_exp_histogram_table.sql", m.ExpHistogramTable},
	}
	stmts := make([]string, 0, len(tables))
	for _, tt := range tables {
		tmpl, err := upstreamMetricsTemplates.ReadFile(upstreamMetricsTemplateDir + "/" + tt.template)
		if err != nil {
			return nil, fmt.Errorf("chsqltest: read upstream template %s: %w", tt.template, err)
		}
		const (
			noClusterClause = ""
			noTTL           = ""
		)
		stmts = append(stmts, fmt.Sprintf(string(tmpl), database, tt.table, noClusterClause, upstreamMetricsEngine, noTTL))
	}
	return stmts, nil
}
