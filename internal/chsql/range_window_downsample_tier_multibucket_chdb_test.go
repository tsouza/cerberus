//go:build chdb

// chDB-backed correctness proof for the multi-bucket extension to the
// downsampled long-range tier (cerberus issue #2857): a range spanning
// several tier buckets now routes to the tier, merging every covering
// bucket's state via timeSeriesLastTwoSamplesMerge and re-filtering the
// merged trailing pair to the exact `(anchor-range, anchor]` window before
// computing the per-Func value (emitRangeWindowDownsampleTier /
// downsampleTierExactWindowFilterFrag, internal/chsql).
//
// Three properties this file proves empirically, each the risky edge case
// #2857's own issue body calls out:
//
//  1. Cross-bucket merge correctness: the trailing pair a query actually
//     needs can span TWO DIFFERENT tier buckets (not just live inside the
//     newest one), and a bucket with no relevant samples correctly
//     contributes nothing — see TestDownsampleTier_ChdbCrossBucketMerge.
//  2. A single tier bucket correctly feeds MULTIPLE overlapping anchors on
//     a sliding (Step < Range) query_range grid, each anchor computing its
//     OWN distinct trailing pair — see
//     TestDownsampleTier_ChdbSlidingWindowMultiAnchor.
//  3. The post-merge exact-window re-filter
//     (downsampleTierExactWindowFilterFrag) is not a no-op belt: a tier row
//     whose retained sample timestamp sits outside its own BucketEnd's
//     canonical interval (the shape a backfill bug or a manual data
//     correction could produce) is correctly excluded from the merged
//     answer rather than silently corrupting it — see
//     TestDownsampleTier_ChdbCorruptedBucketPostMergeFilter.
//
// This file shares downsampleTierChdbSetup / insertDownsampleTierSamples /
// downsampleTierHostSample / downsampleTierLowererTable / hostFromJSON with
// range_window_downsample_tier_chdb_test.go (same package, same DDL, same
// seed/query plumbing) — only the grid-shaped query runner is new
// (runDownsampleTierPromQLGrid), since the single-bucket file's own
// runDownsampleTierPromQL hard-codes a single-anchor grid and discards the
// per-row timestamp, both of which a multi-anchor test needs.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// downsampleTierGridResult is one output row's (timestamp, value) pair,
// keyed by host — the shape a multi-anchor grid query needs that the
// single-bucket file's host-only map cannot represent (two anchors for the
// SAME host are two different map entries here).
type downsampleTierGridResult struct {
	ts    time.Time
	value float64
}

// runDownsampleTierPromQLGrid mirrors runDownsampleTierPromQL
// (range_window_downsample_tier_chdb_test.go) but takes an arbitrary
// [start, end] / step grid instead of a single fixed anchor, and keeps each
// row's own timestamp instead of discarding it — required to tell two
// different anchors' answers for the SAME host apart.
func runDownsampleTierPromQLGrid(
	t *testing.T, db *sql.DB, query string, start, end time.Time, step time.Duration, tierRouted bool,
) map[string]downsampleTierGridResult {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	var lowerers promql.RangeLowerers
	if tierRouted {
		lowerers = downsampleTierLowererTable()
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, end, step, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q (tierRouted=%v): %v", query, tierRouted, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit %q (tierRouted=%v): %v", query, tierRouted, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS host_json, `TimeUnix`, `Value` FROM (" + sqlText + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query %q (tierRouted=%v): %v\nSQL: %s", query, tierRouted, err, wrapped)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]downsampleTierGridResult)
	for rows.Next() {
		var hostJSON string
		var ts time.Time
		var value float64
		if err := rows.Scan(&hostJSON, &ts, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		host := hostFromJSON(t, hostJSON)
		key := fmt.Sprintf("%s@%s", host, ts.UTC().Format(time.RFC3339))
		out[key] = downsampleTierGridResult{ts: ts, value: value}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// runDownsampleTierPromQLSingleAnchor mirrors runDownsampleTierPromQL
// (range_window_downsample_tier_chdb_test.go) — a single fixed anchor,
// host-keyed Value map, no per-row timestamp — but takes an arbitrary
// anchor instead of that file's hardcoded downsampleTierBucketAnchor
// global. Used wherever a test needs dual-emit parity against the fan-out
// oracle: the fan-out emitter's own outer SELECT carries no timestamp
// column at all (only Attributes + Value — matching the original file's
// own host-only wrapper), so a single-anchor comparison is the only shape
// both sides can answer.
func runDownsampleTierPromQLSingleAnchor(t *testing.T, db *sql.DB, query string, anchor time.Time, tierRouted bool) map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	var lowerers promql.RangeLowerers
	if tierRouted {
		lowerers = downsampleTierLowererTable()
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		anchor, anchor, schema.DownsampleTierBucket, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q (tierRouted=%v): %v", query, tierRouted, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit %q (tierRouted=%v): %v", query, tierRouted, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS host_json, `Value` FROM (" + sqlText + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query %q (tierRouted=%v): %v\nSQL: %s", query, tierRouted, err, wrapped)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]float64)
	for rows.Next() {
		var hostJSON string
		var value float64
		if err := rows.Scan(&hostJSON, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[hostFromJSON(t, hostJSON)] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestDownsampleTier_ChdbCrossBucketMerge proves the core #2857 mechanism:
// a 15m range (3x the 5m bucket) merges THREE tier buckets via
// timeSeriesLastTwoSamplesMerge, and the trailing pair the query actually
// needs can span two DIFFERENT buckets rather than living entirely inside
// the newest one.
func TestDownsampleTier_ChdbCrossBucketMerge(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)

	anchor := time.Date(2024, 6, 1, 0, 15, 0, 0, time.UTC) // BucketEnd of the 3rd bucket
	seed := []downsampleTierHostSample{
		// "spanning": trailing pair (100@00:04:30, 200@00:07:00) spans the
		// FIRST and SECOND bucket — the third bucket ([00:10,00:15]) holds no
		// sample for this host at all, proving an empty covering bucket
		// contributes nothing rather than erroring or defaulting to zero.
		{"spanning", time.Date(2024, 6, 1, 0, 4, 30, 0, time.UTC), 100, 2},
		{"spanning", time.Date(2024, 6, 1, 0, 7, 0, 0, time.UTC), 200, 2},
		// "newest_only": every sample lives in the LAST bucket alone — the
		// degenerate case where cross-bucket merging changes nothing,
		// checked here for contrast against "spanning".
		{"newest_only", time.Date(2024, 6, 1, 0, 11, 0, 0, time.UTC), 5, 2},
		{"newest_only", time.Date(2024, 6, 1, 0, 12, 0, 0, time.UTC), 9, 2},
		// "discarded_middle": THREE samples across all three buckets — the
		// merge must keep only the trailing TWO (400@00:09, 500@00:13),
		// discarding the earlier 300@00:02 even though it lives in a
		// DIFFERENT (the oldest) bucket from the pair it's discarded in
		// favour of.
		{"discarded_middle", time.Date(2024, 6, 1, 0, 2, 0, 0, time.UTC), 300, 2},
		{"discarded_middle", time.Date(2024, 6, 1, 0, 9, 0, 0, time.UTC), 400, 2},
		{"discarded_middle", time.Date(2024, 6, 1, 0, 13, 0, 0, time.UTC), 500, 2},
	}
	insertDownsampleTierSamples(t, db, seed)

	const ideltaQuery = `idelta(cpu_seconds_total[15m])`
	const irateQuery = `irate(cpu_seconds_total[15m])`

	ideltaTier := runDownsampleTierPromQLSingleAnchor(t, db, ideltaQuery, anchor, true)
	ideltaFanout := runDownsampleTierPromQLSingleAnchor(t, db, ideltaQuery, anchor, false)
	irateTier := runDownsampleTierPromQLSingleAnchor(t, db, irateQuery, anchor, true)
	irateFanout := runDownsampleTierPromQLSingleAnchor(t, db, irateQuery, anchor, false)

	wantIdelta := map[string]float64{
		"spanning":         100, // 200 - 100
		"newest_only":      4,   // 9 - 5
		"discarded_middle": 100, // 500 - 400, NOT 500-300
	}
	for host, want := range wantIdelta {
		got, ok := ideltaTier[host]
		if !ok {
			t.Errorf("idelta host=%s: expected present, got absent", host)
			continue
		}
		if got != want {
			t.Errorf("idelta host=%s: want %v, got %v", host, want, got)
		}
		fv, ok := ideltaFanout[host]
		if !ok {
			t.Errorf("idelta host=%s: absent from fan-out oracle (test bug?)", host)
			continue
		}
		if fv != got {
			t.Errorf("idelta host=%s: tier=%v fanout(oracle)=%v mismatch", host, got, fv)
		}
	}

	wantIrate := map[string]float64{
		"spanning":         100.0 / 150, // interval 00:04:30 -> 00:07:00 = 150s
		"newest_only":      4.0 / 60,    // interval 00:11:00 -> 00:12:00 = 60s
		"discarded_middle": 100.0 / 240, // interval 00:09:00 -> 00:13:00 = 240s
	}
	for host, want := range wantIrate {
		got, ok := irateTier[host]
		if !ok {
			t.Errorf("irate host=%s: expected present, got absent", host)
			continue
		}
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("irate host=%s: want %v, got %v", host, want, got)
		}
		fv, ok := irateFanout[host]
		if !ok {
			t.Errorf("irate host=%s: absent from fan-out oracle (test bug?)", host)
			continue
		}
		if diff := fv - got; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("irate host=%s: tier=%v fanout(oracle)=%v mismatch", host, got, fv)
		}
	}
}

// TestDownsampleTier_ChdbSlidingWindowMultiAnchor proves a SINGLE tier
// bucket correctly feeds MULTIPLE overlapping query_range anchors (Step=5m
// < Range=15m — three consecutive buckets slide across three anchors, each
// anchor computing its own distinct trailing pair), not just the single
// fixed anchor every other downsample-tier test exercises.
func TestDownsampleTier_ChdbSlidingWindowMultiAnchor(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)

	// Powers of two so each anchor's idelta is unambiguous and a
	// bucket-shifted-by-one bug would produce an easily distinguishable
	// wrong answer (2, 4, or 8) rather than accidentally matching.
	seed := []downsampleTierHostSample{
		{"sliding", time.Date(2024, 6, 1, 0, 4, 0, 0, time.UTC), 1, 2},
		{"sliding", time.Date(2024, 6, 1, 0, 9, 0, 0, time.UTC), 2, 2},
		{"sliding", time.Date(2024, 6, 1, 0, 14, 0, 0, time.UTC), 4, 2},
		{"sliding", time.Date(2024, 6, 1, 0, 19, 0, 0, time.UTC), 8, 2},
		{"sliding", time.Date(2024, 6, 1, 0, 24, 0, 0, time.UTC), 16, 2},
	}
	insertDownsampleTierSamples(t, db, seed)

	start := time.Date(2024, 6, 1, 0, 15, 0, 0, time.UTC)
	end := time.Date(2024, 6, 1, 0, 25, 0, 0, time.UTC)
	got := runDownsampleTierPromQLGrid(t, db, `idelta(cpu_seconds_total[15m])`, start, end, schema.DownsampleTierBucket, true)

	want := map[time.Time]float64{
		time.Date(2024, 6, 1, 0, 15, 0, 0, time.UTC): 2, // pair (2@00:09, 4@00:14)
		time.Date(2024, 6, 1, 0, 20, 0, 0, time.UTC): 4, // pair (4@00:14, 8@00:19)
		time.Date(2024, 6, 1, 0, 25, 0, 0, time.UTC): 8, // pair (8@00:19, 16@00:24)
	}
	for ts, wantVal := range want {
		key := fmt.Sprintf("sliding@%s", ts.Format(time.RFC3339))
		row, ok := got[key]
		if !ok {
			t.Errorf("anchor %s: expected present, got absent", ts)
			continue
		}
		if row.value != wantVal {
			t.Errorf("anchor %s: want %v, got %v", ts, wantVal, row.value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("row-count mismatch: want %d rows, got %d (%v)", len(want), len(got), got)
	}
}

// TestDownsampleTier_ChdbCorruptedBucketPostMergeFilter proves
// downsampleTierExactWindowFilterFrag's post-merge re-filter is not a
// redundant no-op: it manually writes a tier row (bypassing the MV
// entirely, the same way a backfill bug or a manual data correction could)
// whose retained sample timestamp sits OUTSIDE its own BucketEnd's
// canonical interval — and outside the query's own window too — and
// confirms the query correctly excludes that sample rather than treating it
// as the newest one.
func TestDownsampleTier_ChdbCorruptedBucketPostMergeFilter(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)

	// One legitimate sample, written the normal way (via otel_metrics_sum +
	// the live MV), landing in the THIRD covering bucket.
	insertDownsampleTierSamples(t, db, []downsampleTierHostSample{
		{"corrupted", time.Date(2024, 6, 1, 0, 12, 0, 0, time.UTC), 500, 2},
	})

	// A hand-written tier row keyed BucketEnd=00:05:00 (the FIRST covering
	// bucket) whose retained sample timestamp is 00:20:00 — outside its own
	// bucket's (00:00,00:05] interval, and outside the query's own
	// (00:00,00:15] window too. No real write path (the live MV or the
	// backfill CLI) can produce this; it stands in for the data-corruption
	// scenario the re-filter defends against.
	const insertCorrupted = `
INSERT INTO otel_metrics_sum_downsample_tier
	(MetricName, Attributes, ResourceAttributes, ServiceName, BucketEnd, LastTwoSamples, Temporality)
SELECT
	'cpu_seconds_total',
	map('host', 'corrupted'),
	map(),
	'',
	toDateTime64('2024-06-01 00:05:00', 9),
	state,
	2
FROM (
	SELECT timeSeriesLastTwoSamplesState(t, v) AS state
	FROM (SELECT toDateTime64('2024-06-01 00:20:00', 9) AS t, 9999.0 AS v)
)`
	if _, err := db.Exec(insertCorrupted); err != nil {
		t.Fatalf("insert corrupted tier row: %v", err)
	}

	anchor := time.Date(2024, 6, 1, 0, 15, 0, 0, time.UTC)

	lastOverTime := runDownsampleTierPromQLGrid(t, db, `last_over_time(cpu_seconds_total[15m])`, anchor, anchor, schema.DownsampleTierBucket, true)
	idelta := runDownsampleTierPromQLGrid(t, db, `idelta(cpu_seconds_total[15m])`, anchor, anchor, schema.DownsampleTierBucket, true)
	irate := runDownsampleTierPromQLGrid(t, db, `irate(cpu_seconds_total[15m])`, anchor, anchor, schema.DownsampleTierBucket, true)

	key := fmt.Sprintf("corrupted@%s", anchor.Format(time.RFC3339))

	// last_over_time needs only 1 sample: with the corrupted 9999@00:20
	// correctly filtered out, the single legitimate sample (500@00:12) is
	// what remains — NOT 9999, which is what an unfiltered merge would
	// wrongly report as "most recent".
	if row, ok := lastOverTime[key]; !ok || row.value != 500 {
		t.Errorf("last_over_time host=corrupted: want 500 (the legitimate sample), got %v (present=%v)", row.value, ok)
	}

	// idelta / irate need >= 2 samples in-window. With the corrupted sample
	// filtered out, only 1 legitimate sample remains, so both must be
	// ABSENT — NOT a value computed from (500@00:12, 9999@00:20).
	if row, ok := idelta[key]; ok {
		t.Errorf("idelta host=corrupted: expected absent (only 1 legitimate sample after filtering), got %v", row.value)
	}
	if row, ok := irate[key]; ok {
		t.Errorf("irate host=corrupted: expected absent (only 1 legitimate sample after filtering), got %v", row.value)
	}
}
