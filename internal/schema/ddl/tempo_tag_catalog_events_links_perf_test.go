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

// TestTempoTagCatalog_EventsLinks_MeasuredCost is the empirical input for
// cerberus issue #2850 (extend the tag catalog to event/link scopes). It
// mirrors TestTempoTagCatalog_MeasuredCost's methodology exactly (same
// synthetic-corpus-and-system.query_log discipline #2771 used) but seeds
// the SAME table with Events/Links populated too, so the resource/span
// arms (the shipped baseline) and the candidate event/link arms are
// measured on the SAME 2,000,000-row corpus over the SAME trailing-1h
// window — an apples-to-apples comparison, not two separate datasets.
//
// This is a measurement test, not a correctness gate (same posture as its
// sibling): it logs numbers via t.Logf rather than asserting a specific
// ratio. It DOES assert each arm's DISTINCT-key read returns the exact
// synthetic key set seeded for it — a correctness check on the query
// shape itself, since an event/link arm that scanned the wrong column
// would silently report a misleadingly-cheap (or expensive) cost.
func TestTempoTagCatalog_EventsLinks_MeasuredCost(t *testing.T) {
	conn, db := startClickHouse(t)
	ctx := context.Background()

	cfg := ddl.Config{Database: db, TempoTagCatalogEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	const rowCount = 2_000_000
	seedSyntheticTracesWithEventsLinks(ctx, t, conn, db, rowCount)

	// --- correctness sanity: the arm SQL scans the column it claims to ---
	assertDistinctKeys(ctx, t, conn, db,
		fmt.Sprintf("SELECT DISTINCT k FROM %s.otel_traces ARRAY JOIN mapKeys(ResourceAttributes) AS k WHERE Timestamp >= now() - toIntervalHour(1)", db),
		keyNames(syntheticResourceKeys))
	assertDistinctKeys(ctx, t, conn, db,
		fmt.Sprintf("SELECT DISTINCT k FROM %s.otel_traces ARRAY JOIN mapKeys(SpanAttributes) AS k WHERE Timestamp >= now() - toIntervalHour(1)", db),
		keyNames(syntheticSpanKeys))
	assertDistinctKeys(ctx, t, conn, db,
		fmt.Sprintf("SELECT DISTINCT k FROM %s.otel_traces ARRAY JOIN arrayFlatten(arrayMap(m -> mapKeys(m), Events.Attributes)) AS k WHERE Timestamp >= now() - toIntervalHour(1)", db),
		keyNames(syntheticEventKeys))
	assertDistinctKeys(ctx, t, conn, db,
		fmt.Sprintf("SELECT DISTINCT k FROM %s.otel_traces ARRAY JOIN arrayFlatten(arrayMap(m -> mapKeys(m), Links.Attributes)) AS k WHERE Timestamp >= now() - toIntervalHour(1)", db),
		keyNames(syntheticLinkKeys))

	// --- cost measurement: shipped baseline (resource UNION span) -------
	//
	// Each body is wrapped in `SELECT count() FROM (...)`: the inner SELECT
	// projects an AggregateFunction(topK(50), String) state column exactly
	// as the real catalog view does, but clickhouse-go/v2 cannot decode
	// that wire type client-side (it has no read-side consumer here — the
	// real read side is topKMerge+arrayJoin, tested elsewhere). count()
	// forces the server to materialise the aggregate state without ever
	// shipping it over the wire, so the SCAN cost being measured is
	// identical to the real view's; only the client-decode step differs.
	baselineSQL := fmt.Sprintf(`
SELECT count() FROM (
SELECT Scope, TagKey, topKState(50)(TagValue)
FROM (
  SELECT 'resource' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN mapKeys(ResourceAttributes) AS k, mapValues(ResourceAttributes) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
  UNION ALL
  SELECT 'span' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN mapKeys(SpanAttributes) AS k, mapValues(SpanAttributes) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
)
GROUP BY Scope, TagKey
)`, db)

	eventArmSQL := fmt.Sprintf(`
SELECT count() FROM (
SELECT 'event' AS Scope, k AS TagKey, topKState(50)(v)
FROM %[1]s.otel_traces
ARRAY JOIN
  arrayFlatten(arrayMap(m -> mapKeys(m), Events.Attributes)) AS k,
  arrayFlatten(arrayMap(m -> mapValues(m), Events.Attributes)) AS v
WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
GROUP BY Scope, TagKey
)`, db)

	linkArmSQL := fmt.Sprintf(`
SELECT count() FROM (
SELECT 'link' AS Scope, k AS TagKey, topKState(50)(v)
FROM %[1]s.otel_traces
ARRAY JOIN
  arrayFlatten(arrayMap(m -> mapKeys(m), Links.Attributes)) AS k,
  arrayFlatten(arrayMap(m -> mapValues(m), Links.Attributes)) AS v
WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
GROUP BY Scope, TagKey
)`, db)

	widenedSQL := fmt.Sprintf(`
SELECT count() FROM (
SELECT Scope, TagKey, topKState(50)(TagValue)
FROM (
  SELECT 'resource' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN mapKeys(ResourceAttributes) AS k, mapValues(ResourceAttributes) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
  UNION ALL
  SELECT 'span' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN mapKeys(SpanAttributes) AS k, mapValues(SpanAttributes) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
  UNION ALL
  SELECT 'event' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN
    arrayFlatten(arrayMap(m -> mapKeys(m), Events.Attributes)) AS k,
    arrayFlatten(arrayMap(m -> mapValues(m), Events.Attributes)) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
  UNION ALL
  SELECT 'link' AS Scope, k AS TagKey, v AS TagValue
  FROM %[1]s.otel_traces
  ARRAY JOIN
    arrayFlatten(arrayMap(m -> mapKeys(m), Links.Attributes)) AS k,
    arrayFlatten(arrayMap(m -> mapValues(m), Links.Attributes)) AS v
  WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
)
GROUP BY Scope, TagKey
)`, db)

	baseline := runMeasuredStats(ctx, t, conn, baselineSQL)
	eventOnly := runMeasuredStats(ctx, t, conn, eventArmSQL)
	linkOnly := runMeasuredStats(ctx, t, conn, linkArmSQL)
	widened := runMeasuredStats(ctx, t, conn, widenedSQL)

	t.Logf("MEASURED (cerberus issue #2850, %d synthetic otel_traces rows, trailing 1h window):", rowCount)
	t.Logf("  shipped baseline (resource UNION span):        %8d rows read (%10d bytes), %v wall-clock", baseline.rows, baseline.bytes, baseline.wall)
	t.Logf("  event arm alone (nested Array(Map) fan-out):   %8d rows read (%10d bytes), %v wall-clock", eventOnly.rows, eventOnly.bytes, eventOnly.wall)
	t.Logf("  link arm alone (nested Array(Map) fan-out):    %8d rows read (%10d bytes), %v wall-clock", linkOnly.rows, linkOnly.bytes, linkOnly.wall)
	t.Logf("  widened (resource+span+event+link, all 4 arms):%8d rows read (%10d bytes), %v wall-clock", widened.rows, widened.bytes, widened.wall)
	if baseline.wall > 0 {
		t.Logf("  widened / baseline wall-clock ratio: %.2fx", float64(widened.wall)/float64(baseline.wall))
	}
	if baseline.rows > 0 {
		t.Logf("  widened / baseline rows-read ratio:  %.2fx", float64(widened.rows)/float64(baseline.rows))
	}
	if eventOnly.wall > 0 && baseline.wall > 0 {
		t.Logf("  event-arm-alone / baseline wall-clock ratio: %.2fx", float64(eventOnly.wall)/float64(baseline.wall))
	}
	if linkOnly.wall > 0 && baseline.wall > 0 {
		t.Logf("  link-arm-alone / baseline wall-clock ratio:  %.2fx", float64(linkOnly.wall)/float64(baseline.wall))
	}
}

// syntheticEventKeys mirrors a realistic OTel exception-event attribute
// shape (the dominant real-world span-event use case): the four
// `exception.*` semantic-convention keys, with exception.stacktrace
// deliberately high-cardinality (mirrors syntheticSpanKeys' db.statement
// tail) to exercise the topK cap the same way.
var syntheticEventKeys = []struct {
	name        string
	cardinality int
}{
	{"exception.type", 15},
	{"exception.message", 200},
	{"exception.stacktrace", 2000},
	{"exception.escaped", 2},
}

// syntheticLinkKeys mirrors span-link attributes in an async-messaging
// topology (the dominant real-world span-link use case: a consumer span
// linking back to the producer span that published the message).
var syntheticLinkKeys = []struct {
	name        string
	cardinality int
}{
	{"opentracing.ref_type", 3},
	{"messaging.batch.id", 500},
}

// eventsPerSpan / linksPerSpan model realistic OTel event/link
// PREVALENCE, not just per-populated-row shape: exception events fire on
// error paths (typical service error rates run low single digits to low
// tens of percent; 10% here is a deliberately generous/pessimistic
// assumption so the measured cost is not flattered by an unrealistically
// sparse corpus), occasionally two on a span that raises and re-raises.
// Links are rarer still — only async-messaging consumer spans carry one,
// and even in a messaging-heavy fleet that is a minority of spans.
func eventsPerSpan(rng *rand.Rand) int {
	switch {
	case rng.Float64() < 0.02: // 2%: two events (raise + re-raise)
		return 2
	case rng.Float64() < 0.10: // additional 8%: one event -> 10% of spans carry >=1
		return 1
	default:
		return 0
	}
}

func linksPerSpan(rng *rand.Rand) int {
	if rng.Float64() < 0.03 { // 3%: one link (consumer -> producer)
		return 1
	}
	return 0
}

// seedSyntheticTracesWithEventsLinks extends seedSyntheticTraces
// (tempo_tag_catalog_perf_test.go) with populated Events/Links Nested
// columns on the SAME rows, so the resource/span baseline and the
// event/link candidate arms are measured against the SAME corpus rather
// than two differently-shaped tables.
func seedSyntheticTracesWithEventsLinks(ctx context.Context, t *testing.T, conn driver.Conn, db string, n int) {
	t.Helper()
	const seedBatchSize = 50_000
	rng := rand.New(rand.NewSource(2850))
	now := time.Now().UTC()

	inserted := 0
	for inserted < n {
		batchN := seedBatchSize
		if remaining := n - inserted; remaining < batchN {
			batchN = remaining
		}
		batch, err := conn.PrepareBatch(ctx, fmt.Sprintf(
			"INSERT INTO %s.otel_traces (Timestamp, ResourceAttributes, SpanAttributes, "+
				"`Events.Timestamp`, `Events.Name`, `Events.Attributes`, "+
				"`Links.TraceId`, `Links.SpanId`, `Links.TraceState`, `Links.Attributes`)", db,
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

			nEvents := eventsPerSpan(rng)
			evTs := make([]time.Time, nEvents)
			evNames := make([]string, nEvents)
			evAttrs := make([]map[string]string, nEvents)
			for e := 0; e < nEvents; e++ {
				evTs[e] = ts
				evNames[e] = "exception"
				m := map[string]string{}
				for _, k := range syntheticEventKeys {
					m[k.name] = fmt.Sprintf("%s-%d", k.name, rng.Intn(k.cardinality))
				}
				evAttrs[e] = m
			}

			nLinks := linksPerSpan(rng)
			lnTraceID := make([]string, nLinks)
			lnSpanID := make([]string, nLinks)
			lnTraceState := make([]string, nLinks)
			lnAttrs := make([]map[string]string, nLinks)
			for l := 0; l < nLinks; l++ {
				lnTraceID[l] = fmt.Sprintf("%032x", rng.Int63())
				lnSpanID[l] = fmt.Sprintf("%016x", rng.Int63())
				lnTraceState[l] = ""
				m := map[string]string{}
				for _, k := range syntheticLinkKeys {
					m[k.name] = fmt.Sprintf("%s-%d", k.name, rng.Intn(k.cardinality))
				}
				lnAttrs[l] = m
			}

			if err := batch.Append(
				ts, resAttrs, spanAttrs,
				evTs, evNames, evAttrs,
				lnTraceID, lnSpanID, lnTraceState, lnAttrs,
			); err != nil {
				t.Fatalf("batch append: %v", err)
			}
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("batch send: %v", err)
		}
		inserted += batchN
	}
	t.Logf("seeded %d synthetic otel_traces rows (with Events/Links)", inserted)
}

// runMeasuredStats runs sqlStr tagged with a fresh query_id, drains the
// result without decoding it (these queries project topKState aggregate
// states, not plain strings — cost is what's being measured, not the
// value), and reads back read_rows/read_bytes/query_duration_ms from
// system.query_log — the same verification method runMeasured uses.
func runMeasuredStats(ctx context.Context, t *testing.T, conn driver.Conn, sqlStr string) queryStats {
	t.Helper()
	queryID := uuid.NewString()

	start := time.Now()
	rows, err := conn.Query(clickhouse.Context(ctx, clickhouse.WithQueryID(queryID)), sqlStr)
	if err != nil {
		t.Fatalf("query: %v: %s", err, sqlStr)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
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
	return stats
}

// assertDistinctKeys runs sqlStr (expected to project one String column
// `k`) and fails the test if the resulting set differs from want.
func assertDistinctKeys(ctx context.Context, t *testing.T, conn driver.Conn, db, sqlStr string, want []string) {
	t.Helper()
	rows, err := conn.Query(ctx, sqlStr)
	if err != nil {
		t.Fatalf("query: %v: %s", err, sqlStr)
	}
	defer rows.Close()
	got := map[string]struct{}{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[k] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("distinct-key SQL %q returned %d keys, want %d (%v)", sqlStr, len(got), len(want), want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Fatalf("distinct-key SQL %q missing expected key %q; got %v", sqlStr, w, got)
		}
	}
}

// keyNames extracts the `name` field of a syntheticResourceKeys /
// syntheticSpanKeys / syntheticEventKeys / syntheticLinkKeys-shaped slice.
func keyNames(keys []struct {
	name        string
	cardinality int
},
) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.name
	}
	return out
}
