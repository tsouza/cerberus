package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
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

func TestMetricsTimestampPrecisionOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  string
		want schema.MetricsTimestampPrecision
		ok   bool
	}{
		{"DateTime64(9)", schema.MetricsTimestampNanos, true},
		{"Array(DateTime64(9))", schema.MetricsTimestampNanos, true},
		{"DateTime64(9, 'UTC')", schema.MetricsTimestampNanos, true},
		{"Array(DateTime64(9, 'America/Sao_Paulo'))", schema.MetricsTimestampNanos, true},
		{"DateTime", schema.MetricsTimestampSeconds, true},
		{"Array(DateTime)", schema.MetricsTimestampSeconds, true},
		{" DateTime ", schema.MetricsTimestampSeconds, true},
		{"DateTime('UTC')", schema.MetricsTimestampSeconds, true},
		{"DateTime64(3)", schema.MetricsTimestampSeconds, true},
		{"Array(DateTime64(6, 'UTC'))", schema.MetricsTimestampSeconds, true},
		{"UInt64", schema.MetricsTimestampNanos, false},
		{"Array(String)", schema.MetricsTimestampNanos, false},
		{"", schema.MetricsTimestampNanos, false},
	}
	for _, c := range cases {
		got, ok := metricsTimestampPrecisionOf(c.typ)
		if got != c.want || ok != c.ok {
			t.Errorf("metricsTimestampPrecisionOf(%q) = (%v, %t), want (%v, %t)", c.typ, got, ok, c.want, c.ok)
		}
	}
}

// runMetricsTimestamp runs the boot probe over cols and returns its result.
func runMetricsTimestamp(cols map[string][]chclient.NameTypePair) Result {
	return Run(context.Background(), &stubQuerier{Version: "25.8.2.1", Columns: cols}, defaultReq())
}

// TestRunMetricsTimestampPrecisions pins the deployed timestamp types that
// pass, and the precision each resolves to: DateTime64(9) is the nanosecond
// default; DateTime (the column type the exporter's own metrics templates
// use from v0.157) and every DateTime64 scale below 9 read through a
// conversion to DateTime64(9); and a timezone on any of them changes
// nothing.
func TestRunMetricsTimestampPrecisions(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		typ  string
		want schema.MetricsTimestampPrecision
	}{
		{"DateTime64(9)", schema.MetricsTimestampNanos},
		{"DateTime64(9, 'UTC')", schema.MetricsTimestampNanos},
		{"DateTime", schema.MetricsTimestampSeconds},
		{"DateTime('UTC')", schema.MetricsTimestampSeconds},
		{"DateTime('America/Sao_Paulo')", schema.MetricsTimestampSeconds},
		{"DateTime64(3)", schema.MetricsTimestampSeconds},
	} {
		res := runMetricsTimestamp(withMetricsTimestampTypes(c.typ))
		if res.Fatal != nil {
			t.Errorf("%s timestamps must pass: %v", c.typ, res.Fatal)
			continue
		}
		if got := res.Strategies.Metrics.MetricsTimestamp; got != c.want {
			t.Errorf("%s: MetricsTimestamp = %v, want %v", c.typ, got, c.want)
		}
	}
}

// TestRunMetricsTimestampSamePrecisionTypesMix pins that columns of
// different types that read at the same precision pass together: a
// DateTime TimeUnix beside a DateTime64(3) one both read through the
// conversion to DateTime64(9).
func TestRunMetricsTimestampSamePrecisionTypesMix(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	cols[m.SumTable] = withMetricsTimestampTypes("DateTime64(3, 'UTC')")[m.SumTable]
	res := runMetricsTimestamp(cols)
	if res.Fatal != nil {
		t.Fatalf("same-precision types must pass together: %v", res.Fatal)
	}
	if got := res.Strategies.Metrics.MetricsTimestamp; got != schema.MetricsTimestampSeconds {
		t.Errorf("MetricsTimestamp = %v, want seconds", got)
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
	res := runMetricsTimestamp(cols)
	if res.Fatal != nil {
		t.Fatalf("DateTime TimeUnix alone must pass: %v", res.Fatal)
	}
	if got := res.Strategies.Metrics.MetricsTimestamp; got != schema.MetricsTimestampSeconds {
		t.Errorf("MetricsTimestamp = %v, want seconds", got)
	}
}

// TestRunMetricsTimestampUnsupportedTypeFails pins that a timestamp column
// of a type that is not a ClickHouse timestamp fails boot with a message
// naming the table, the column and the type found — instead of booting
// ready and failing every query.
func TestRunMetricsTimestampUnsupportedTypeFails(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"UInt64", "Int64", "DateTime64(10)"} {
		m := schema.DefaultOTelMetrics()
		cols := withMetricsTimestampTypes("DateTime64(9)")
		cols[m.SumTable] = setColumnType(cols[m.SumTable], m.TimestampColumn, typ)
		res := runMetricsTimestamp(cols)
		if res.Fatal == nil {
			t.Fatalf("timestamp type %s must fail boot", typ)
		}
		want := "table otel_metrics_sum column TimeUnix: expected DateTime or DateTime64, found " + typ
		if !strings.Contains(res.Fatal.Error(), want) {
			t.Errorf("message missing %q: %v", want, res.Fatal)
		}
		if got := res.Strategies.Metrics.MetricsTimestamp; got != schema.MetricsTimestampNanos {
			t.Errorf("a failed probe must leave the zero precision, got %v", got)
		}
	}
}

// TestRunMetricsTimestampMixedPrecisionsFails pins that metric tables whose
// timestamp columns hold different precisions fail boot — one statement
// reads several metric tables at once (a merge() over gauge and sum), so no
// single rendering serves both — and that the message names every column on
// each side and the setting that types the tables cerberus creates: the
// state a nanosecond deployment reaches when the setting is switched to
// seconds and cerberus creates a missing table.
func TestRunMetricsTimestampMixedPrecisionsFails(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime64(9)")
	cols[m.SumTable] = withMetricsTimestampTypes("DateTime")[m.SumTable]
	res := runMetricsTimestamp(cols)
	if res.Fatal == nil {
		t.Fatal("mixed timestamp precisions across tables must fail boot")
	}
	msg := res.Fatal.Error()
	for _, want := range []string{
		"metrics timestamp columns mix precisions",
		"CERBERUS_SCHEMA_METRICS_TIMESTAMP_PRECISION",
		"schema.metricsTimestampPrecision",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %v", want, res.Fatal)
		}
	}
	nanos, seconds := mixedPrecisionSides(t, msg)
	for _, side := range []struct {
		name, list string
		in, out    []string
	}{
		{"nanosecond", nanos, []string{"otel_metrics_gauge.TimeUnix DateTime64(9)", "otel_metrics_histogram.TimeUnix DateTime64(9)"}, []string{"otel_metrics_sum.TimeUnix DateTime"}},
		{"coarser", seconds, []string{"otel_metrics_sum.TimeUnix DateTime"}, []string{"otel_metrics_gauge.", "otel_metrics_histogram."}},
	} {
		for _, col := range side.in {
			if !strings.Contains(side.list, col) {
				t.Errorf("%s side %q does not list %s", side.name, side.list, col)
			}
		}
		for _, col := range side.out {
			if strings.Contains(side.list, col) {
				t.Errorf("%s side %q lists %s", side.name, side.list, col)
			}
		}
	}
}

// mixedPrecisionSides splits a mixed-precision verdict into the column list
// it names as nanosecond and the one it names as coarser.
func mixedPrecisionSides(t *testing.T, msg string) (nanos, seconds string) {
	t.Helper()
	_, rest, ok := strings.Cut(msg, "nanosecond: ")
	if !ok {
		t.Fatalf("verdict names no nanosecond side: %s", msg)
	}
	nanos, rest, ok = strings.Cut(rest, "; coarser: ")
	if !ok {
		t.Fatalf("verdict names no coarser side: %s", msg)
	}
	seconds, _, ok = strings.Cut(rest, " — ")
	if !ok {
		t.Fatalf("verdict's coarser side is unterminated: %s", msg)
	}
	return nanos, seconds
}

// TestRunMetricsTimestampColumnsMixWithinTableFails pins the same verdict
// when the columns of ONE table mix precisions: an exemplar timestamp left
// DateTime64(9) beside a DateTime TimeUnix.
func TestRunMetricsTimestampColumnsMixWithinTableFails(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	cols[m.HistogramTable] = setColumnType(cols[m.HistogramTable], m.ExemplarsColumn+".TimeUnix", "Array(DateTime64(9))")
	res := runMetricsTimestamp(cols)
	if res.Fatal == nil {
		t.Fatal("mixed timestamp precisions within a table must fail boot")
	}
	if want := "otel_metrics_histogram.Exemplars.TimeUnix Array(DateTime64(9))"; !strings.Contains(res.Fatal.Error(), want) {
		t.Errorf("message missing %q: %v", want, res.Fatal)
	}
}

// TestRunMetricsTimestampAbsentTablesResolveFromPresent pins that an
// absent metric table — the transient schema race — contributes nothing:
// the precision resolves from the tables that exist.
func TestRunMetricsTimestampAbsentTablesResolveFromPresent(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	cols := withMetricsTimestampTypes("DateTime")
	delete(cols, m.GaugeTable)
	res := runMetricsTimestamp(cols)
	if res.Fatal != nil {
		t.Fatalf("an absent table must not be fatal: %v", res.Fatal)
	}
	if got := res.Strategies.Metrics.MetricsTimestamp; got != schema.MetricsTimestampSeconds {
		t.Errorf("MetricsTimestamp = %v, want seconds", got)
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
	if got := res.Strategies.Metrics.MetricsTimestamp; got != schema.MetricsTimestampNanos {
		t.Errorf("MetricsTimestamp = %v, want the zero value", got)
	}
}

// TestRunResolvesSumTimestampTimezone pins that the check resolves the
// timezone the sum table's sample timestamp column names — the one the
// DELTA-prefix view buckets by — and only that column's.
func TestRunResolvesSumTimestampTimezone(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	const zoned = "DateTime64(9, 'America/Sao_Paulo')"
	sumZoned := withMetricsTimestampTypes("DateTime64(9)")
	sumZoned[m.SumTable] = setColumnType(sumZoned[m.SumTable], m.TimestampColumn, zoned)
	othersZoned := withMetricsTimestampTypes(zoned)
	othersZoned[m.SumTable] = setColumnType(othersZoned[m.SumTable], m.TimestampColumn, "DateTime64(9)")
	for name, c := range map[string]struct {
		cols map[string][]chclient.NameTypePair
		want string
	}{
		"sum zoned":    {sumZoned, "America/Sao_Paulo"},
		"others zoned": {othersZoned, ""},
		"none zoned":   {withMetricsTimestampTypes("DateTime64(9)"), ""},
	} {
		res := runMetricsTimestamp(c.cols)
		if res.Fatal != nil {
			t.Fatalf("%s: %v", name, res.Fatal)
		}
		if got := res.Strategies.Metrics.SumTimestampTimezone; got != c.want {
			t.Errorf("%s: sum timestamp timezone = %q, want %q", name, got, c.want)
		}
	}
}
