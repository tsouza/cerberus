//go:build chdb

// chDB-backed correctness proof for the Gauge-sourced extension of the
// downsampled long-range tier (cerberus issue #2858, following #2751 /
// #2857): internal/schema/ddl's real renderDownsampleTierGaugeView DDL,
// folding otel_metrics_gauge into the SAME schema.DownsampleTierTable
// #2751's Sum-sourced MV already feeds, run end to end against a real
// ClickHouse engine (chDB) — mirroring range_window_downsample_tier_chdb_test.go's
// own approach for the Sum-only v1.
//
// Three properties this file proves empirically:
//
//  1. last_over_time() over a Gauge-table metric routes to the tier and
//     agrees EXACTLY with the ordinary fan-out reading the SAME raw
//     otel_metrics_gauge rows — see TestDownsampleTierGauge_ChdbCorrectness.
//  2. irate() / idelta() over the SAME Gauge-table metric, with the SAME
//     tier lowerers wired, do NOT route to the tier at all: the emitted SQL
//     never references schema.DownsampleTierTable, and the query still
//     executes correctly via the ordinary fan-out — see
//     TestDownsampleTierGauge_IrateIdeltaNeverRouteToTier.
//  3. The chosen design (a shared tier table fed by two independent MVs,
//     rather than two physical tier tables) leaves the pre-existing
//     Sum-table irate()/idelta()/last_over_time() behaviour completely
//     unaffected — see TestDownsampleTierGauge_SumPathUnaffected.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"strings"
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

// downsampleTierGaugeBucketAnchor mirrors downsampleTierBucketAnchor
// (range_window_downsample_tier_chdb_test.go) — the single query_range
// anchor every scenario below evaluates at.
var downsampleTierGaugeBucketAnchor = time.Date(2024, 6, 1, 0, 5, 0, 0, time.UTC)

// downsampleTierGaugeChdbSetup mirrors downsampleTierChdbSetup, provisioning
// the REAL DDL (both the Sum-sourced and Gauge-sourced MVs, since
// DownsampleTierEnabled gates the whole feature — internal/schema/ddl's
// RenderAll) inside db's isolated database.
func downsampleTierGaugeChdbSetup(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
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

// downsampleTierGaugeSample is one raw (host, ts, value) row
// downsampleTierGaugeSeed inserts into otel_metrics_gauge — no temporality
// field: a Gauge sample has no AggregationTemporality column at all.
type downsampleTierGaugeSample struct {
	host  string
	ts    time.Time
	value float64
}

// downsampleTierGaugeSeed mirrors downsampleTierSeed's shape for a Gauge
// metric: a normal two-sample pair, a single-sample host, a bucket gap, and
// a boundary-exact sample proving the Gauge MV shares the SAME
// downsampleTierBucketEndExpr ceiling-bucket convention the Sum MV uses.
var downsampleTierGaugeSeed = []downsampleTierGaugeSample{
	// "normal": two samples in [00:00,00:05], last_over_time must report
	// the LATER one (150).
	{"normal", time.Date(2024, 6, 1, 0, 1, 0, 0, time.UTC), 100},
	{"normal", time.Date(2024, 6, 1, 0, 3, 0, 0, time.UTC), 150},
	// "boundary": a boundary-exact sample AT 00:05:00 itself must join
	// THIS bucket (ending at 00:05:00), not the next one.
	{"boundary", downsampleTierGaugeBucketAnchor, 42},
	// "single": exactly one raw sample — last_over_time must still answer.
	{"single", time.Date(2024, 6, 1, 0, 2, 0, 0, time.UTC), 77},
	// "gap": no rows in THIS bucket, present in the next one only.
	{"gap", time.Date(2024, 6, 1, 0, 6, 0, 0, time.UTC), 999},
}

func insertDownsampleTierGaugeSamples(t *testing.T, db *sql.DB, samples []downsampleTierGaugeSample) {
	t.Helper()
	for _, s := range samples {
		_, err := db.Exec(
			`INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES (?, map('host', ?), ?, ?)`,
			"cpu_temperature", s.host, s.ts, s.value,
		)
		if err != nil {
			t.Fatalf("insert host=%s ts=%s: %v", s.host, s.ts, err)
		}
	}
}

// downsampleTierGaugeOnlySchema returns schema.DefaultOTelMetrics() with
// SumTable cleared. An UNSUFFIXED metric name like "cpu_temperature" is
// otherwise ambiguous (schema.Metrics.TablesForUnknownName's own doc: it
// deliberately fans across BOTH Gauge and Sum, so the hostmetrics-style
// cumulative-sum-under-a-bare-name shape isn't silently dropped) — the SAME
// pre-existing restriction that already keeps an unsuffixed COUNTER metric
// out of irate()/idelta()'s own tier eligibility (#2751/#1628: only a
// `_total`-suffixed name resolves unambiguously to Sum). Clearing SumTable
// collapses TablesForUnknownName to [GaugeTable] alone, giving
// rangeVectorSingleGaugeTable (internal/promql/lower.go, cerberus issue
// #2858) the unambiguous resolution its eligibility check requires — the
// realistic deployment shape this proves against is a Gauge-only (no Sum
// table) metrics source, mirroring the Sum-only restriction's own precedent
// rather than introducing a new one.
func downsampleTierGaugeOnlySchema() schema.Metrics {
	m := schema.DefaultOTelMetrics()
	m.SumTable = ""
	return m
}

// runDownsampleTierGaugeQuery mirrors runDownsampleTierPromQL: lowers query
// against s at the single downsampleTierGaugeBucketAnchor grid point, emits
// it, runs it against db, and returns (host -> value, emitted SQL text).
func runDownsampleTierGaugeQuery(t *testing.T, db *sql.DB, s schema.Metrics, query string, tierRouted bool) (map[string]float64, string) {
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
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		downsampleTierGaugeBucketAnchor, downsampleTierGaugeBucketAnchor, schema.DownsampleTierBucket,
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
	return out, sqlText
}

// TestDownsampleTierGauge_ChdbCorrectness proves last_over_time() over a
// Gauge-table metric routes to the tier (via renderDownsampleTierGaugeView's
// real MV) and agrees exactly with the ordinary fan-out over the SAME raw
// rows — the Gauge-sourced counterpart of TestDownsampleTier_ChdbCorrectness.
func TestDownsampleTierGauge_ChdbCorrectness(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierGaugeChdbSetup(t, db)
	insertDownsampleTierGaugeSamples(t, db, downsampleTierGaugeSeed)

	s := downsampleTierGaugeOnlySchema()
	const lastQuery = `last_over_time(cpu_temperature[5m])`
	tier, tierSQL := runDownsampleTierGaugeQuery(t, db, s, lastQuery, true)
	fanout, _ := runDownsampleTierGaugeQuery(t, db, s, lastQuery, false)

	if !strings.Contains(tierSQL, schema.DownsampleTierTable) {
		t.Fatalf("last_over_time() over a Gauge metric did not route to the tier — SQL:\n%s", tierSQL)
	}

	for host, fv := range fanout {
		tv, ok := tier[host]
		if !ok {
			t.Errorf("host=%s: present in fan-out (%v) but ABSENT from tier route", host, fv)
			continue
		}
		if math.Float64bits(tv) != math.Float64bits(fv) {
			t.Errorf("host=%s: tier=%.20g fanout=%.20g NOT bit-identical", host, tv, fv)
		}
	}
	if len(tier) != len(fanout) {
		t.Errorf("row-count divergence: tier=%d fanout=%d\ntier=%v\nfanout=%v", len(tier), len(fanout), tier, fanout)
	}

	wantLast := map[string]float64{
		"normal":   150, // the LATER of the two in-window samples
		"boundary": 42,  // boundary-exact sample joins THIS bucket
		"single":   77,  // a single sample is enough for last_over_time
	}
	for host, want := range wantLast {
		got, ok := tier[host]
		if !ok {
			t.Errorf("host=%s: expected present, got absent", host)
			continue
		}
		if got != want {
			t.Errorf("host=%s: want %v, got %v", host, want, got)
		}
	}
	if _, ok := tier["gap"]; ok {
		t.Error("host=gap: expected absent (no data in this bucket), got a value")
	}
}

// TestDownsampleTierGauge_IrateIdeltaNeverRouteToTier is the negative test
// cerberus issue #2858 explicitly calls for: irate()/idelta() over a
// Gauge-table metric — a gauge has no counter-reset semantics for either —
// must NOT route to the tier even with the SAME tier lowerers wired that
// correctly route last_over_time() for the identical metric above. Checked
// structurally (the emitted SQL never references schema.DownsampleTierTable)
// AND empirically (the query still executes correctly via the ordinary
// fan-out, matching a baseline run with NO tier lowerers wired at all).
func TestDownsampleTierGauge_IrateIdeltaNeverRouteToTier(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierGaugeChdbSetup(t, db)
	insertDownsampleTierGaugeSamples(t, db, downsampleTierGaugeSeed)

	s := downsampleTierGaugeOnlySchema()
	for _, query := range []string{`irate(cpu_temperature[5m])`, `idelta(cpu_temperature[5m])`} {
		tierWired, sqlText := runDownsampleTierGaugeQuery(t, db, s, query, true)
		if strings.Contains(sqlText, schema.DownsampleTierTable) {
			t.Errorf("%s: SQL references %s even though a gauge must never route to it:\n%s", query, schema.DownsampleTierTable, sqlText)
		}
		noLowerers, _ := runDownsampleTierGaugeQuery(t, db, s, query, false)
		if len(tierWired) != len(noLowerers) {
			t.Errorf("%s: row-count differs between tier-lowerers-wired (%d) and no-lowerers (%d) runs — "+
				"a gauge metric's routing must be identical either way", query, len(tierWired), len(noLowerers))
		}
		for host, want := range noLowerers {
			got, ok := tierWired[host]
			if !ok {
				t.Errorf("%s host=%s: present without tier lowerers (%v) but absent with them wired", query, host, want)
				continue
			}
			if math.Float64bits(got) != math.Float64bits(want) {
				t.Errorf("%s host=%s: tier-lowerers-wired=%.20g no-lowerers=%.20g NOT bit-identical", query, host, got, want)
			}
		}
	}
}

// TestDownsampleTierGauge_SumPathUnaffected confirms the Gauge-sourced MV
// addition (cerberus issue #2858) leaves the pre-existing Sum-table
// irate()/idelta()/last_over_time() tier routing (#2751/#2857) completely
// unaffected: a counter metric, seeded ALONGSIDE the gauge metric in the
// SAME database (both MVs live), still answers exactly as
// TestDownsampleTier_ChdbCorrectness proves it does in isolation.
func TestDownsampleTierGauge_SumPathUnaffected(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierGaugeChdbSetup(t, db)

	// A counter metric (Sum table) alongside the gauge fixtures above —
	// proving the two MVs coexist without cross-contamination.
	insertDownsampleTierGaugeSamples(t, db, downsampleTierGaugeSeed)
	if _, err := db.Exec(
		`INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, AggregationTemporality) VALUES
		 ('cpu_seconds_total', map('host', 'counter'), ?, 100, 2),
		 ('cpu_seconds_total', map('host', 'counter'), ?, 150, 2)`,
		time.Date(2024, 6, 1, 0, 1, 0, 0, time.UTC), time.Date(2024, 6, 1, 0, 3, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert counter samples: %v", err)
	}

	const irateQuery = `irate(cpu_seconds_total[5m])`
	s := schema.DefaultOTelMetrics()
	tier, sqlText := runDownsampleTierGaugeQuery(t, db, s, irateQuery, true)
	fanout, _ := runDownsampleTierGaugeQuery(t, db, s, irateQuery, false)

	if !strings.Contains(sqlText, schema.DownsampleTierTable) {
		t.Fatalf("irate() over the Sum-table counter did not route to the tier — SQL:\n%s", sqlText)
	}
	tv, ok := tier["counter"]
	if !ok {
		t.Fatal("host=counter: expected present in tier route, got absent")
	}
	fv, ok := fanout["counter"]
	if !ok {
		t.Fatal("host=counter: expected present in fan-out, got absent")
	}
	if math.Float64bits(tv) != math.Float64bits(fv) {
		t.Errorf("host=counter: tier=%.20g fanout=%.20g NOT bit-identical", tv, fv)
	}
	const want = 50.0 / 120 // (150-100)/120s, no reset
	if math.Abs(tv-want) > 1e-12 {
		t.Errorf("host=counter: want %v, got %v", want, tv)
	}
}
