package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// stubQuerier is the in-memory ClickHouse stub the preflight tests drive.
// It answers the version() scalar from Version (or VersionErr) and the
// per-table system.columns read from Columns keyed by the bound table
// name (the second positional arg the introspection SQL binds). A table
// absent from Columns reports zero rows (== "not found").
type stubQuerier struct {
	Version    string
	VersionErr error

	Columns      map[string][]chclient.NameTypePair
	ColumnsErr   error
	NoVersionRow bool
}

func (s *stubQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	if s.VersionErr != nil {
		return nil, s.VersionErr
	}
	if s.NoVersionRow {
		return nil, nil
	}
	return []string{s.Version}, nil
}

func (s *stubQuerier) QueryNameTypePairs(_ context.Context, _ string, args ...any) ([]chclient.NameTypePair, error) {
	if s.ColumnsErr != nil {
		return nil, s.ColumnsErr
	}
	// The introspection SQL binds (database, table); the table name is the
	// second positional arg. Dispatch the stubbed columns on it.
	if len(args) < 2 {
		return nil, errors.New("stub: expected (database, table) args")
	}
	table, _ := args[1].(string)
	return s.Columns[table], nil
}

// mapType is the canonical attribute-map type the deployed schema reports.
const mapType = "Map(String, String)"

// healthyColumns builds a system.columns response for every table the
// default OTel schema requires, each carrying exactly the essential
// columns with the right types. Tests clone + mutate it to inject
// divergence.
func healthyColumns() map[string][]chclient.NameTypePair {
	m := schema.DefaultOTelMetrics()
	l := schema.DefaultOTelLogs()
	tr := schema.DefaultOTelTraces()

	col := func(name, typ string) chclient.NameTypePair {
		return chclient.NameTypePair{Name: name, Type: typ}
	}
	attr := func(names ...string) []chclient.NameTypePair {
		out := make([]chclient.NameTypePair, 0, len(names))
		for _, n := range names {
			if n != "" {
				out = append(out, col(n, mapType))
			}
		}
		return out
	}

	out := map[string][]chclient.NameTypePair{}

	sampleCols := func() []chclient.NameTypePair {
		base := []chclient.NameTypePair{
			col(m.MetricNameColumn, "String"),
			col(m.TimestampColumn, "DateTime64(9)"),
			col(m.ValueColumn, "Float64"),
			col(m.ServiceNameColumn, "LowCardinality(String)"),
		}
		return append(base, attr(m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn)...)
	}
	histCols := func() []chclient.NameTypePair {
		base := []chclient.NameTypePair{
			col(m.MetricNameColumn, "String"),
			col(m.TimestampColumn, "DateTime64(9)"),
			col(m.CountColumn, "UInt64"),
			col(m.SumColumn, "Float64"),
		}
		return append(base, attr(m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn)...)
	}

	out[m.GaugeTable] = sampleCols()
	out[m.SumTable] = sampleCols()
	out[m.HistogramTable] = histCols()
	out[m.ExpHistogramTable] = histCols()

	out[l.LogsTable] = append([]chclient.NameTypePair{
		col(l.TimestampColumn, "DateTime64(9)"),
		col(l.BodyColumn, "String"),
		col(l.ServiceNameColumn, "LowCardinality(String)"),
	}, attr(l.AttributesColumn, l.ResourceAttributesColumn, l.ScopeAttributesColumn)...)

	out[tr.SpansTable] = append([]chclient.NameTypePair{
		col(tr.TraceIDColumn, "String"),
		col(tr.SpanIDColumn, "String"),
		col(tr.SpanNameColumn, "LowCardinality(String)"),
		col(tr.ServiceNameColumn, "LowCardinality(String)"),
		col(tr.DurationColumn, "UInt64"),
		col(tr.StartTimeColumn, "DateTime64(9)"),
	}, attr(tr.AttributesColumn, tr.ResourceAttributesColumn, tr.ScopeAttributesColumn)...)

	return out
}

// defaultReq is the active-config requirement for the default OTel schema.
func defaultReq() Requirements {
	return Requirements{
		Database: "otel",
		Metrics:  schema.DefaultOTelMetrics(),
		Logs:     schema.DefaultOTelLogs(),
		Traces:   schema.DefaultOTelTraces(),
	}
}

// panicQuerier fails the test if either gate touches ClickHouse — used to
// prove the knob-off path skips both gates entirely.
type panicQuerier struct{ t *testing.T }

func (p panicQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	p.t.Fatal("knob off: version gate must not query ClickHouse")
	return nil, nil
}

func (p panicQuerier) QueryNameTypePairs(context.Context, string, ...any) ([]chclient.NameTypePair, error) {
	p.t.Fatal("knob off: schema gate must not query ClickHouse")
	return nil, nil
}

func TestRunIfEnabledOffSkipsBothGates(t *testing.T) {
	t.Parallel()
	// A deliberately-broken requirement (empty everything) would fail if
	// Run executed; with enabled=false it must return nil without touching
	// the querier.
	if err := RunIfEnabled(context.Background(), false, panicQuerier{t}, defaultReq()); err != nil {
		t.Fatalf("knob off must return nil, got: %v", err)
	}
}

func TestRunIfEnabledOnDelegatesToRun(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.4.0", Columns: healthyColumns()}
	if err := RunIfEnabled(context.Background(), true, q, defaultReq()); err == nil {
		t.Fatal("knob on must run the gates (too-old version should fail)")
	}
}

func TestParseCHVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		want      chVersion
		wantOK    bool
		wantMajor int
	}{
		{in: "25.8.2.1", want: chVersion{25, 8}, wantOK: true},
		{in: "25.8.2.1-lts", want: chVersion{25, 8}, wantOK: true},
		{in: "24.3.1.2557-stable", want: chVersion{24, 3}, wantOK: true},
		{in: "25.6", want: chVersion{25, 6}, wantOK: true},
		{in: "  25.8.2.1  ", want: chVersion{25, 8}, wantOK: true},
		{in: "25", wantOK: false},
		{in: "lts.8", wantOK: false},
		{in: "", wantOK: false},
		{in: "garbage", wantOK: false},
	}
	for _, tc := range cases {
		got, ok := parseCHVersion(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseCHVersion(%q): ok=%v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseCHVersion(%q): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMinVersionMaxOfApplicable(t *testing.T) {
	t.Parallel()
	// Native rate disabled → base floor.
	off := Requirements{}
	if got := off.minVersion(); got != minCHBase {
		t.Errorf("native-rate off: min=%v, want base %v", got, minCHBase)
	}
	// Native rate enabled → max(base, native). Base (25.8) > native (25.6),
	// so the base still wins today — but the result must be the max, never
	// the lower native floor.
	on := Requirements{NativeRateEnabled: true}
	got := on.minVersion()
	if !got.atLeast(minCHBase) || !got.atLeast(minCHNativeRate) {
		t.Errorf("native-rate on: min=%v must be >= both base %v and native %v", got, minCHBase, minCHNativeRate)
	}
	if got != minCHBase {
		t.Errorf("native-rate on: min=%v, want max-of-applicable = %v (base dominates)", got, minCHBase)
	}
}

func TestRunVersionTooOld(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.4.1.2", Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("expected failure for too-old version, got nil")
	}
	if !strings.Contains(err.Error(), "25.4.1.2 is below the required minimum 25.8") {
		t.Errorf("message missing version diff: %v", err)
	}
	if !strings.Contains(err.Error(), "native rate disabled") {
		t.Errorf("message missing rate note: %v", err)
	}
}

func TestRunVersionOKAllPass(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1-lts", Columns: healthyColumns()}
	if err := Run(context.Background(), q, defaultReq()); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

func TestRunVersionExactlyAtFloor(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.0.0", Columns: healthyColumns()}
	if err := Run(context.Background(), q, defaultReq()); err != nil {
		t.Fatalf("version exactly at floor must pass, got: %v", err)
	}
}

func TestRunNativeRateRaisesMinAndStillPasses(t *testing.T) {
	t.Parallel()
	// 25.6 meets the native-rate floor but NOT the base floor; with native
	// rate on the effective min is max(25.8, 25.6)=25.8, so 25.6 fails.
	req := defaultReq()
	req.NativeRateEnabled = true
	q := &stubQuerier{Version: "25.6.0.0", Columns: healthyColumns()}
	err := Run(context.Background(), q, req)
	if err == nil {
		t.Fatal("native-rate on: 25.6 below the 25.8 base floor must fail")
	}
	if !strings.Contains(err.Error(), "native rate enabled") {
		t.Errorf("message missing enabled rate note: %v", err)
	}
}

func TestRunUnparseableVersionFails(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "not-a-version", Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("unparseable version must fail (never silently pass)")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("message missing unparseable note: %v", err)
	}
}

func TestRunVersionQueryErrorFails(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{VersionErr: errors.New("connection refused"), Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("version query error must fail startup")
	}
	if !strings.Contains(err.Error(), "could not read clickhouse version") {
		t.Errorf("message missing version-read note: %v", err)
	}
}

func TestRunNoVersionRowFails(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{NoVersionRow: true, Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq())
	if err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("empty version result must fail; got %v", err)
	}
}

func TestRunMissingColumnFails(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	// Drop ServiceName from otel_traces.
	tr := schema.DefaultOTelTraces()
	pruned := cols[tr.SpansTable][:0:0]
	for _, c := range cols[tr.SpansTable] {
		if c.Name == tr.ServiceNameColumn {
			continue
		}
		pruned = append(pruned, c)
	}
	cols[tr.SpansTable] = pruned

	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("missing column must fail")
	}
	want := "table otel_traces: missing required column ServiceName"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("message missing %q: %v", want, err)
	}
}

func TestRunWrongAttributeTypeFails(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	m := schema.DefaultOTelMetrics()
	// Corrupt the Attributes column type on otel_metrics_gauge.
	for i, c := range cols[m.GaugeTable] {
		if c.Name == m.AttributesColumn {
			cols[m.GaugeTable][i].Type = "JSON"
		}
	}
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("wrong attribute-map type must fail")
	}
	want := "table otel_metrics_gauge column Attributes: expected Map(String,String), found JSON"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("message missing %q: %v", want, err)
	}
}

func TestRunLowCardinalityMapAccepted(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	m := schema.DefaultOTelMetrics()
	// Some exporters emit Map(String, LowCardinality(String)); it must be
	// accepted as the expected map shape.
	for i, c := range cols[m.SumTable] {
		if c.Name == m.AttributesColumn {
			cols[m.SumTable][i].Type = "Map(String, LowCardinality(String))"
		}
	}
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	if err := Run(context.Background(), q, defaultReq()); err != nil {
		t.Fatalf("LowCardinality map value should pass, got: %v", err)
	}
}

func TestRunMissingTableFails(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	l := schema.DefaultOTelLogs()
	delete(cols, l.LogsTable)
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("missing table must fail")
	}
	if !strings.Contains(err.Error(), "table otel_logs: not found") {
		t.Errorf("message missing not-found note: %v", err)
	}
}

func TestRunRespectsOverrides(t *testing.T) {
	t.Parallel()
	// Operator renamed the gauge table; the deployed schema only has the
	// renamed table. Validating against the override-resolved name must
	// pass; the default name must NOT be probed.
	req := defaultReq()
	req.Metrics.GaugeTable = "my_gauge"

	cols := healthyColumns()
	m := schema.DefaultOTelMetrics()
	cols["my_gauge"] = cols[m.GaugeTable]
	delete(cols, m.GaugeTable) // the default name no longer exists

	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	if err := Run(context.Background(), q, req); err != nil {
		t.Fatalf("override-resolved schema should pass, got: %v", err)
	}
}

func TestRunAggregatesAllFailures(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	m := schema.DefaultOTelMetrics()
	tr := schema.DefaultOTelTraces()

	// 1: wrong attribute type on gauge.
	for i, c := range cols[m.GaugeTable] {
		if c.Name == m.AttributesColumn {
			cols[m.GaugeTable][i].Type = "JSON"
		}
	}
	// 2: missing ServiceName on traces.
	pruned := cols[tr.SpansTable][:0:0]
	for _, c := range cols[tr.SpansTable] {
		if c.Name != tr.ServiceNameColumn {
			pruned = append(pruned, c)
		}
	}
	cols[tr.SpansTable] = pruned

	// 3: version too old.
	q := &stubQuerier{Version: "25.4.1", Columns: cols}
	err := Run(context.Background(), q, defaultReq())
	if err == nil {
		t.Fatal("expected aggregated failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"startup preflight failed:",
		"clickhouse version 25.4.1 is below the required minimum 25.8",
		"table otel_metrics_gauge column Attributes: expected Map(String,String), found JSON",
		"table otel_traces: missing required column ServiceName",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated message missing %q:\n%s", want, msg)
		}
	}
	// Aggregation: at least three bullet lines.
	if n := strings.Count(msg, "\n  - "); n < 3 {
		t.Errorf("expected >=3 aggregated bullets, got %d:\n%s", n, msg)
	}
}

func TestRunCollapsedGaugeSumIntrospectedOnce(t *testing.T) {
	t.Parallel()
	// When Gauge and Sum resolve to the same physical table, the table is
	// introspected once — verify by counting distinct table queries.
	req := defaultReq()
	req.Metrics.SumTable = req.Metrics.GaugeTable

	cols := healthyColumns()
	counter := &countingQuerier{stubQuerier: stubQuerier{Version: "25.8.2.1", Columns: cols}}
	if err := Run(context.Background(), counter, req); err != nil {
		t.Fatalf("collapsed gauge/sum should pass, got: %v", err)
	}
	if got := counter.tableQueries[req.Metrics.GaugeTable]; got != 1 {
		t.Errorf("gauge table introspected %d times, want 1 (dedup)", got)
	}
}

// countingQuerier wraps stubQuerier to record how many times each table
// was introspected — used to assert the same-physical-name dedup.
type countingQuerier struct {
	stubQuerier
	tableQueries map[string]int
}

func (c *countingQuerier) QueryNameTypePairs(ctx context.Context, sql string, args ...any) ([]chclient.NameTypePair, error) {
	if c.tableQueries == nil {
		c.tableQueries = map[string]int{}
	}
	if len(args) >= 2 {
		if table, ok := args[1].(string); ok {
			c.tableQueries[table]++
		}
	}
	return c.stubQuerier.QueryNameTypePairs(ctx, sql, args...)
}
