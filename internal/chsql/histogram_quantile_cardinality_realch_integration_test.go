//go:build integration

package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Exercise the cardinality at which the previous ARRAY JOIN / GROUP BY route
// became more memory-intensive than the legacy per-row implementation.
func assertHQRankWalkCardinality(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	const (
		seriesCount    = 75_000
		queryMemoryCap = 1024 * 1024 * 1024
		table          = "hq_cardinality"
		// Total mass 91 gives rank 45.5, halfway through the first of ten
		// observations in (256,512]: 256 + (0.5/10)*(512-256).
		median = 268.8
	)
	if _, err := db.ExecContext(ctx, "CREATE TABLE hq_cardinality (Attributes Map(String, String), BucketCounts Array(UInt64), ExplicitBounds Array(Float64)) ENGINE = MergeTree ORDER BY tuple()"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO hq_cardinality SELECT map('case', toString(number)), [1,2,3,4,5,6,7,8,9,10,11,12,13], [1.,2.,4.,8.,16.,32.,64.,128.,256.,512.,1024.,2048.] FROM numbers(?)", seriesCount); err != nil {
		t.Fatal(err)
	}
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			tag := fmt.Sprintf("hq-cardinality-native-%t", native)
			queryCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_memory_usage": queryMemoryCap, "log_comment": tag}))
			query := hqRankWalkDiffQuery(table, 0.5, nil, native)
			rows, err := hqRankWalkQueryRows(queryCtx, db, query)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != seriesCount {
				t.Fatalf("got %d series, want %d", len(rows), seriesCount)
			}
			for _, row := range rows {
				if !hqRankWalkValuesAgree(row.value, median) {
					t.Fatalf("series %s: got %v, want %v", row.attrs, row.value, median)
				}
			}
			if _, err := db.ExecContext(ctx, "SYSTEM FLUSH LOGS"); err != nil {
				t.Fatal(err)
			}
			var memory, duration, readRows uint64
			if err := db.QueryRowContext(ctx, "SELECT memory_usage, query_duration_ms, read_rows FROM system.query_log WHERE log_comment = ? AND type = 'QueryFinish'", tag).Scan(&memory, &duration, &readRows); err != nil {
				t.Fatal(err)
			}
			t.Logf("native=%t series=%d memory=%d duration_ms=%d read_rows=%d", native, len(rows), memory, duration, readRows)
		})
	}
}
