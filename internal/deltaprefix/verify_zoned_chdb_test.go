//go:build chdb

package deltaprefix

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// The DELTA-prefix view buckets the sum table's TimeUnix by toStartOfDay,
// which runs in the timezone the column type names, else in the session's
// (the server's). These tests cut a deployment over at verifyZonedBefore with
// DELTA history straddling UTC midnight and each zone's midnight, backfill it,
// and require Verify to pass on the complete backfill and to fail once one
// pre-cutover row is missing from the aggregate table — for an unzoned column
// on a UTC server, an unzoned column on a server running in Kolkata, and
// columns zoned west (São Paulo) and east (Kolkata) of UTC.

// verifyZonedBefore is the view's creation instant: 2026-01-15 06:00 UTC,
// 03:00 in São Paulo and 11:30 in Kolkata, all on their Jan 15.
var verifyZonedBefore = time.Date(2026, 1, 15, 6, 0, 0, 0, time.UTC)

// verifyZonedHistory is the DELTA history every scenario seeds before the
// view exists. Relative to the cutover day each zone splits at — Jan 15
// 00:00 UTC, 03:00 UTC (São Paulo) and Jan 14 18:30 UTC (Kolkata) — it puts
// rows on both sides of every one of those midnights. The values are
// distinct powers of two, so a row counted on one side only leaves a
// difference no other mistake produces.
var verifyZonedHistory = []struct {
	at    time.Time
	value float64
}{
	{time.Date(2026, 1, 10, 5, 0, 0, 0, time.UTC), 1},
	{time.Date(2026, 1, 14, 18, 0, 0, 0, time.UTC), 2},  // Kolkata Jan 14 23:30
	{time.Date(2026, 1, 14, 20, 0, 0, 0, time.UTC), 4},  // Kolkata Jan 15 01:30
	{time.Date(2026, 1, 14, 23, 50, 0, 0, time.UTC), 8}, // São Paulo Jan 14 20:50
	{time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC), 16},  // São Paulo Jan 14 22:00
	{time.Date(2026, 1, 15, 3, 30, 0, 0, time.UTC), 32}, // São Paulo Jan 15 00:30
}

// verifyZonedLive is the DELTA traffic the view captures after the cutover.
var verifyZonedLive = []struct {
	at    time.Time
	value float64
}{
	{time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC), 64},
	{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), 128},
}

// verifyZonedGap is the pre-cutover row the failing half writes past the
// view, so the aggregate table never sees it.
var verifyZonedGap = time.Date(2026, 1, 12, 9, 0, 0, 0, time.UTC)

// verifyZonedMetric names the one metric every row carries.
const verifyZonedMetric = "reqs_total"

// verifyZonedScenario is one sum-table TimeUnix type, the session timezone
// the server runs in ("" for its default, UTC), and the cutover day Verify
// must report: the start of verifyZonedBefore's day where the view splits.
type verifyZonedScenario struct {
	name, typ, sessionTimezone string
	cutover                    time.Time
}

var verifyZonedScenarios = []verifyZonedScenario{
	{"unzoned on a UTC server", "DateTime64(9)", "", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)},
	{"unzoned on a Kolkata server", "DateTime64(9)", "Asia/Kolkata", time.Date(2026, 1, 14, 18, 30, 0, 0, time.UTC)},
	{"zoned Sao Paulo", "DateTime64(9, 'America/Sao_Paulo')", "", time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC)},
	{"zoned Kolkata", "DateTime64(9, 'Asia/Kolkata')", "", time.Date(2026, 1, 14, 18, 30, 0, 0, time.UTC)},
}

func TestVerify_ZonedSumTimestamp_ChDB(t *testing.T) {
	for _, sc := range verifyZonedScenarios {
		t.Run(sc.name, func(t *testing.T) {
			db := chsqltest.OpenIsolatedChDB(t)
			if sc.sessionTimezone != "" {
				mustExecChDB(t, db, fmt.Sprintf("SET session_timezone = '%s'", sc.sessionTimezone))
				// chDB runs one session per process: leave it as found.
				t.Cleanup(func() { mustExecChDB(t, db, "SET session_timezone = ''") })
			}
			c := verifyZonedColumns(t, db)
			conn := &chdbConn{db: db}
			ctx := context.Background()

			seedVerifyZoned(t, db, sc.typ)
			if _, err := Backfill(ctx, conn, c, verifyZonedBefore, 0); err != nil {
				t.Fatalf("Backfill: %v", err)
			}

			rep, err := Verify(ctx, conn, c, verifyZonedBefore, 0, 0)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !rep.CutoverDay.Equal(sc.cutover) {
				t.Errorf("CutoverDay = %v; want %v", rep.CutoverDay, sc.cutover)
			}
			if !rep.Pass() || rep.BaseTotals[verifyZonedMetric] == 0 {
				t.Fatalf("Verify on a complete backfill: mismatches %+v (aggregate %v, base %v)",
					rep.Mismatches, rep.AggregateTotals, rep.BaseTotals)
			}

			// A pre-cutover row the view never saw and the backfill did not
			// cover: the aggregate table is now missing it.
			mustExecChDB(t, db, "DROP TABLE otel_metrics_sum_delta_prefix_mv")
			insertVerifyZonedRow(t, db, verifyZonedGap, 256)
			rep, err = Verify(ctx, conn, c, verifyZonedBefore, 0, 0)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(rep.Mismatches) != 1 || rep.Mismatches[0].Diff() != -256 {
				t.Errorf("Verify with a missing row: mismatches %+v; want the one row of 256 missing", rep.Mismatches)
			}
		})
	}
}

// verifyZonedColumns resolves Columns against the isolated database.
func verifyZonedColumns(t *testing.T, db *sql.DB) Columns {
	t.Helper()
	var database string
	if err := db.QueryRow("SELECT currentDatabase()").Scan(&database); err != nil {
		t.Fatalf("currentDatabase: %v", err)
	}
	m := schema.DefaultOTelMetrics()
	m.DeltaPrefixTable = "otel_metrics_sum_delta_prefix"
	m.DeltaPrefixBucketColumn = "BucketStart"
	m.DeltaPrefixSumColumn = "PartialSum"
	return FromSchema(database, m)
}

// seedVerifyZoned creates the sum table with TimeUnix typed typ and the
// DELTA-prefix table, writes the pre-cutover history, then creates the view
// (internal/schema/ddl's renderDeltaPrefixView expression) and writes the
// live traffic through it.
func seedVerifyZoned(t *testing.T, db *sql.DB, typ string) {
	t.Helper()
	mustExecChDB(t, db, fmt.Sprintf(`CREATE TABLE otel_metrics_sum (
		MetricName String,
		Attributes Map(String, String),
		ResourceAttributes Map(String, String),
		ServiceName String,
		AggregationTemporality Int32,
		TimeUnix %s,
		Value Float64
	) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)`, typ))
	mustExecChDB(t, db, `CREATE TABLE otel_metrics_sum_delta_prefix (
		MetricName String,
		Attributes Map(String, String),
		ResourceAttributes Map(String, String),
		ServiceName String,
		BucketStart DateTime64(9),
		PartialSum SimpleAggregateFunction(sum, Float64)
	) ENGINE = AggregatingMergeTree ORDER BY (MetricName, BucketStart)`)
	for _, h := range verifyZonedHistory {
		insertVerifyZonedRow(t, db, h.at, h.value)
	}
	mustExecChDB(t, db, `CREATE MATERIALIZED VIEW otel_metrics_sum_delta_prefix_mv TO otel_metrics_sum_delta_prefix AS
		SELECT MetricName, Attributes, ResourceAttributes, ServiceName,
			toStartOfDay(TimeUnix) AS BucketStart, sum(Value) AS PartialSum
		FROM otel_metrics_sum WHERE AggregationTemporality = 1
		GROUP BY MetricName, Attributes, ResourceAttributes, ServiceName, BucketStart`)
	for _, h := range verifyZonedLive {
		insertVerifyZonedRow(t, db, h.at, h.value)
	}
}

func insertVerifyZonedRow(t *testing.T, db *sql.DB, at time.Time, value float64) {
	t.Helper()
	mustExecChDB(t, db, fmt.Sprintf(
		`INSERT INTO otel_metrics_sum VALUES ('%s', map('job', 'zoned'), map(), 'svc', 1, fromUnixTimestamp64Nano(toInt64(%d)), %v)`,
		verifyZonedMetric, at.UnixNano(), value,
	))
}

func mustExecChDB(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%v\n%s", err, stmt)
	}
}

// chdbConn adapts a database/sql chDB handle to Conn.
type chdbConn struct{ db *sql.DB }

func (c *chdbConn) Exec(ctx context.Context, query string, args ...any) error {
	_, err := c.db.ExecContext(ctx, query, args...)
	return err
}

func (c *chdbConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	r, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &chdbRows{rows: r}, nil
}

// chdbRows implements the rows surface this package reads (Next, Scan, Err,
// Close) over *sql.Rows; driver.Rows is embedded for the rest.
type chdbRows struct {
	driver.Rows
	rows *sql.Rows
}

func (r *chdbRows) Next() bool          { return r.rows.Next() }
func (r *chdbRows) Scan(d ...any) error { return r.rows.Scan(d...) }
func (r *chdbRows) Err() error          { return testsql.TolerantRowsErr(r.rows.Err()) }
func (r *chdbRows) Close() error        { return r.rows.Close() }
