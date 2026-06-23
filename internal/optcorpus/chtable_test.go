package optcorpus

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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
		"`exit_status` Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2)",
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

// fakeExecer records the DDL executed and the batch rows appended.
type fakeExecer struct {
	execSQL  string
	execErr  error
	batchErr error
	batch    *fakeBatch
}

func (f *fakeExecer) Exec(_ context.Context, query string, _ ...any) error {
	f.execSQL = query
	return f.execErr
}

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
	if !strings.Contains(fe.execSQL, "CREATE TABLE IF NOT EXISTS cerberus_router_corpus") {
		t.Fatalf("construction did not run the corpus DDL; got %q", fe.execSQL)
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
}
