package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// metricTables lists the metric tables the boot probe introspects.
func metricTables(m schema.Metrics) []string {
	return []string{m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable}
}

// withMetricsTimestampTypes returns healthyColumns with every metric
// table's timestamp columns typed typ: TimeUnix and StartTimeUnix typ, and
// the Nested Exemplars.TimeUnix Array(typ), as system.columns reports them.
func withMetricsTimestampTypes(typ string) map[string][]chclient.NameTypePair {
	m := schema.DefaultOTelMetrics()
	cols := healthyColumns()
	for _, table := range metricTables(m) {
		cols[table] = setColumnType(cols[table], m.TimestampColumn, typ)
		cols[table] = setColumnType(cols[table], m.StartTimeColumn, typ)
		cols[table] = setColumnType(cols[table], m.ExemplarsColumn+".TimeUnix", "Array("+typ+")")
	}
	return cols
}

// setColumnType sets column's type in cols, appending the column when cols
// lacks it.
func setColumnType(cols []chclient.NameTypePair, column, typ string) []chclient.NameTypePair {
	for i := range cols {
		if cols[i].Name == column {
			cols[i].Type = typ
			return cols
		}
	}
	return append(cols, chclient.NameTypePair{Name: column, Type: typ})
}

func TestMetricsTimestampStrategyFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  string
		want chsql.MetricsTimestampStrategy
		ok   bool
	}{
		{"DateTime64(9)", chsql.MetricsTimestampDateTime64, true},
		{"Array(DateTime64(9))", chsql.MetricsTimestampDateTime64, true},
		{"DateTime", chsql.MetricsTimestampDateTime, true},
		{"Array(DateTime)", chsql.MetricsTimestampDateTime, true},
		{" DateTime ", chsql.MetricsTimestampDateTime, true},
		{"DateTime64(3)", chsql.MetricsTimestampDateTime64, false},
		{"DateTime64(9, 'UTC')", chsql.MetricsTimestampDateTime64, false},
		{"DateTime('UTC')", chsql.MetricsTimestampDateTime64, false},
		{"UInt64", chsql.MetricsTimestampDateTime64, false},
		{"Array(String)", chsql.MetricsTimestampDateTime64, false},
		{"", chsql.MetricsTimestampDateTime64, false},
	}
	for _, c := range cases {
		got, ok := metricsTimestampStrategyFor(c.typ)
		if got != c.want || ok != c.ok {
			t.Errorf("metricsTimestampStrategyFor(%q) = (%v, %t), want (%v, %t)", c.typ, got, ok, c.want, c.ok)
		}
	}
}

// TestRunMetricsTimestampDateTime64 pins the schema every deployment before
// the exporter's v0.157 has: every timestamp column DateTime64(9) resolves
// to the zero strategy and passes.
func TestRunMetricsTimestampDateTime64(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1", Columns: withMetricsTimestampTypes("DateTime64(9)")}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("DateTime64(9) timestamps must pass: %v", res.Fatal)
	}
	if res.MetricsTimestamp != chsql.MetricsTimestampDateTime64 {
		t.Errorf("MetricsTimestamp = %v, want DateTime64", res.MetricsTimestamp)
	}
}

// TestRunMetricsTimestampDateTime pins the exporter's v0.157 metrics schema:
// every timestamp column DateTime (the exemplar one Array(DateTime))
// resolves to MetricsTimestampDateTime and passes.
func TestRunMetricsTimestampDateTime(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1", Columns: withMetricsTimestampTypes("DateTime")}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("DateTime timestamps must pass: %v", res.Fatal)
	}
	if res.MetricsTimestamp != chsql.MetricsTimestampDateTime {
		t.Errorf("MetricsTimestamp = %v, want DateTime", res.MetricsTimestamp)
	}
}

// TestRunMetricsTimestampOnlyRequiredColumn pins that StartTimeUnix and
// Exemplars.TimeUnix are probed only when present: a table carrying just
// TimeUnix resolves from that column alone.
func TestRunMetricsTimestampOnlyRequiredColumn(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := healthyColumns()
	for _, table := range metricTables(m) {
		cols[table] = setColumnType(cols[table], m.TimestampColumn, "DateTime")
	}
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("DateTime TimeUnix alone must pass: %v", res.Fatal)
	}
	if res.MetricsTimestamp != chsql.MetricsTimestampDateTime {
		t.Errorf("MetricsTimestamp = %v, want DateTime", res.MetricsTimestamp)
	}
}

// TestRunMetricsTimestampUnsupportedTypeFails pins that a timestamp column
// of a type cerberus renders no SQL for fails boot with a message naming
// the table, the column, the two supported types and the type found —
// instead of booting ready and failing every query.
func TestRunMetricsTimestampUnsupportedTypeFails(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"DateTime64(3)", "DateTime64(9, 'UTC')", "UInt64"} {
		m := schema.DefaultOTelMetrics()
		cols := withMetricsTimestampTypes("DateTime64(9)")
		cols[m.SumTable] = setColumnType(cols[m.SumTable], m.TimestampColumn, typ)
		q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
		res := Run(context.Background(), q, defaultReq())
		if res.Fatal == nil {
			t.Fatalf("timestamp type %s must fail boot", typ)
		}
		want := "table otel_metrics_sum column TimeUnix: expected DateTime64(9) or DateTime, found " + typ
		if !strings.Contains(res.Fatal.Error(), want) {
			t.Errorf("message missing %q: %v", want, res.Fatal)
		}
		if res.MetricsTimestamp != chsql.MetricsTimestampDateTime64 {
			t.Errorf("a failed probe must leave the zero strategy, got %v", res.MetricsTimestamp)
		}
	}
}

// TestRunMetricsTimestampTablesDisagreeFails pins that metric tables whose
// timestamp columns resolve to different strategies fail boot: one
// statement reads several metric tables at once (a merge() over gauge and
// sum), so no single rendering serves both.
func TestRunMetricsTimestampTablesDisagreeFails(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime64(9)")
	cols[m.SumTable] = withMetricsTimestampTypes("DateTime")[m.SumTable]
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("disagreeing timestamp types across tables must fail boot")
	}
	for _, want := range []string{
		"metrics timestamp columns disagree",
		"otel_metrics_gauge.TimeUnix DateTime64(9)",
		"otel_metrics_sum.TimeUnix DateTime",
	} {
		if !strings.Contains(res.Fatal.Error(), want) {
			t.Errorf("message missing %q: %v", want, res.Fatal)
		}
	}
}

// TestRunMetricsTimestampColumnsDisagreeWithinTableFails pins the same
// verdict when the columns of ONE table disagree: an exemplar timestamp
// left DateTime64(9) beside a DateTime TimeUnix.
func TestRunMetricsTimestampColumnsDisagreeWithinTableFails(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	cols[m.HistogramTable] = setColumnType(cols[m.HistogramTable], m.ExemplarsColumn+".TimeUnix", "Array(DateTime64(9))")
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("disagreeing timestamp types within a table must fail boot")
	}
	if want := "otel_metrics_histogram.Exemplars.TimeUnix Array(DateTime64(9))"; !strings.Contains(res.Fatal.Error(), want) {
		t.Errorf("message missing %q: %v", want, res.Fatal)
	}
}

// TestRunMetricsTimestampAbsentTablesResolveFromPresent pins that an
// absent metric table — the transient schema race — contributes nothing:
// the strategy resolves from the tables that exist.
func TestRunMetricsTimestampAbsentTablesResolveFromPresent(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	delete(cols, m.GaugeTable)
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("an absent table must not be fatal: %v", res.Fatal)
	}
	if res.MetricsTimestamp != chsql.MetricsTimestampDateTime {
		t.Errorf("MetricsTimestamp = %v, want DateTime", res.MetricsTimestamp)
	}
}

// TestRunMetricsTimestampNotProbedWithoutMetricsSignal pins that a process
// not serving metrics never reads the metric tables' timestamp types, so a
// metrics schema it does not serve cannot fail its boot.
func TestRunMetricsTimestampNotProbedWithoutMetricsSignal(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	cols[m.SumTable] = setColumnType(cols[m.SumTable], m.TimestampColumn, "UInt64")
	req := defaultReq()
	req.Signals = Signals{Logs: true}
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, req)
	if res.Fatal != nil {
		t.Fatalf("a logs-only process must not probe metrics timestamps: %v", res.Fatal)
	}
	if res.MetricsTimestamp != chsql.MetricsTimestampDateTime64 {
		t.Errorf("MetricsTimestamp = %v, want the zero value", res.MetricsTimestamp)
	}
}
