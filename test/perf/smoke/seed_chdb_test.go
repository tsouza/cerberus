//go:build chdb

package smoke

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers the "chdb" sql driver

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// detectedLevelLogsTestLines is the line count the chDB check seeds: a
// multiple of every period the seed cycles through (the 12 severities, the
// failed lines' 7, the spans modulus 11, and the 12 severities over the one
// line in 3 that carries a level attribute), so each value of each cycle
// lands on exactly lines/period rows.
const detectedLevelLogsTestLines = 11_088

// TestDetectedLevelLogsInsert_SeedsTheDocumentedShape runs the INSERT
// SeedDetectedLevelLogs executes against the otel_logs table ddl renders,
// on chDB, and reads back every property its doc comment promises and the
// merge_plan_filters sentinels depend on: the row count, the even spread over
// [start - lookback, end), the one stream, the failed-line and spans cycles in
// the logfmt body, every severity spelling in equal measure, and the level
// LogAttribute on one line in detectedLevelLogsAttributeLevelEvery.
func TestDetectedLevelLogsInsert_SeedsTheDocumentedShape(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS otel_logs"); err != nil {
		t.Fatalf("drop otel_logs: %v", err)
	}
	stmts, err := ddl.RenderAll(ddl.Config{}, []ddl.Signal{ddl.Logs})
	if err != nil {
		t.Fatalf("render the logs DDL: %v", err)
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("apply logs DDL: %v\n%s", err, stmt)
		}
	}

	start := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	insert, err := detectedLevelLogsInsert(detectedLevelLogsTestLines, start, end)
	if err != nil {
		t.Fatalf("detectedLevelLogsInsert: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert); err != nil {
		t.Fatalf("seed: %v\n%s", err, insert)
	}

	const lines = detectedLevelLogsTestLines
	from := start.Add(-detectedLevelLogsLookback)

	var (
		rows, beforeStart, lastLookback, foreign, failed, ok, withLevel uint64
		minNanos, maxNanos                                              int64
		spanValues, maxSpans                                            uint64
	)
	row := db.QueryRowContext(
		ctx, `
SELECT
    count(),
    countIf(Timestamp < fromUnixTimestamp64Nano(?)),
    countIf(Timestamp >= fromUnixTimestamp64Nano(?)),
    countIf(ServiceName != ? OR ResourceAttributes['service.name'] != ? OR ResourceAttributes['service_name'] != ?),
    countIf(startsWith(Body, 'status=500 msg=failed spans=')),
    countIf(startsWith(Body, 'status=200 msg=ok spans=')),
    countIf(mapContains(LogAttributes, 'level')),
    toUnixTimestamp64Nano(min(Timestamp)),
    toUnixTimestamp64Nano(max(Timestamp)),
    uniqExact(extract(Body, 'spans=([0-9]+)$')),
    max(toUInt64(extract(Body, 'spans=([0-9]+)$')))
FROM otel_logs`,
		start.UnixNano(), end.Add(-detectedLevelLogsLookback).UnixNano(),
		DetectedLevelLogsService, DetectedLevelLogsService, DetectedLevelLogsService,
	)
	if err := row.Scan(&rows, &beforeStart, &lastLookback, &foreign, &failed, &ok, &withLevel,
		&minNanos, &maxNanos, &spanValues, &maxSpans); err != nil {
		t.Fatalf("read the seeded shape: %v", err)
	}

	if rows != lines {
		t.Errorf("seeded %d rows, want %d", rows, lines)
	}
	if minNanos != from.UnixNano() {
		t.Errorf("first line at %v, want start - lookback = %v", time.Unix(0, minNanos).UTC(), from)
	}
	if maxNanos >= end.UnixNano() || maxNanos <= start.UnixNano() {
		t.Errorf("last line at %v, want inside (%v, %v)", time.Unix(0, maxNanos).UTC(), start, end)
	}
	// The lookback before start and the window's final lookback-long stretch
	// must hold the same number of lines, give or take the one line the
	// integer step leaves at a boundary: that is what lets the first anchor's
	// range selector see as many lines as the last one's.
	if diff := int64(beforeStart) - int64(lastLookback); diff < -1 || diff > 1 {
		t.Errorf("%d lines before start against %d in the window's last %v: the seed is not spread evenly",
			beforeStart, lastLookback, detectedLevelLogsLookback)
	}
	if foreign != 0 {
		t.Errorf("%d lines outside the %q stream", foreign, DetectedLevelLogsService)
	}
	if want := uint64(lines / detectedLevelLogsFailedEvery); failed != want {
		t.Errorf("%d status=500 lines, want one in %d = %d", failed, detectedLevelLogsFailedEvery, want)
	}
	if failed+ok != lines {
		t.Errorf("%d status=500 and %d status=200 lines, want every one of %d lines to carry one", failed, ok, lines)
	}
	if want := uint64(lines / detectedLevelLogsAttributeLevelEvery); withLevel != want {
		t.Errorf("%d lines carry a level LogAttribute, want one in %d = %d",
			withLevel, detectedLevelLogsAttributeLevelEvery, want)
	}
	if spanValues != detectedLevelLogsSpanModulus || maxSpans != detectedLevelLogsSpanModulus-1 {
		t.Errorf("spans takes %d values up to %d, want %d values up to %d",
			spanValues, maxSpans, detectedLevelLogsSpanModulus, detectedLevelLogsSpanModulus-1)
	}

	assertEvenSeverities(ctx, t, db, "SeverityText", severityTextCountsQuery, lines)
	assertEvenSeverities(ctx, t, db, "the level LogAttribute", levelAttributeCountsQuery,
		lines/detectedLevelLogsAttributeLevelEvery)
}

// severityTextCountsQuery counts the seeded lines per SeverityText.
const severityTextCountsQuery = `SELECT SeverityText, count() FROM otel_logs GROUP BY SeverityText`

// levelAttributeCountsQuery counts the seeded lines carrying a level
// LogAttribute per value of it.
const levelAttributeCountsQuery = `SELECT LogAttributes['level'] AS level, count() FROM otel_logs
WHERE mapContains(LogAttributes, 'level') GROUP BY level`

// assertEvenSeverities asserts the (value, count) rows query returns for what
// name exactly the detectedLevelLogsSeverities values, each on the same share
// of total rows.
func assertEvenSeverities(ctx context.Context, t *testing.T, db *sql.DB, what, query string, total int) {
	t.Helper()
	rs, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("count %s: %v", what, err)
	}
	defer func() { _ = rs.Close() }()
	got := map[string]uint64{}
	for rs.Next() {
		var (
			v string
			n uint64
		)
		if err := rs.Scan(&v, &n); err != nil {
			t.Fatalf("scan %s: %v", what, err)
		}
		got[v] = n
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	want := uint64(total / len(detectedLevelLogsSeverities))
	for _, severity := range detectedLevelLogsSeverities {
		if got[severity] != want {
			t.Errorf("%s = %q on %d rows, want %d", what, severity, got[severity], want)
		}
	}
	for v := range got {
		if !slices.Contains(detectedLevelLogsSeverities, v) {
			t.Errorf("%s takes %q, which the seed does not cycle through", what, v)
		}
	}
}
