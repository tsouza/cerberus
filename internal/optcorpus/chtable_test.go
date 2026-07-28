package optcorpus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestCorpusCreateTableSQL_Shape pins the rendered MergeTree DDL against the
// dossier schema. The typed chsql builder produces it; this test is the golden
// that catches a column / type / engine / order-by / TTL drift.
func TestCorpusCreateTableSQL_Shape(t *testing.T) {
	t.Parallel()
	sql := corpusCreateTableSQL()

	wantFragments := []string{
		"CREATE TABLE IF NOT EXISTS cerberus_router_corpus (",
		"`event_time` DateTime",
		"`shape_id` LowCardinality(String)",
		"`language` LowCardinality(String)",
		"`normalized_query_hash` UInt64",
		"`n_anchors` UInt32",
		"`fanout` UInt32",
		"`cumulative_d` UInt32",
		"`outer_range` UInt32",
		"`step` UInt32",
		"`route` Enum8('A' = 0, 'B' = 1)",
		"`k_shards` UInt8",
		"`decision_reason` LowCardinality(String)",
		"`read_rows` UInt64",
		"`read_bytes` UInt64",
		"`query_duration_ms` UInt64",
		"`memory_usage` UInt64",
		"`exit_status` Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
			"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5, " +
			"'aborted' = 6, 'error' = 7)",
		"ENGINE = MergeTree",
		"ORDER BY (`shape_id`, `n_anchors`, `fanout`)",
		"TTL toDateTime(event_time) + toIntervalDay(30)",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(sql, frag) {
			t.Errorf("DDL missing %q\nfull SQL:\n%s", frag, sql)
		}
	}
}

// fakeExecer records every statement executed and the batch rows appended, and
// answers the deployed-column read with deployedExitType (defaulting to the
// type this binary writes, so construction succeeds unless a test says
// otherwise).
// failStatement / failErr make ONE statement fail while the rest succeed, which
// is how a least-privilege deployment presents (a CH user may hold CREATE but
// not ALTER); execErr fails every statement.
type fakeExecer struct {
	execSQL          []string
	execErr          error
	failStatement    string
	failErr          error
	batchErr         error
	batch            *fakeBatch
	deployedExitType string
	queryErr         error
}

func (f *fakeExecer) Exec(_ context.Context, query string, _ ...any) error {
	f.execSQL = append(f.execSQL, query)
	if f.failStatement != "" && strings.Contains(query, f.failStatement) {
		return f.failErr
	}
	return f.execErr
}

func (f *fakeExecer) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	deployed := f.deployedExitType
	if deployed == "" {
		deployed = chsql.RenderDDL(exitStatusEnumType())
	}
	return &fakeRows{value: deployed}, nil
}

// executed reports whether any recorded statement contains want.
func (f *fakeExecer) executed(want string) bool {
	for _, sql := range f.execSQL {
		if strings.Contains(sql, want) {
			return true
		}
	}
	return false
}

// fakeRows is a single-row, single-String-column driver.Rows: exactly the shape
// the deployed-column read consumes.
type fakeRows struct {
	value string
	done  bool
}

func (r *fakeRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("fakeRows: want exactly one scan destination")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("fakeRows: want a *string scan destination")
	}
	*p = r.value
	return nil
}

func (r *fakeRows) HasData() bool                    { return !r.done }
func (r *fakeRows) ScanStruct(any) error             { return nil }
func (r *fakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeRows) Totals(...any) error              { return nil }
func (r *fakeRows) Columns() []string                { return []string{"type"} }
func (r *fakeRows) Close() error                     { return nil }
func (r *fakeRows) Err() error                       { return nil }

func (f *fakeExecer) PrepareBatch(_ context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	f.batch = &fakeBatch{}
	return f.batch, nil
}

// fakeBatch records appended rows; it implements driver.Batch with the methods
// the sink uses (Append / Send) and no-ops the rest.
type fakeBatch struct {
	rows [][]any
	sent bool
}

func (b *fakeBatch) Append(v ...any) error         { b.rows = append(b.rows, v); return nil }
func (b *fakeBatch) AppendStruct(any) error        { return nil }
func (b *fakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeBatch) Flush() error                  { return nil }
func (b *fakeBatch) Send() error                   { b.sent = true; return nil }
func (b *fakeBatch) Abort() error                  { return nil }
func (b *fakeBatch) IsSent() bool                  { return b.sent }
func (b *fakeBatch) Rows() int                     { return len(b.rows) }
func (b *fakeBatch) Columns() []column.Interface   { return nil }
func (b *fakeBatch) Close() error                  { return nil }

// TestCHTableSink_CreatesTableAndWrites pins the CH-table sink end to end on a
// fake conn: construction runs the CREATE TABLE DDL, and Write streams the Row
// through the columnar batch in the corpus column order.
func TestCHTableSink_CreatesTableAndWrites(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{}
	sink, err := NewCHTableSink(context.Background(), fe)
	if err != nil {
		t.Fatalf("NewCHTableSink: %v", err)
	}
	if !fe.executed("CREATE TABLE IF NOT EXISTS cerberus_router_corpus") {
		t.Fatalf("construction did not run the corpus DDL; got %q", fe.execSQL)
	}
	if !fe.executed("MODIFY COLUMN IF EXISTS `exit_status`") {
		t.Fatalf("construction did not reconcile the exit_status column; got %q", fe.execSQL)
	}

	row := Row{
		ShapeID:        "cerb:agg",
		Language:       "promql",
		NAnchors:       241,
		Fanout:         20,
		CumulativeD:    300,
		OuterRange:     3600,
		Step:           15,
		Route:          "B",
		KShards:        8,
		DecisionReason: "routed",
		ReadRows:       1000,
		MemoryUsage:    2048,
		ExitStatus:     "oom",
	}
	if err := sink.Write([]Row{row}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fe.batch == nil || !fe.batch.sent {
		t.Fatal("batch not sent")
	}
	if len(fe.batch.rows) != 1 {
		t.Fatalf("appended %d rows; want 1", len(fe.batch.rows))
	}
	got := fe.batch.rows[0]
	// 17 columns in the corpus schema (event_time + 16 data columns).
	if len(got) != 17 {
		t.Fatalf("appended %d columns; want 17", len(got))
	}
	// Spot-check the enum mappings and a couple of features in column order:
	// index 2 = language, 9 = route enum, 16 = exit_status enum.
	if got[2] != "promql" {
		t.Errorf("col[2] language = %v, want promql", got[2])
	}
	if got[9] != int8(1) {
		t.Errorf("col[9] route enum = %v, want 1 (B)", got[9])
	}
	if got[16] != int8(1) {
		t.Errorf("col[16] exit_status enum = %v, want 1 (oom)", got[16])
	}
}

// TestRouteEnumValue / TestExitEnumValue pin the string→Enum8 mappings.
func TestRouteEnumValue(t *testing.T) {
	t.Parallel()
	if routeEnumValue("B") != 1 || routeEnumValue("A") != 0 || routeEnumValue("") != 0 {
		t.Error("route enum mapping wrong")
	}
}

func TestExitEnumValue(t *testing.T) {
	t.Parallel()
	if exitEnumValue("ok") != 0 || exitEnumValue("oom") != 1 || exitEnumValue("timeout") != 2 || exitEnumValue("") != 0 {
		t.Error("exit enum mapping wrong")
	}
	// Cerberus-side outcomes must map to their DDL Enum8 values, in lockstep
	// with the ExitStatus iota and the corpusCreateTableSQL Enum8.
	if exitEnumValue("sample_budget") != 3 || exitEnumValue("breaker") != 4 || exitEnumValue("rejected") != 5 {
		t.Error("cerberus-side exit enum mapping wrong")
	}
	// The ClickHouse-side abort / error classes take the next two values.
	if exitEnumValue("aborted") != 6 || exitEnumValue("error") != 7 {
		t.Error("clickhouse-side exit enum mapping wrong")
	}
	// The enum value must round-trip from ExitStatus.String() through
	// exitEnumValue for EVERY status, so a member added to the iota without a
	// mapping cannot slip through.
	for _, status := range exitStatuses {
		if got := exitEnumValue(status.String()); got != int8(status) {
			t.Errorf("exitEnumValue(%q) = %d, want %d", status.String(), got, int8(status))
		}
	}
	if len(exitStatuses) != 8 {
		t.Fatalf("exitStatuses covers %d statuses; the round-trip above must cover all 8", len(exitStatuses))
	}
}

// TestEnum8Members_ParsesDeployedType pins the deployed-type parser against the
// exact rendering ClickHouse returns from system.columns (spaces around `=`),
// including an escaped quote inside a member name.
func TestEnum8Members_ParsesDeployedType(t *testing.T) {
	t.Parallel()
	got := enum8Members(`Enum8('ok' = 0, 'o\'q' = 1, 'error' = 7)`)
	want := []string{"ok", "o'q", "error"}
	if len(got) != len(want) {
		t.Fatalf("parsed %d members (%v); want %d", len(got), got, len(want))
	}
	for _, member := range want {
		if _, ok := got[member]; !ok {
			t.Errorf("member %q missing from %v", member, got)
		}
	}
}

// TestNewCHTableSink_RejectsNarrowDeployedColumn pins that construction FAILS
// when the deployed exit_status column cannot hold a member this binary emits,
// naming it. Without this the sink would return healthy and every batch
// carrying that member would be rejected on every reconcile interval.
func TestNewCHTableSink_RejectsNarrowDeployedColumn(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{deployedExitType: "Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
		"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5)"}
	_, err := NewCHTableSink(context.Background(), fe)
	if err == nil {
		t.Fatal("NewCHTableSink over a narrow exit_status column: want an error, got nil")
	}
	for _, member := range []string{ExitAborted.String(), ExitError.String()} {
		if !strings.Contains(err.Error(), member) {
			t.Errorf("error %q does not name the missing member %q", err, member)
		}
	}
}

// alterExitStatusMarker is the fragment that identifies the exit_status
// widening among the statements construction runs.
const alterExitStatusMarker = "MODIFY COLUMN IF EXISTS `exit_status`"

// TestNewCHTableSink_WideningIsBestEffort pins that a REFUSED widening does not
// by itself disable the sink.
//
// A CH user holding INSERT and CREATE but not ALTER is a routine least-
// privilege grant, and so is an operator-owned table that would need ON
// CLUSTER. On such a deployment the widening is refused on every start while
// the deployed column already holds every member — nothing is wrong, and
// failing construction there would turn the whole corpus off for a statement
// whose work was already done.
func TestNewCHTableSink_WideningIsBestEffort(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{
		failStatement: alterExitStatusMarker,
		failErr:       errors.New("not enough privileges"),
	}
	sink, err := NewCHTableSink(context.Background(), fe)
	if err != nil {
		t.Fatalf("NewCHTableSink over an already-wide column with no ALTER grant: %v", err)
	}
	if sink == nil {
		t.Fatal("NewCHTableSink returned no sink and no error")
	}
	if !fe.executed(alterExitStatusMarker) {
		t.Errorf("construction never attempted the widening; got %q", fe.execSQL)
	}
}

// TestNewCHTableSink_NarrowColumnReportsWideningFailure pins the other half:
// when the column IS too narrow AND the widening was refused, the refusal is
// the operator's actionable cause and must appear in the error. The verify is
// still the authority on whether construction fails.
func TestNewCHTableSink_NarrowColumnReportsWideningFailure(t *testing.T) {
	t.Parallel()

	const grantErr = "not enough privileges"
	fe := &fakeExecer{
		deployedExitType: "Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
			"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5)",
		failStatement: alterExitStatusMarker,
		failErr:       errors.New(grantErr),
	}
	_, err := NewCHTableSink(context.Background(), fe)
	if err == nil {
		t.Fatal("NewCHTableSink over a narrow exit_status column: want an error, got nil")
	}
	if !strings.Contains(err.Error(), grantErr) {
		t.Errorf("error %q does not carry the widening failure %q", err, grantErr)
	}
	for _, member := range []string{ExitAborted.String(), ExitError.String()} {
		if !strings.Contains(err.Error(), member) {
			t.Errorf("error %q does not name the missing member %q", err, member)
		}
	}
}

// TestNewCHTableSink_CreateFailureIsFatal pins that the CREATE remains a hard
// failure: without a table there is nothing to verify and nowhere to write.
func TestNewCHTableSink_CreateFailureIsFatal(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{
		failStatement: "CREATE TABLE IF NOT EXISTS " + CorpusTableName,
		failErr:       errors.New("not enough privileges"),
	}
	if _, err := NewCHTableSink(context.Background(), fe); err == nil {
		t.Fatal("NewCHTableSink with a refused CREATE: want an error, got nil")
	}
}
