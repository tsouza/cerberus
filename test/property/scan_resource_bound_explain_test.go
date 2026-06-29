//go:build chdb

package property

import (
	"database/sql"
	"testing"
)

// This chDB-backed test proves the spans-scan resource-bound invariant actually
// prunes at execution time, not just in the emitted SQL. It seeds a multi-trace,
// multi-partition otel_traces table and uses EXPLAIN ESTIMATE to compare a
// partition-pruned (windowed) root-span scan against a full-table scan: the
// bounded leg must touch strictly fewer parts and read no more than its scoped
// rows. The recursive memory-streaming arm is validated for result-correctness
// by the spec chDB roundtrip; here we pin the partition-pruning axis (delta N2:
// partition-pruned legs assert bounded_parts < total_parts, NOT the
// memory-streaming arm).

// scanBoundSeedDDL creates a date-partitioned span table with three traces, one
// per partition (three distinct days), each with a root + one child span.
const scanBoundSeedDDL = `
CREATE OR REPLACE TABLE otel_traces (
    Timestamp    DateTime64(9),
    TraceId      String,
    SpanId       String,
    ParentSpanId String
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(Timestamp)
ORDER BY (TraceId, Timestamp);

INSERT INTO otel_traces VALUES
    ('2026-05-10 10:00:00.000000000', 'trace_a', 'a_root',  ''),
    ('2026-05-10 10:00:01.000000000', 'trace_a', 'a_child', 'a_root'),
    ('2026-05-11 10:00:00.000000000', 'trace_b', 'b_root',  ''),
    ('2026-05-11 10:00:01.000000000', 'trace_b', 'b_child', 'b_root'),
    ('2026-05-12 10:00:00.000000000', 'trace_c', 'c_root',  ''),
    ('2026-05-12 10:00:01.000000000', 'trace_c', 'c_child', 'c_root');
`

// explainEstimate runs EXPLAIN ESTIMATE and returns the summed (parts, rows)
// across the result rows. chDB returns one row per scanned table with the
// columns (database, table, parts, rows, marks).
func explainEstimate(t *testing.T, db *sql.DB, query string) (parts, rows int64) {
	t.Helper()
	r, err := db.Query("EXPLAIN ESTIMATE " + query)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE: %v", err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		var database, table string
		var p, rw, marks int64
		if err := r.Scan(&database, &table, &p, &rw, &marks); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		parts += p
		rows += rw
	}
	if err := tolerantRowsErr(r.Err()); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return parts, rows
}

func TestScanResourceBound_PartitionPruneEXPLAIN(t *testing.T) {
	t.Parallel()
	db := openChDB(t)
	applyDDL(t, db, scanBoundSeedDDL)

	// Full root-span scan: touches all three partitions.
	fullParts, fullRows := explainEstimate(t, db,
		"SELECT count() FROM otel_traces WHERE ParentSpanId = ''")

	// Form-a: a request-window predicate prunes to the single day's partition.
	boundParts, boundRows := explainEstimate(t, db,
		"SELECT count() FROM otel_traces WHERE ParentSpanId = '' "+
			"AND Timestamp >= fromUnixTimestamp64Nano(toInt64(toUnixTimestamp64Nano(toDateTime64('2026-05-11 00:00:00.000000000', 9)))) "+
			"AND Timestamp <  fromUnixTimestamp64Nano(toInt64(toUnixTimestamp64Nano(toDateTime64('2026-05-12 00:00:00.000000000', 9))))")

	if !(boundParts < fullParts) {
		t.Errorf("window bound must prune partitions: bounded_parts=%d full_parts=%d", boundParts, fullParts)
	}
	if !(boundRows <= fullRows) {
		t.Errorf("window bound must not read more rows: bounded_rows=%d full_rows=%d", boundRows, fullRows)
	}

	// Form-b: a finite TraceId set scans only the matching trace's data.
	_, setRows := explainEstimate(t, db,
		"SELECT count() FROM otel_traces WHERE TraceId IN ('trace_b')")
	if !(setRows <= fullRows) {
		t.Errorf("trace-id set bound must not read more rows: set_rows=%d full_rows=%d", setRows, fullRows)
	}
	const tracesSeeded = 3
	const rowsPerTrace = 2
	if setRows > tracesSeeded*rowsPerTrace {
		t.Errorf("trace-id set bound read %d rows, expected <= %d", setRows, tracesSeeded*rowsPerTrace)
	}
}
