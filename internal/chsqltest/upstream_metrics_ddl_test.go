package chsqltest

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestUpstreamDateTimeMetricsDDL pins the rendering of the vendored v0.157.0
// templates: one statement per metric table, each naming the database and
// the table, every placeholder consumed, and the timestamp columns typed
// `DateTime` — the property the chDB seeds built from it exist to exercise.
func TestUpstreamDateTimeMetricsDDL(t *testing.T) {
	stmts, err := UpstreamDateTimeMetricsDDL("db1")
	if err != nil {
		t.Fatal(err)
	}
	m := schema.DefaultOTelMetrics()
	tables := []string{m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable}
	if len(stmts) != len(tables) {
		t.Fatalf("got %d statements, want %d", len(stmts), len(tables))
	}
	for i, stmt := range stmts {
		for _, want := range []string{
			`CREATE TABLE IF NOT EXISTS "db1"."` + tables[i] + `"`,
			"ENGINE = " + upstreamMetricsEngine,
			"StartTimeUnix DateTime CODEC",
			"TimeUnix DateTime CODEC",
			"TimeUnix DateTime,",
		} {
			if !strings.Contains(stmt, want) {
				t.Errorf("%s: missing %q", tables[i], want)
			}
		}
		for _, bad := range []string{"%!", "%s", "DateTime64"} {
			if strings.Contains(stmt, bad) {
				t.Errorf("%s: contains %q:\n%s", tables[i], bad, stmt)
			}
		}
	}
}
