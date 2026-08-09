package preflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// dialRefusedErr mimics the error the clickhouse-go/v2 driver surfaces when
// ClickHouse is not accepting connections yet: a *net.OpError wrapping an
// ECONNREFUSED syscall, then wrapped again under the chclient stage prefix
// (preserved via %w, exactly as client.go does). isUnreachable must see
// through both wrappers via errors.As.
func dialRefusedErr() error {
	opErr := &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 9000},
		Err:  os.NewSyscallError("connect", syscall.ECONNREFUSED),
	}
	return fmt.Errorf("chclient: query: %w", opErr)
}

// stubQuerier is the in-memory ClickHouse stub the preflight tests drive.
// It answers the version() scalar from Version (or VersionErr), the volumes of
// the configured storage policy from Volumes (or VolumesErr), and the
// per-table system.columns read from Columns keyed by the bound table
// name (the second positional arg the introspection SQL binds). A table
// absent from Columns reports zero rows (== "not found").
type stubQuerier struct {
	Version    string
	VersionErr error

	// Volumes is the system.storage_policies answer for the tiering gate, in
	// volume_priority order (first = the hot volume). Nil means the policy is
	// not defined on the server.
	Volumes    []string
	VolumesErr error

	Columns      map[string][]chclient.NameTypePair
	ColumnsErr   error
	NoVersionRow bool
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	// Both string-scalar probes land here; the storage-policy gate is the only
	// one reading system.storage_policies, so the table name dispatches them.
	if strings.Contains(sql, "storage_policies") {
		if s.VolumesErr != nil {
			return nil, s.VolumesErr
		}
		return s.Volumes, nil
	}
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
	// Run executed; with enabled=false it must return an all-clear Result
	// without touching the querier.
	res := RunIfEnabled(context.Background(), false, panicQuerier{t}, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("knob off must return no fatal, got: %v", res.Fatal)
	}
	if !res.SchemaProvisioned() {
		t.Fatalf("knob off must report schema provisioned (gates bypassed), got absent: %v", res.AbsentTables)
	}
}

func TestRunIfEnabledOnDelegatesToRun(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "24.3.0", Columns: healthyColumns()}
	if res := RunIfEnabled(context.Background(), true, q, defaultReq()); res.Fatal == nil {
		t.Fatal("knob on must run the gates (too-old version should be fatal)")
	}
}

func TestMinVersionMaxOfApplicable(t *testing.T) {
	t.Parallel()
	// Native rate disabled → base floor.
	off := Requirements{}
	if got := off.minVersion(); got != minCHBase {
		t.Errorf("native-rate off: min=%v, want base %v", got, minCHBase)
	}
	// Native rate enabled → max(base, native). Native (25.9) > base (24.8),
	// so enabling native rate raises the effective floor to the native one.
	on := Requirements{NativeRateEnabled: true}
	got := on.minVersion()
	if !got.AtLeast(minCHBase) || !got.AtLeast(minCHNativeRate) {
		t.Errorf("native-rate on: min=%v must be >= both base %v and native %v", got, minCHBase, minCHNativeRate)
	}
	if got != minCHNativeRate {
		t.Errorf("native-rate on: min=%v, want max-of-applicable = %v (native dominates)", got, minCHNativeRate)
	}
}

func TestRunVersionTooOld(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "24.3.1.2557-stable", Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq()).Fatal
	if err == nil {
		t.Fatal("expected failure for too-old version, got nil")
	}
	if !strings.Contains(err.Error(), "24.3.1.2557-stable is below the required minimum 24.8") {
		t.Errorf("message missing version diff: %v", err)
	}
	if !strings.Contains(err.Error(), "native rate disabled") {
		t.Errorf("message missing rate note: %v", err)
	}
}

func TestRunVersionOKAllPass(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1-lts", Columns: healthyColumns()}
	if err := Run(context.Background(), q, defaultReq()).Fatal; err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

// A known-defective ClickHouse line WARNS but never blocks: the server clears
// the floor, the schema is healthy, and the defect degrades one endpoint rather
// than the gateway. The pins below are the contract the boot log and
// docs/operations.md both rest on — a defective server must not be fatal, and a
// healthy one must not warn.
func TestRunDefectiveLineWarnsButPasses(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "26.5.6.64", Columns: healthyColumns()}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("a defective-line server must still boot, got fatal: %v", res.Fatal)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(res.Warnings), res.Warnings)
	}
	w := res.Warnings[0]
	// The raw server string (not just the line) so the log is self-contained,
	// the affected endpoint so the operator can match it to what they see, and
	// the fixed version so the message says where to go.
	for _, want := range []string{"26.5.6.64", "detected_fields", "26.6"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q: %s", want, w)
		}
	}
}

// The adjacent lines on both sides of the defect are clean — the regression
// does not exist below 26.5 and is fixed above it — so neither may warn.
func TestRunAdjacentLinesDoNotWarn(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"26.4.5.143", "26.6.2.81", "25.8.2.1-lts"} {
		q := &stubQuerier{Version: v, Columns: healthyColumns()}
		res := Run(context.Background(), q, defaultReq())
		if len(res.Warnings) != 0 {
			t.Errorf("version %s must not warn, got: %v", v, res.Warnings)
		}
	}
}

// A too-old server is rejected on the floor gate and never reaches the
// defect lookup, so it carries the fatal alone — no warning noise on a boot
// that is already failing for a different reason.
func TestRunTooOldCarriesNoWarning(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "24.3.1.2557-stable", Columns: healthyColumns()}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("expected fatal for too-old version")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("too-old version must not also warn, got: %v", res.Warnings)
	}
}

// A defective server with a wrong-shape schema must report BOTH: the fatal
// stops the boot, and the warning still explains the server the operator is
// pointed at. Warnings are not swallowed by an unrelated fatal.
func TestRunDefectiveLineWarnsAlongsideFatal(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	// Drop Body from otel_logs so the shape gate has something fatal to say.
	l := schema.DefaultOTelLogs()
	pruned := cols[l.LogsTable][:0:0]
	for _, c := range cols[l.LogsTable] {
		if c.Name == l.BodyColumn {
			continue
		}
		pruned = append(pruned, c)
	}
	cols[l.LogsTable] = pruned

	q := &stubQuerier{Version: "26.5.6.64", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("expected fatal for the missing body column")
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("want the defect warning alongside the fatal, got %d: %v", len(res.Warnings), res.Warnings)
	}
}

func TestRunVersionExactlyAtFloor(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "24.8.0.0", Columns: healthyColumns()}
	if err := Run(context.Background(), q, defaultReq()).Fatal; err != nil {
		t.Fatalf("version exactly at floor must pass, got: %v", err)
	}
}

func TestRunNativeRateRaisesMin(t *testing.T) {
	t.Parallel()
	// Native rate raises the effective floor from base 24.8 to native 25.9.
	// 25.9 meets the raised floor and passes; a 25.6 that clears the base
	// floor (and even has the aggregate) must now fail because native rate is
	// on and the Prometheus-correct window only lands at 25.9.
	req := defaultReq()
	req.NativeRateEnabled = true

	atNative := &stubQuerier{Version: "25.9.0.0", Columns: healthyColumns()}
	if err := Run(context.Background(), atNative, req).Fatal; err != nil {
		t.Fatalf("native-rate on: 25.9 meets the raised floor and must pass, got: %v", err)
	}

	belowNative := &stubQuerier{Version: "25.6.0.0", Columns: healthyColumns()}
	err := Run(context.Background(), belowNative, req).Fatal
	if err == nil {
		t.Fatal("native-rate on: 25.6 (above base 24.8 but below native 25.9) must fail")
	}
	if !strings.Contains(err.Error(), "below the required minimum 25.9") {
		t.Errorf("message missing raised-floor diff: %v", err)
	}
	if !strings.Contains(err.Error(), "native rate enabled") {
		t.Errorf("message missing enabled rate note: %v", err)
	}
}

func TestRunUnparseableVersionFails(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "not-a-version", Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq()).Fatal
	if err == nil {
		t.Fatal("unparseable version must fail (never silently pass)")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("message missing unparseable note: %v", err)
	}
}

func TestRunVersionQueryErrorFails(t *testing.T) {
	t.Parallel()
	// A reachable server that rejects the version query server-side (NOT a
	// transport error) is still fatal — the check ran but couldn't read the
	// version. A connection-refused / dial error is the transient case,
	// covered separately by TestRunUnreachableVersionIsTransient.
	q := &stubQuerier{VersionErr: errors.New("code 241: memory limit exceeded"), Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq()).Fatal
	if err == nil {
		t.Fatal("server-side version query error must fail startup")
	}
	if !strings.Contains(err.Error(), "could not read clickhouse version") {
		t.Errorf("message missing version-read note: %v", err)
	}
}

func TestRunNoVersionRowFails(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{NoVersionRow: true, Columns: healthyColumns()}
	err := Run(context.Background(), q, defaultReq()).Fatal
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
	err := Run(context.Background(), q, defaultReq()).Fatal
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
	err := Run(context.Background(), q, defaultReq()).Fatal
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
	if err := Run(context.Background(), q, defaultReq()).Fatal; err != nil {
		t.Fatalf("LowCardinality map value should pass, got: %v", err)
	}
}

// TestRunAbsentTableIsTransientNotFatal: a single table reporting zero
// columns is "not yet provisioned" — it must NOT be fatal, and must surface
// in AbsentTables so the caller waits (NOT READY) rather than exiting.
func TestRunAbsentTableIsTransientNotFatal(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	l := schema.DefaultOTelLogs()
	delete(cols, l.LogsTable)
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("absent table must NOT be fatal (transient schema race), got: %v", res.Fatal)
	}
	if res.SchemaProvisioned() {
		t.Fatal("absent table must report schema NOT provisioned")
	}
	if got := res.AbsentTables; len(got) != 1 || got[0] != l.LogsTable {
		t.Errorf("AbsentTables = %v, want [%s]", got, l.LogsTable)
	}
	reason := res.AbsentReason()
	if !strings.Contains(reason, "schema not yet provisioned") || !strings.Contains(reason, l.LogsTable) {
		t.Errorf("AbsentReason = %q, want precise not-yet-provisioned reason naming %s", reason, l.LogsTable)
	}
	if !strings.Contains(reason, "table ") || strings.Contains(reason, "tables ") {
		t.Errorf("single absent table should use singular noun: %q", reason)
	}
}

// TestRunAllTablesAbsentTransient: a cold cluster where NOTHING is
// provisioned yet (every table absent) is the canonical cerberus+collector
// startup race — still transient, never fatal, with a plural reason.
func TestRunAllTablesAbsentTransient(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1", Columns: map[string][]chclient.NameTypePair{}}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("all-absent schema must NOT be fatal, got: %v", res.Fatal)
	}
	if res.SchemaProvisioned() {
		t.Fatal("all-absent must report schema NOT provisioned")
	}
	if len(res.AbsentTables) < 2 {
		t.Fatalf("expected multiple absent tables, got %v", res.AbsentTables)
	}
	if reason := res.AbsentReason(); !strings.Contains(reason, "tables ") {
		t.Errorf("multiple absent tables should use plural noun: %q", reason)
	}
}

// TestRunWrongShapeFatalEvenIfOtherTableAbsent: an EXISTING table with a
// wrong shape is fatal (never self-heals) even when a different table is
// merely absent — the fatal bucket wins, the caller exits.
func TestRunWrongShapeFatalEvenIfOtherTableAbsent(t *testing.T) {
	t.Parallel()
	cols := healthyColumns()
	m := schema.DefaultOTelMetrics()
	l := schema.DefaultOTelLogs()
	// otel_logs absent (transient) …
	delete(cols, l.LogsTable)
	// … but otel_metrics_gauge exists with a wrong Attributes type (fatal).
	for i, c := range cols[m.GaugeTable] {
		if c.Name == m.AttributesColumn {
			cols[m.GaugeTable][i].Type = "JSON"
		}
	}
	q := &stubQuerier{Version: "25.8.2.1", Columns: cols}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("wrong-shape table must be fatal even when another table is absent")
	}
	if !strings.Contains(res.Fatal.Error(), "expected Map(String,String), found JSON") {
		t.Errorf("fatal message missing wrong-shape diff: %v", res.Fatal)
	}
	// The absent table is still tracked even though Fatal wins.
	if res.SchemaProvisioned() {
		t.Error("absent table should still be reported alongside the fatal finding")
	}
}

// TestRunIntrospectionErrorIsFatal: an introspection QUERY error (not a
// clean zero-row absence) is fatal — the check could not run, which is not
// the same signal as a cleanly-absent table.
func TestRunIntrospectionErrorIsFatal(t *testing.T) {
	t.Parallel()
	// A server-side introspection rejection (NOT a transport error) is fatal —
	// the check ran against a reachable server but couldn't read the columns.
	// A dial / connection-refused error is transient, covered by
	// TestRunUnreachableTableIsTransient.
	q := &stubQuerier{Version: "25.8.2.1", ColumnsErr: errors.New("code 60: unknown table")}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal == nil {
		t.Fatal("introspection query error must be fatal")
	}
	if !strings.Contains(res.Fatal.Error(), "could not introspect table") {
		t.Errorf("fatal message missing introspection-error note: %v", res.Fatal)
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
	if err := Run(context.Background(), q, req).Fatal; err != nil {
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
	q := &stubQuerier{Version: "24.3.1", Columns: cols}
	err := Run(context.Background(), q, defaultReq()).Fatal
	if err == nil {
		t.Fatal("expected aggregated failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"requirements check failed:",
		"clickhouse version 24.3.1 is below the required minimum 24.8",
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
	if err := Run(context.Background(), counter, req).Fatal; err != nil {
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

// assertTransient is the shared assertion for the unreachable cases: the
// Result must be transient (not fatal, not provisioned), carry the
// Unreachable flag, and render a precise not-reachable reason.
func assertTransient(t *testing.T, res Result) {
	t.Helper()
	if res.Fatal != nil {
		t.Fatalf("unreachable ClickHouse must NOT be fatal (transient boot race), got: %v", res.Fatal)
	}
	if !res.Unreachable {
		t.Fatal("unreachable ClickHouse must set Result.Unreachable")
	}
	if res.SchemaProvisioned() {
		t.Fatal("unreachable ClickHouse must report schema NOT provisioned")
	}
	reason := res.UnreachableReason()
	if !strings.Contains(reason, "clickhouse not reachable") {
		t.Errorf("UnreachableReason = %q, want a precise not-reachable reason", reason)
	}
}

// TestRunUnreachableVersionIsTransient: a connection-refused / dial error on
// the version probe means ClickHouse hasn't come up yet — the residual sibling
// of the absent-schema race. It must be TRANSIENT (Unreachable), never fatal,
// so the pod boots NOT READY and re-probes instead of crash-looping.
func TestRunUnreachableVersionIsTransient(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{VersionErr: dialRefusedErr(), Columns: healthyColumns()}
	assertTransient(t, Run(context.Background(), q, defaultReq()))
}

// TestRunUnreachableTableIsTransient: the version probe succeeds but the
// server drops before the schema gate, so introspection fails with a dial
// error. That too is transient — abandon the shape gate and wait, don't crash.
func TestRunUnreachableTableIsTransient(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1", ColumnsErr: dialRefusedErr()}
	assertTransient(t, Run(context.Background(), q, defaultReq()))
}

// TestRunUnreachableVersionShortCircuitsShapeGate: an unreachable server on
// the version probe must NOT then run (and fail) the shape gate — the Result
// carries only the Unreachable signal, no wrong-shape fatal even though the
// columns would diverge.
func TestRunUnreachableVersionShortCircuitsShapeGate(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{VersionErr: dialRefusedErr(), ColumnsErr: errors.New("code 60: unknown table")}
	res := Run(context.Background(), q, defaultReq())
	assertTransient(t, res)
	if res.Fatal != nil {
		t.Fatalf("unreachable on version probe must short-circuit the shape gate, got fatal: %v", res.Fatal)
	}
}

// unknownDatabaseErr mimics the error the clickhouse-go/v2 driver surfaces when
// the connection's session default database does not exist: a typed
// *clickhouse.Exception with code 81 (UNKNOWN_DATABASE), wrapped under the
// chclient stage prefix (preserved via %w). isDatabaseAbsent must see through
// the wrapper via errors.As. This is the reported cold-cluster failure: even
// SELECT version() fails with this code until the database is created.
func unknownDatabaseErr() error {
	ex := &clickhouse.Exception{
		Code:    chCodeUnknownDatabase,
		Name:    "UNKNOWN_DATABASE",
		Message: "Database otel does not exist",
	}
	return fmt.Errorf("chclient: query: %w", ex)
}

// assertDatabaseAbsentTransient is the shared assertion for the
// not-yet-created-database cases: the Result must be transient (not fatal, not
// provisioned), carry the DatabaseAbsent flag, and render a precise reason.
func assertDatabaseAbsentTransient(t *testing.T, res Result) {
	t.Helper()
	if res.Fatal != nil {
		t.Fatalf("absent database must NOT be fatal (transient cold-cluster race), got: %v", res.Fatal)
	}
	if !res.DatabaseAbsent {
		t.Fatal("absent database must set Result.DatabaseAbsent")
	}
	if res.Unreachable {
		t.Fatal("absent database is reachable; Result.Unreachable must be false")
	}
	if res.SchemaProvisioned() {
		t.Fatal("absent database must report schema NOT provisioned")
	}
	reason := res.DatabaseAbsentReason("otel")
	if !strings.Contains(reason, "otel") || !strings.Contains(reason, "not yet provisioned") {
		t.Errorf("DatabaseAbsentReason = %q, want a precise not-yet-provisioned reason naming the database", reason)
	}
}

// TestRunUnknownDatabaseIsTransient is the regression test for the reported
// rc.1 bug: on a cold cluster the configured database does not exist, so the
// version probe fails with UNKNOWN_DATABASE (code 81). The old preflight
// bucketed that as a FATAL "could not read clickhouse version" and cerberus
// exited(1) before anything could create the database. It must instead be
// TRANSIENT — boot NOT READY and re-probe until the database appears.
func TestRunUnknownDatabaseIsTransient(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{VersionErr: unknownDatabaseErr(), Columns: healthyColumns()}
	assertDatabaseAbsentTransient(t, Run(context.Background(), q, defaultReq()))
}

// TestRunUnknownDatabaseStringFallback: when the driver wraps the exception
// opaquely enough to defeat errors.As (no typed *clickhouse.Exception in the
// chain), the narrow string fallback on the UNKNOWN_DATABASE message still
// classifies it as the transient absent-database case.
func TestRunUnknownDatabaseStringFallback(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{
		VersionErr: errors.New("clickhouse: Database otel does not exist (UNKNOWN_DATABASE)"),
		Columns:    healthyColumns(),
	}
	assertDatabaseAbsentTransient(t, Run(context.Background(), q, defaultReq()))
}

// TestRunUnknownDatabaseShortCircuitsShapeGate: an absent database on the
// version probe must NOT then run (and fail) the shape gate — the Result
// carries only the DatabaseAbsent signal, no wrong-shape fatal even though the
// columns would diverge.
func TestRunUnknownDatabaseShortCircuitsShapeGate(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{VersionErr: unknownDatabaseErr(), ColumnsErr: errors.New("code 60: unknown table")}
	res := Run(context.Background(), q, defaultReq())
	assertDatabaseAbsentTransient(t, res)
	if res.Fatal != nil {
		t.Fatalf("absent database on version probe must short-circuit the shape gate, got fatal: %v", res.Fatal)
	}
}

// TestRunUnreachableSubstringFallback: when the driver wraps the dial failure
// opaquely (a plain error whose message carries transport vocabulary but no
// *net.OpError in the chain), the named substring fallback still classifies it
// transient.
func TestRunUnreachableSubstringFallback(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{
		VersionErr: errors.New("chclient: query: dial tcp clickhouse:9000: connect: connection refused"),
		Columns:    healthyColumns(),
	}
	assertTransient(t, Run(context.Background(), q, defaultReq()))
}

// --- Gate 3: storage tiering ---

// supportedVersion clears the preflight version floor so a storage-tiering test
// exercises gate 3 rather than tripping gate 1 first.
const supportedVersion = "25.8.1.1"

// tieringReq is defaultReq plus the storage-policy / tiering configuration
// under test.
func tieringReq(policy, tierVolume string) Requirements {
	req := defaultReq()
	req.StoragePolicy = policy
	req.TierVolume = tierVolume
	return req
}

// tieringQuerier is a healthy server whose named storage policy has the given
// volumes.
func tieringQuerier(volumes ...string) *stubQuerier {
	return &stubQuerier{Version: supportedVersion, Columns: healthyColumns(), Volumes: volumes}
}

// assertWarns asserts exactly one warning is reported and that it names each
// of the given substrings — the operator-actionable parts of the message.
func assertWarns(t *testing.T, res Result, substrings ...string) {
	t.Helper()
	if res.Fatal != nil {
		t.Fatalf("storage tiering must never be fatal, got: %v", res.Fatal)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(res.Warnings), res.Warnings)
	}
	for _, sub := range substrings {
		if !strings.Contains(res.Warnings[0], sub) {
			t.Errorf("warning does not mention %q: %s", sub, res.Warnings[0])
		}
	}
}

// TestStoragePolicyMultiVolumeWithNoTieringWarns is the gate this whole change
// exists for: a hot/cold policy is attached to every table and nothing tiers
// into it, so data sits on the expensive volume until retention deletes it.
// The configuration is accepted, nothing errors, and the storage bill is the
// only symptom — so the boot says so, loudly, naming the fix.
func TestStoragePolicyMultiVolumeWithNoTieringWarns(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	res := Run(context.Background(), q, tieringReq("s3_tiered", ""))
	assertWarns(
		t, res,
		`storage policy "s3_tiered"`,
		"2 volumes",
		"hot, cold",
		"CERBERUS_SCHEMA_TIER_VOLUME",
	)
}

// TestStoragePolicySingleVolumeIsSilent pins the other half: a single-volume
// policy (the bundled-chart object-store shape) has nothing to tier into, so
// demanding a tiering rule there would be noise — and noise is how a real
// warning gets ignored.
func TestStoragePolicySingleVolumeIsSilent(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("main")
	res := Run(context.Background(), q, tieringReq("bwc_object_store", ""))
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("single-volume policy must warn about nothing, got: %v", res.Warnings)
	}
}

// TestStoragePolicyTieringConfiguredIsSilent pins that a policy WITH a tiering
// rule — the fixed configuration — is silent, so acting on the warning makes it
// go away.
func TestStoragePolicyTieringConfiguredIsSilent(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	res := Run(context.Background(), q, tieringReq("s3_tiered", "cold"))
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("a tiered policy with a move rule must be silent, got: %v", res.Warnings)
	}
}

// TestStoragePolicyUnknownTierVolumeWarns covers the inert configuration a
// CREATE TABLE cannot surface: on a cluster whose tables already exist the
// create is a no-op, so a tiering volume the policy does not have never fails
// anywhere — it just never moves anything.
func TestStoragePolicyUnknownTierVolumeWarns(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	res := Run(context.Background(), q, tieringReq("s3_tiered", "frozen"))
	assertWarns(t, res, `"frozen"`, `storage policy "s3_tiered"`, "hot, cold")
}

// TestStoragePolicyUndefinedWarns covers a policy name the server does not
// define at all — every table cerberus auto-creates with it would be rejected.
func TestStoragePolicyUndefinedWarns(t *testing.T) {
	t.Parallel()
	q := tieringQuerier()
	res := Run(context.Background(), q, tieringReq("typo_tiered", ""))
	assertWarns(t, res, `"typo_tiered"`, "not defined")
}

// TestStoragePolicyDefaultsToServerDefaultPolicy pins the no-storage-policy
// case: a tiering rule is legitimate against the server's own `default` policy
// (volumes can be declared on it in storage_configuration), so the gate probes
// that policy by name rather than skipping the check.
func TestStoragePolicyDefaultsToServerDefaultPolicy(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot")
	res := Run(context.Background(), q, tieringReq("", "cold"))
	assertWarns(t, res, `"cold"`, `storage policy "default"`)
}

// TestStoragePolicyGateSkippedWhenUnconfigured pins that the default
// deployment — no storage policy, no tiering — pays for no probe at all.
func TestStoragePolicyGateSkippedWhenUnconfigured(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	res := Run(context.Background(), q, defaultReq())
	if len(res.Warnings) != 0 {
		t.Fatalf("no storage config → no tiering warnings, got: %v", res.Warnings)
	}
}

// TestStoragePolicyProbeErrorWarns pins that an unreadable
// system.storage_policies is reported rather than silently treated as "fine" —
// a check that cannot run must not look like a check that passed.
func TestStoragePolicyProbeErrorWarns(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	q.VolumesErr = errors.New("code 497: not enough privileges")
	res := Run(context.Background(), q, tieringReq("s3_tiered", ""))
	assertWarns(t, res, "could not verify storage policy", "not enough privileges")
}

// TestStoragePolicyProbeUnreachableIsTransient pins that a server that drops
// mid-gate lands in the transient bucket, like every other probe here — not as
// a tiering finding about a server that never answered.
func TestStoragePolicyProbeUnreachableIsTransient(t *testing.T) {
	t.Parallel()
	q := tieringQuerier("hot", "cold")
	q.VolumesErr = dialRefusedErr()
	res := Run(context.Background(), q, tieringReq("s3_tiered", ""))
	if !res.Unreachable {
		t.Fatalf("want Unreachable, got %+v", res)
	}
	if res.Fatal != nil {
		t.Fatalf("unreachable must not be fatal, got: %v", res.Fatal)
	}
}

// TestIsUnreachableClassification pins the typed vs server-side split that the
// fatal/transient routing depends on.
func TestIsUnreachableClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed-dial-refused", dialRefusedErr(), true},
		{"net-timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"substring-no-route", errors.New("dial tcp: no route to host"), true},
		{"substring-conn-reset", errors.New("read: connection reset by peer"), true},
		{"server-side-memory", errors.New("code 241: memory limit exceeded"), false},
		{"server-side-unknown-table", errors.New("code 60: unknown table"), false},
		{"too-old-shape-text", errors.New("table otel_logs: missing required column Body"), false},
	}
	for _, tc := range cases {
		if got := isUnreachable(tc.err); got != tc.want {
			t.Errorf("isUnreachable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
