//go:build chdb

package chsql

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
)

// The DELTA-prefix aggregate table is filled by a materialized view that
// buckets the sum table's TimeUnix by toStartOfDay (internal/schema/ddl's
// renderDeltaPrefixView), which runs in the column's own timezone when its
// type names one. These tests seed that table through the same view
// expression over a sum table whose TimeUnix is plain DateTime64(9) and one
// whose TimeUnix names a timezone west and one east of UTC, with DELTA
// history straddling UTC midnight and each zone's midnight, and require the instant and the
// matrix DELTA-prefix reads to reconstruct exactly the level an unbounded
// scan of the sum table does.

// zonedDeltaPrefixHistory is the DELTA history every scenario seeds, as
// (instant, value): 2026-01-15 03:30 UTC (00:30 in São Paulo, its Jan 15),
// 01:00 UTC (22:00 on its Jan 14), 2026-01-14 23:50 UTC, 18:00 UTC (23:30
// in Kolkata, its Jan 14), and 2026-01-10 05:00 UTC. The values are distinct powers of two, so a sample counted
// twice or dropped leaves a level no other mistake produces.
var zonedDeltaPrefixHistory = []struct {
	at    time.Time
	value float64
}{
	{time.Date(2026, 1, 15, 3, 30, 0, 0, time.UTC), 0.5},
	{time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC), 0.25},
	{time.Date(2026, 1, 14, 23, 50, 0, 0, time.UTC), 0.125},
	{time.Date(2026, 1, 14, 18, 0, 0, 0, time.UTC), 0.03125},
	{time.Date(2026, 1, 10, 5, 0, 0, 0, time.UTC), 0.015625},
}

// zonedSumTimestampTypes are the sum table TimeUnix types the scenarios
// run over.
var zonedSumTimestampTypes = []string{
	"DateTime64(9)", "DateTime64(9, 'America/Sao_Paulo')", "DateTime64(9, 'Asia/Kolkata')",
}

// seedZonedDeltaPrefix creates the sum table with TimeUnix typed typ, the
// aggregate table and the view filling it, inserts the history, and returns
// the context carrying the strategies the requirements check resolves for
// that sum table.
func seedZonedDeltaPrefix(t *testing.T, db *sql.DB, typ string) context.Context {
	t.Helper()
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE otel_metrics_sum (
			AggregationTemporality Int32,
			Attributes Map(String, String),
			TimeUnix %s,
			Value Float64
		) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`, typ),
		`CREATE TABLE otel_metrics_sum_delta_prefix (
			Attributes Map(String, String),
			BucketStart DateTime64(9),
			PartialSum Float64
		) ENGINE = MergeTree ORDER BY (Attributes, BucketStart)`,
		`CREATE MATERIALIZED VIEW otel_metrics_sum_delta_prefix_mv TO otel_metrics_sum_delta_prefix AS
			SELECT Attributes, toStartOfDay(TimeUnix) AS BucketStart, sum(Value) AS PartialSum
			FROM otel_metrics_sum WHERE AggregationTemporality = 1
			GROUP BY Attributes, BucketStart`,
	}
	for _, h := range zonedDeltaPrefixHistory {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO otel_metrics_sum VALUES (1, map('job', 'zoned'), fromUnixTimestamp64Nano(toInt64(%d)), %v)`,
			h.at.UnixNano(), h.value,
		))
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	return WithSchemaStrategies(context.Background(), SchemaStrategies{
		SumTimestampTimezone: schema.TimestampTimezoneOfType(typ),
	})
}

// TestDeltaPrefixAggregateSource_ZonedSumTimestamp pins the instant read.
// The two in-window samples sit at 11:59:30 and 12:00:00 UTC with small
// values, so the reconstructed level moves the counter zero-crossing clamp
// and with it the answer.
func TestDeltaPrefixAggregateSource_ZonedSumTimestamp(t *testing.T) {
	for _, typ := range zonedSumTimestampTypes {
		t.Run(typ, func(t *testing.T) {
			db := chsqltest.OpenIsolatedChDB(t)
			ctx := seedZonedDeltaPrefix(t, db, typ)
			insertDeltaPrefixWindowSamples(t, db, `map('job', 'zoned')`, "zoned")

			oracle := runDeltaPrefixQueryIn(ctx, t, db,
				deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, ""), 0, false)
			got := runDeltaPrefixQueryIn(ctx, t, db,
				deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix"), 0, true)
			if len(oracle) != 1 || got["zoned"] != oracle["zoned"] {
				t.Errorf("DELTA-prefix read = %v, unbounded scan = %v", got, oracle)
			}
		})
	}
}

// TestDeltaMatrixAggregateSource_ZonedSumTimestamp pins the matrix read's
// reconstructed level at every grid anchor.
func TestDeltaMatrixAggregateSource_ZonedSumTimestamp(t *testing.T) {
	for _, typ := range zonedSumTimestampTypes {
		t.Run(typ, func(t *testing.T) {
			db := chsqltest.OpenIsolatedChDB(t)
			ctx := seedZonedDeltaPrefix(t, db, typ)
			insertDeltaMatrixWindowSamples(t, db, `map('job', 'zoned')`, "zoned")

			oracle := runDeltaMatrixPrefixLevelQueryIn(ctx, t, db, deltaPrefixCanonicalizedMatrixWindow(
				deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange, "",
			), 0, false)["zoned"]
			got := runDeltaMatrixPrefixLevelQueryIn(ctx, t, db, deltaPrefixCanonicalizedMatrixWindow(
				deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
				"otel_metrics_sum_delta_prefix",
			), 0, true)["zoned"]
			const anchors = 2
			if len(oracle) != anchors || len(got) != anchors {
				t.Fatalf("anchors: DELTA-prefix read %v, unbounded scan %v; want %d each", got, oracle, anchors)
			}
			for anchor, want := range oracle {
				if got[anchor] != want {
					t.Errorf("anchor %d: DELTA-prefix level = %v, unbounded scan = %v", anchor, got[anchor], want)
				}
			}
		})
	}
}
