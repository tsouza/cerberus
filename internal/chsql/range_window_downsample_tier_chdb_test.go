//go:build chdb

// chDB-backed correctness proof for the operator opt-in downsampled
// long-range tier (cerberus issue #2751): internal/schema/ddl's real
// renderDownsampleTierTable/View DDL, the real timeSeriesLastTwoSamples
// state/merge/finalize mechanics, and internal/chsql's real
// emitRangeWindowDownsampleTier emission, run end to end against a real
// ClickHouse engine (chDB) rather than trusted from reasoning alone.
//
// Five properties this file proves empirically, each the load-bearing
// claim a comment elsewhere in this feature makes:
//
//  1. Boundary-exact bucketing (internal/schema/ddl's downsampleTierBucketEndExpr):
//     a raw sample landing EXACTLY on a bucket boundary joins the bucket
//     ENDING there, matching PromQL's half-open-left/closed-right window —
//     see host "boundary" below.
//  2. The counter-reset restriction is SAFE BY CONSTRUCTION: irate() /
//     idelta() over the tier's last-two-samples state agree EXACTLY with
//     computing the same functions over the SAME two raw samples via the
//     ordinary fan-out (dual-emit parity) — including a reset landing ON
//     the retained trailing pair, where irate correctly reset-corrects and
//     idelta correctly does not — see hosts "cumulative" / "reset".
//  3. DELTA-temporality counters take the RAW-current-sample irate branch,
//     not the CUMULATIVE reset-aware one — see host "delta".
//  4. The multi-part AggregatingMergeTree read hazard is real and this
//     package's GROUP BY + xMerge read handles it correctly — see host
//     "multipart" (two separate INSERT batches, i.e. two unmerged parts,
//     for the SAME bucket key).
//  5. Insufficient samples degrade to an ABSENT row (never a wrong value)
//     for irate/idelta, while last_over_time still answers from a single
//     sample — see host "single".
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// downsampleTierBucketAnchor is the single query_range anchor every
// scenario below evaluates at — the BucketEnd of the bucket [00:00, 00:05)
// on an otherwise arbitrary but Unix-epoch bucket-aligned day.
var downsampleTierBucketAnchor = time.Date(2024, 1, 1, 0, 5, 0, 0, time.UTC)

// downsampleTierChdbSetup creates the real DDL (via internal/schema/ddl's
// actual render functions, not a hand-copied mirror) inside db's isolated
// database, and enables the experimental setting every statement below
// needs.
func downsampleTierChdbSetup(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	// ddl.Config.withDefaults forces an empty Database to "default", so an
	// empty cfg.Database here would qualify every CREATE as
	// default.<table> — landing OUTSIDE chsqltest.OpenIsolatedChDB's own
	// isolated database. Reading currentDatabase() and pinning cfg.Database
	// to it keeps every statement inside the isolated database, the same
	// isolation contract every other chdb test in this package gets by
	// using bare (unqualified) hand-written CREATE TABLE statements
	// instead — this file uses the REAL ddl.RenderAll, so it has to be
	// explicit about the database ddl.Config otherwise defaults away from.
	var currentDB string
	if err := db.QueryRow("SELECT currentDatabase()").Scan(&currentDB); err != nil {
		t.Fatalf("read currentDatabase(): %v", err)
	}
	cfg := ddl.Config{Database: currentDB, SkipDatabaseCreate: true, DownsampleTierEnabled: true}
	stmts, err := ddl.RenderAll(cfg, []ddl.Signal{ddl.Metrics})
	if err != nil {
		t.Fatalf("ddl.RenderAll: %v", err)
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec DDL: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
}

// downsampleTierHostSample is one raw (host, ts, value, temporality) row
// downsampleTierSeed inserts into otel_metrics_sum.
type downsampleTierHostSample struct {
	host        string
	ts          time.Time
	value       float64
	temporality int
}

// downsampleTierSeed is every scenario host's raw data, all landing in the
// SAME bucket [00:00, 00:05) except "gap" (deliberately empty) and
// "otherbucket" (the following bucket, proving the eligible query's own
// [Start,End] bound excludes it). CUMULATIVE is temporality 2 (neither
// Unspecified=0 nor Delta=1 — see chsql.CounterOrDeltaPairDelta, which
// treats anything other than Delta as the reset-aware branch); DELTA is 1
// (schema.AggregationTemporalityDelta).
var downsampleTierSeed = []downsampleTierHostSample{
	// "cumulative": a plain monotonic pair, PLUS a third, EARLIER sample
	// this bucket must discard (proving the tier keeps only the last two
	// per bucket, not every sample) and a boundary-exact sample AT
	// 00:05:00 itself (proving it joins THIS bucket, not the next).
	{"cumulative", time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC), 20, 2},
	{"cumulative", time.Date(2024, 1, 1, 0, 3, 0, 0, time.UTC), 100, 2},
	{"cumulative", downsampleTierBucketAnchor, 150, 2}, // exactly 00:05:00
	// "reset": a counter reset landing ON the retained trailing pair.
	{"reset", time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC), 500, 2},
	{"reset", time.Date(2024, 1, 1, 0, 3, 0, 0, time.UTC), 50, 2},
	// "delta": DELTA temporality — irate must use the raw current sample
	// alone, not curr-prev.
	{"delta", time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC), 30, 1},
	{"delta", time.Date(2024, 1, 1, 0, 3, 0, 0, time.UTC), 45, 1},
	// "single": exactly one raw sample — irate/idelta must report absent,
	// last_over_time must still answer.
	{"single", time.Date(2024, 1, 1, 0, 2, 0, 0, time.UTC), 77, 2},
	// "gap": no rows in THIS bucket, but present in the next one — proves
	// the tier read for THIS anchor is a clean absence, not an error, and
	// that presence in a DIFFERENT bucket does not leak in.
	{"gap", time.Date(2024, 1, 1, 0, 6, 0, 0, time.UTC), 999, 2},
	// "boundary_excluded": a SOLE sample exactly on the PREVIOUS bucket's
	// own boundary (00:00:00) must NOT be pulled into the (00:00,00:05]
	// bucket this test queries (which ends at 00:05:00) — a sample AT
	// 00:00:00 belongs to the bucket ENDING at 00:00:00 instead
	// (internal/schema/ddl's bucketEndExpr). With no other sample in this
	// bucket, this host must be entirely ABSENT from every function's
	// answer at the 00:05:00 anchor.
	{"boundary_excluded", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 1, 2},
}

// downsampleTierMultipartSeed is applied via TWO SEPARATE INSERT batches
// (see TestDownsampleTier_ChdbCorrectness) to force two unmerged
// AggregatingMergeTree parts for the SAME (host="multipart", bucket) key —
// the real-world shape of an MV firing once per insert batch, or late
// data. The TRUE last-two across BOTH parts is (20@00:02:00, 30@00:04:00);
// a naive per-part read would see only ITS OWN part's last two and answer
// wrong for at least one part.
var downsampleTierMultipartSeedBatch1 = []downsampleTierHostSample{
	{"multipart", time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC), 10, 2},
	{"multipart", time.Date(2024, 1, 1, 0, 2, 0, 0, time.UTC), 20, 2},
}

var downsampleTierMultipartSeedBatch2 = []downsampleTierHostSample{
	{"multipart", time.Date(2024, 1, 1, 0, 4, 0, 0, time.UTC), 30, 2},
}

func insertDownsampleTierSamples(t *testing.T, db *sql.DB, samples []downsampleTierHostSample) {
	t.Helper()
	for _, s := range samples {
		_, err := db.Exec(
			`INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, AggregationTemporality) VALUES (?, map('host', ?), ?, ?, ?)`,
			"cpu_seconds_total", s.host, s.ts, s.value, s.temporality,
		)
		if err != nil {
			t.Fatalf("insert host=%s ts=%s: %v", s.host, s.ts, err)
		}
	}
}

// downsampleTierLowererTable wires ONLY the DownsampleTier strategy
// (mirroring cmd/cerberus's nativeRangeLowerers with ONLY
// chopt.FeatureDownsampleTier resolved — no ts_grid_irate/idelta/
// last_over_time layered beneath it), so this test proves the tier's OWN
// correctness in isolation from the native ts_grid family's.
func downsampleTierLowererTable() promql.RangeLowerers {
	return promql.RangeLowerers{
		Irate:        promql.DownsampleTierIrateLowerer{Fallback: promql.FanoutIrateLowerer{}},
		Idelta:       promql.DownsampleTierIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}},
		LastOverTime: promql.DownsampleTierLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}},
	}
}

// runDownsampleTierPromQL lowers query at the single downsampleTierBucketAnchor
// grid point (Step == bucket, one anchor) with tierRouted selecting between
// the tier-wired table and a plain all-fan-out table, emits it, runs it
// against db, and returns host -> value.
func runDownsampleTierPromQL(t *testing.T, db *sql.DB, query string, tierRouted bool) map[string]float64 {
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
		downsampleTierBucketAnchor, downsampleTierBucketAnchor, schema.DownsampleTierBucket,
		promql.LowerOpts{Lowerers: lowerers})
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

func TestDownsampleTier_ChdbCorrectness(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)
	// otel_metrics_sum already exists — ddl.RenderAll (via
	// downsampleTierChdbSetup) creates it from the real upstream template,
	// which carries AggregationTemporality, MetricName, Attributes, and
	// TimeUnix/Value, matching insertDownsampleTierSamples' own INSERT
	// column list.

	insertDownsampleTierSamples(t, db, downsampleTierSeed)
	insertDownsampleTierSamples(t, db, downsampleTierMultipartSeedBatch1)
	insertDownsampleTierSamples(t, db, downsampleTierMultipartSeedBatch2)

	const irateQuery = `irate(cpu_seconds_total[5m])`
	const ideltaQuery = `idelta(cpu_seconds_total[5m])`
	const lastQuery = `last_over_time(cpu_seconds_total[5m])`

	irateTier := runDownsampleTierPromQL(t, db, irateQuery, true)
	irateFanout := runDownsampleTierPromQL(t, db, irateQuery, false)
	ideltaTier := runDownsampleTierPromQL(t, db, ideltaQuery, true)
	ideltaFanout := runDownsampleTierPromQL(t, db, ideltaQuery, false)
	lastTier := runDownsampleTierPromQL(t, db, lastQuery, true)
	lastFanout := runDownsampleTierPromQL(t, db, lastQuery, false)

	// Property 2 + 3 + 4: dual-emit parity against the fan-out (which reads
	// RAW rows, never the tier) proves the tier's read is numerically
	// correct for every host, including "delta" (temporality-aware irate)
	// and "multipart" (the multi-part merge hazard) — bit-identical, no
	// tolerance, since both sides do the same float arithmetic in the same
	// order (subtract two float64 samples, divide by an integer-derived
	// interval).
	for host, fv := range irateFanout {
		tv, ok := irateTier[host]
		if !ok {
			t.Errorf("irate host=%s: present in fan-out (%v) but ABSENT from tier route", host, fv)
			continue
		}
		if math.Float64bits(tv) != math.Float64bits(fv) {
			t.Errorf("irate host=%s: tier=%.20g fanout=%.20g NOT bit-identical", host, tv, fv)
		}
	}
	if len(irateTier) != len(irateFanout) {
		t.Errorf("irate row-count divergence: tier=%d fanout=%d\ntier=%v\nfanout=%v",
			len(irateTier), len(irateFanout), irateTier, irateFanout)
	}
	for host, fv := range ideltaFanout {
		tv, ok := ideltaTier[host]
		if !ok {
			t.Errorf("idelta host=%s: present in fan-out (%v) but ABSENT from tier route", host, fv)
			continue
		}
		if math.Float64bits(tv) != math.Float64bits(fv) {
			t.Errorf("idelta host=%s: tier=%.20g fanout=%.20g NOT bit-identical", host, tv, fv)
		}
	}
	for host, fv := range lastFanout {
		tv, ok := lastTier[host]
		if !ok {
			t.Errorf("last_over_time host=%s: present in fan-out (%v) but ABSENT from tier route", host, fv)
			continue
		}
		if math.Float64bits(tv) != math.Float64bits(fv) {
			t.Errorf("last_over_time host=%s: tier=%.20g fanout=%.20g NOT bit-identical", host, tv, fv)
		}
	}

	// Property 5: "single" (one raw sample) must be ABSENT from irate/idelta
	// on BOTH paths, but present in last_over_time.
	if _, ok := irateTier["single"]; ok {
		t.Errorf("irate host=single: expected absent (only 1 sample in window), got a value")
	}
	if _, ok := ideltaTier["single"]; ok {
		t.Errorf("idelta host=single: expected absent (only 1 sample in window), got a value")
	}
	if v, ok := lastTier["single"]; !ok || v != 77 {
		t.Errorf("last_over_time host=single: want 77, got %v (present=%v)", v, ok)
	}

	// Property: "gap" (no data in this bucket) must be absent everywhere,
	// and "boundary_excluded"'s two samples (both landing in the PREVIOUS
	// bucket) must never surface in THIS anchor's answer.
	for _, absentHost := range []string{"gap", "boundary_excluded"} {
		if v, ok := irateTier[absentHost]; ok {
			t.Errorf("irate host=%s: expected absent (no data in this bucket), got %v", absentHost, v)
		}
		if v, ok := lastTier[absentHost]; ok {
			t.Errorf("last_over_time host=%s: expected absent (no data in this bucket), got %v", absentHost, v)
		}
	}

	// Property 1 (boundary-exact bucketing): "cumulative" pins its trailing
	// pair at (100@00:03:00, 150@00:05:00 exactly) — the boundary-exact
	// sample MUST be included. Pinned expected values, not just "matches
	// fan-out" (fan-out could theoretically share the same bug):
	wantIdelta := map[string]float64{
		"cumulative": 50,   // 150 - 100
		"reset":      -450, // 50 - 500, uncorrected
		"delta":      15,   // 45 - 30, uncorrected (idelta never branches on temporality)
		"multipart":  10,   // 30 - 20 (the TRUE last two across both parts)
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
	}
	wantIrate := map[string]float64{
		"cumulative": 50.0 / 120, // no reset: plain diff / 120s
		"reset":      50.0 / 120, // reset detected: raw current / 120s
		"delta":      45.0 / 120, // DELTA temporality: raw current / 120s
		"multipart":  10.0 / 120, // TRUE last two across both parts
	}
	for host, want := range wantIrate {
		got, ok := irateTier[host]
		if !ok {
			t.Errorf("irate host=%s: expected present, got absent", host)
			continue
		}
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("irate host=%s: want %v, got %v", host, want, got)
		}
	}
}

// hostFromJSON extracts the "host" label out of a toJSONString'd
// Map(String,String) column, mirroring the family's own dual-emit tests'
// (e.g. range_window_last_over_time_chdb_test.go's) JSON-decode idiom.
func hostFromJSON(t *testing.T, hostJSON string) string {
	t.Helper()
	// Attributes carries exactly one key ("host") in this seed, so a tiny
	// manual extract avoids pulling encoding/json into this file purely for
	// a single-key map — {"host":"cumulative"}.
	const prefix = `{"host":"`
	if len(hostJSON) < len(prefix)+2 || hostJSON[:len(prefix)] != prefix {
		t.Fatalf("unexpected Attributes JSON shape: %s", hostJSON)
	}
	rest := hostJSON[len(prefix):]
	end := 0
	for end < len(rest) && rest[end] != '"' {
		end++
	}
	return rest[:end]
}
