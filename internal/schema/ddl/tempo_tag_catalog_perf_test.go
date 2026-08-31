//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// TestTempoTagCatalog_MeasuredCost seeds a realistic-scale synthetic
// otel_traces table and measures, via system.query_log (the same
// verification method docs/operations.md's Loki catalog section used —
// see cerberus issue #2770's own PR), the real rows-read and wall-clock
// cost of the EXISTING live /search/tags scan
// (`SELECT DISTINCT arrayJoin(mapKeys(ResourceAttributes))`) against the
// NEW catalog read (`SELECT TagKey FROM tempo_tag_catalog WHERE
// Scope = 'resource' GROUP BY TagKey`) over the SAME trailing-1h window.
//
// The repo carries no Git-LFS sample TRACE data (only metrics parquet
// samples under test/perf/{nightly,smoke}/testdata — verified by grep
// before writing this test); this synthetic corpus is the substitute,
// sized and shaped to mirror the Loki catalog's own synthetic benchmark
// (docs/operations.md's "Loki label-cardinality catalog" section): a
// realistic resource-attribute key set (service.name,
// k8s.namespace.name, k8s.pod.name, deployment.environment.name, region)
// plus a wider span-attribute key set including a couple of
// higher-cardinality tails (http.route, db.statement) to exercise the
// topK cap under realistic skew.
//
// This is a measurement test, not a correctness gate: it logs the
// numbers (via t.Logf) rather than asserting a specific speedup ratio,
// which would be a flaky, environment-dependent assertion (CI runner
// noise) — see cerberus issue #2846's Loki PR for the same posture. It
// DOES assert the catalog read returns the identical key SET the live
// scan does (a correctness check riding along for free) and that the
// catalog reads dramatically fewer rows (a robust structural assertion
// that holds regardless of absolute timing).
func TestTempoTagCatalog_MeasuredCost(t *testing.T) {
	conn, db := startClickHouse(t)
	ctx := context.Background()

	cfg := ddl.Config{Database: db, TempoTagCatalogEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	const rowCount = 2_000_000
	seedSyntheticTraces(ctx, t, conn, db, rowCount)

	// The view's initial refresh runs as part of CREATE, before the seed
	// data exists, so force one explicitly rather than waiting on the
	// 5-minute schedule.
	if err := conn.Exec(ctx, fmt.Sprintf("SYSTEM REFRESH VIEW %s.tempo_tag_catalog_mv", db)); err != nil {
		t.Fatalf("SYSTEM REFRESH VIEW: %v", err)
	}
	refresh := waitForRefreshPast(ctx, t, conn, db, "tempo_tag_catalog_mv", "", 120*time.Second)
	if !refresh.succeeded() {
		t.Fatalf("post-seed refresh did not succeed: %+v", refresh)
	}

	liveSQL := fmt.Sprintf(
		"SELECT DISTINCT arrayJoin(mapKeys(ResourceAttributes)) AS k FROM %s.otel_traces WHERE Timestamp >= now() - toIntervalHour(1)",
		db,
	)
	catalogSQL := fmt.Sprintf(
		"SELECT TagKey FROM %s.tempo_tag_catalog WHERE Scope = 'resource' GROUP BY TagKey",
		db,
	)

	liveKeys, liveStats := runMeasured(ctx, t, conn, liveSQL)
	catalogKeys, catalogStats := runMeasured(ctx, t, conn, catalogSQL)

	if !stringSetEq(liveKeys, catalogKeys) {
		t.Errorf("catalog key set = %v, want the SAME set the live scan returned: %v", catalogKeys, liveKeys)
	}

	t.Logf("MEASURED (cerberus issue #2771, %d synthetic otel_traces rows, trailing 1h window):", rowCount)
	t.Logf("  live scan:    %d rows read (%d bytes), %v wall-clock", liveStats.rows, liveStats.bytes, liveStats.wall)
	t.Logf("  catalog read: %d rows read (%d bytes), %v wall-clock", catalogStats.rows, catalogStats.bytes, catalogStats.wall)
	if catalogStats.rows > 0 {
		t.Logf("  rows-read reduction: %.0fx", float64(liveStats.rows)/float64(catalogStats.rows))
	}
	if catalogStats.wall > 0 {
		t.Logf("  wall-clock reduction: %.1fx", float64(liveStats.wall)/float64(catalogStats.wall))
	}

	if catalogStats.rows >= liveStats.rows {
		t.Errorf("catalog read scanned %d rows, live scan scanned %d — the catalog read must scan dramatically fewer rows to be worth serving from",
			catalogStats.rows, liveStats.rows)
	}
}

// queryStats carries one query's system.query_log-reported cost.
type queryStats struct {
	rows  uint64
	bytes uint64
	wall  time.Duration
}

// runMeasured runs sqlStr tagged with a fresh query_id, then reads back
// read_rows/read_bytes/query_duration_ms for that query_id from
// system.query_log (after FLUSH LOGS, since the log table writes
// asynchronously) — the same verification method (system.query_log, not
// client-side row counting) the Loki catalog PR used: a client-side count
// would miss the rows ClickHouse actually scanned to produce the result
// (e.g. rows read before a DISTINCT/GROUP BY collapses them).
func runMeasured(ctx context.Context, t *testing.T, conn driver.Conn, sqlStr string) (map[string]struct{}, queryStats) {
	t.Helper()
	queryID := uuid.NewString()

	start := time.Now()
	rows, err := conn.Query(clickhouse.Context(ctx, clickhouse.WithQueryID(queryID)), sqlStr)
	if err != nil {
		t.Fatalf("query: %v: %s", err, sqlStr)
	}
	keys := map[string]struct{}{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys[k] = struct{}{}
	}
	_ = rows.Close()
	wall := time.Since(start)

	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
	}
	logRows, err := conn.Query(
		ctx,
		"SELECT read_rows, read_bytes FROM system.query_log WHERE query_id = ? AND type = 'QueryFinish' ORDER BY event_time DESC LIMIT 1",
		queryID,
	)
	if err != nil {
		t.Fatalf("query system.query_log: %v", err)
	}
	defer logRows.Close()
	var stats queryStats
	stats.wall = wall
	if logRows.Next() {
		if err := logRows.Scan(&stats.rows, &stats.bytes); err != nil {
			t.Fatalf("scan query_log row: %v", err)
		}
	} else {
		t.Fatalf("no system.query_log QueryFinish row found for query_id %s", queryID)
	}
	return keys, stats
}

// stringSetEq compares two string sets for equality.
func stringSetEq(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// syntheticResourceKeys / syntheticResourceCardinality mirror a realistic
// resource-attribute shape: 5 keys, each with a bounded value pool —
// service.name/k8s.namespace.name/deployment.environment.name/region are
// genuinely low-cardinality in production, k8s.pod.name is moderate
// (bounded by replica count, not request count).
var syntheticResourceKeys = []struct {
	name        string
	cardinality int
}{
	{"service.name", 12},
	{"k8s.namespace.name", 6},
	{"k8s.pod.name", 200},
	{"deployment.environment.name", 3},
	{"region", 4},
}

// syntheticSpanKeys mirrors a realistic span-attribute shape: most keys
// are low-cardinality (http.method, http.status_code, rpc.method,
// db.system), two are deliberately higher-cardinality
// (http.route/db.statement) to exercise the catalog's
// schema.TagCatalogTopValuesLimit cap under realistic skew rather than a
// trivially-satisfied one.
var syntheticSpanKeys = []struct {
	name        string
	cardinality int
}{
	{"http.method", 5},
	{"http.status_code", 8},
	{"http.route", 300},
	{"rpc.method", 20},
	{"db.system", 4},
	{"db.statement", 5000},
	{"net.peer.name", 50},
	{"messaging.system", 3},
	{"exception.type", 15},
	{"cache.hit", 2},
}

// seedSyntheticTraces bulk-inserts n otel_traces rows via PrepareBatch in
// batches of seedBatchSize, spread across the trailing hour, with the
// resource/span attribute shapes above. rng is seeded deterministically
// (not crypto/rand) so a failing run reproduces.
func seedSyntheticTraces(ctx context.Context, t *testing.T, conn driver.Conn, db string, n int) {
	t.Helper()
	const seedBatchSize = 50_000
	rng := rand.New(rand.NewSource(2771))
	now := time.Now().UTC()

	inserted := 0
	for inserted < n {
		batchN := seedBatchSize
		if remaining := n - inserted; remaining < batchN {
			batchN = remaining
		}
		batch, err := conn.PrepareBatch(ctx, fmt.Sprintf(
			"INSERT INTO %s.otel_traces (Timestamp, ResourceAttributes, SpanAttributes)", db,
		))
		if err != nil {
			t.Fatalf("prepare batch: %v", err)
		}
		for i := 0; i < batchN; i++ {
			ts := now.Add(-time.Duration(rng.Int63n(int64(time.Hour))))
			resAttrs := map[string]string{}
			for _, k := range syntheticResourceKeys {
				resAttrs[k.name] = fmt.Sprintf("%s-%d", k.name, rng.Intn(k.cardinality))
			}
			spanAttrs := map[string]string{}
			for _, k := range syntheticSpanKeys {
				spanAttrs[k.name] = fmt.Sprintf("%s-%d", k.name, rng.Intn(k.cardinality))
			}
			if err := batch.Append(ts, resAttrs, spanAttrs); err != nil {
				t.Fatalf("batch append: %v", err)
			}
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("batch send: %v", err)
		}
		inserted += batchN
	}
	t.Logf("seeded %d synthetic otel_traces rows", inserted)
}
