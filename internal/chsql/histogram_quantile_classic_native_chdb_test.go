//go:build chdb

// chDB-backed dual-emit parity gate for the ClickHouse-native
// classic-histogram ladder (chplan.RangeBucketGridNative).
//
// The test lowers the SAME range-mode
// `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[5m])))` expression
// TWICE against the SAME seed — once with the native strategy OFF (the
// RangeBucketFanout + array-expression ladder fold) and once with it ON (the
// unnest + timeSeriesRateToGrid ladder) — runs BOTH on one ephemeral chDB
// session, and compares the answered quantile per (series, anchor) at full
// float64 precision.
//
// Why this is the correctness proof rather than a golden diff. The fan-out's
// expected_rows are already Prometheus-pinned by the spec corpus (many of the
// classic-histogram fixtures carry `oracle: prometheus`), so native == fan-out
// transitively proves native == Prometheus. A golden alone would only pin what
// the native path DOES, not that it agrees with the reference.
//
// The seeds are chosen for the semantics most likely to break, one per case:
// heterogeneous bucket layouts ACROSS series, a layout change WITHIN a series
// mid-window, the per-series two-sample floor at the leading grid edge, a
// counter reset, and DELTA aggregation temporality (which the native arm must
// hand to the fan-out arm rather than answer itself).
//
// Feature-detect, not a test-skip. The two aggregates are gated behind a
// ClickHouse version floor; the substrate is probed once via system.functions
// and the native assertion fires only when both are present (true on the
// current chDB substrate). When absent the fan-out half still runs and a notice
// is logged, so the coverage loss is never silent.
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
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// hqNativeSeedDDL is the OTel-CH classic-histogram table shape the default
// schema reads: the bucket pair plus the ResourceAttributes / ServiceName
// columns the label projection always references and the
// AggregationTemporality column the rate fold branches on.
const hqNativeSeedDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

// hqNativeGridStart / hqNativeGridEnd / hqNativeGridStep define the query_range
// grid every case is evaluated over, and hqNativeWindow the rate window.
var (
	hqNativeGridStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hqNativeGridEnd   = hqNativeGridStart.Add(10 * time.Minute)
)

const (
	hqNativeGridStep = 30 * time.Second
	hqNativeWindow   = "5m"
)

// hqNativeCase is one seed + query pair. Each names the semantic it exists to
// break, so a failure says which rule the native ladder got wrong.
type hqNativeCase struct {
	name  string
	query string
	rows  string
}

func hqNativeCases() []hqNativeCase {
	q := func(agg string) string {
		return "histogram_quantile(0.5, " + agg + " by(le) (rate(http_server_request_duration_bucket[" + hqNativeWindow + "])))"
	}
	return []hqNativeCase{
		{
			// Baseline: one series, stable layout, monotone counter. Pins that
			// the ladder, the extrapolation factor and the /range division all
			// agree before any of the harder rules are exercised.
			name:  "uniform_layout",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [3, 5, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [4, 9, 14], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:04:00', 9), [7, 11, 20], [0.1, 0.5, 1.0])`,
		},
		{
			// Two series whose bucket layouts share NO bound. The union-bounds
			// machinery must produce a ladder per series over ITS OWN layout
			// and the across-series merge must widen them; a rung a series
			// never reported must not be fabricated on its ladder.
			name:  "mixed_layouts_across_series",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [10, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), [14, 3, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [100.0, 200.0, 300.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), [0, 0, 10], [100.0, 200.0, 300.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:02:00', 9), [2, 0, 17], [100.0, 200.0, 300.0])`,
		},
		{
			// ONE series whose layout changes mid-window. Bounds that leave the
			// layout must still appear on the ladder for the anchors whose
			// window carries them, and must NOT appear (carrying a fabricated
			// zero) for the anchors whose window does not.
			name:  "layout_change_midwindow",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [2, 4, 6], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [4, 7, 11], [0.1, 0.5, 2.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [6, 9, 15], [0.1, 0.5, 2.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:06:00', 9), [9, 12, 21], [0.1, 0.5, 2.0])`,
		},
		{
			// A series whose counter restarts mid-window, per `le` rung.
			name:  "counter_reset",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [10, 20, 30], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [12, 25, 38], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [1, 2, 4], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [3, 6, 10], [0.1, 0.5, 1.0])`,
		},
		{
			// Two series with DIFFERENT scrape cadences and phases, so their
			// boundary-extrapolation factors genuinely differ — the correction
			// reference Prometheus applies per series before summing.
			name:  "distinct_cadences",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:00:10', 9), [0, 1, 2], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:00:40', 9), [1, 3, 5], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:01:10', 9), [2, 6, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:01:40', 9), [4, 8, 14], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:02:00', 9), [3, 4, 7], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:04:00', 9), [5, 9, 15], [0.1, 0.5, 1.0])`,
		},
		{
			// DELTA aggregation temporality. The native aggregate reads a
			// CUMULATIVE counter, so the whole series must ride the fan-out arm
			// of the complementary union — this case proves the split routes it
			// there rather than answering it natively.
			name:  "delta_temporality",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [1, 3, 4], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [2, 2, 5], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [1, 4, 6], [0.1, 0.5, 1.0], 1)`,
		},
		{
			// One CUMULATIVE series and one DELTA series in the same query, so
			// BOTH arms of the union produce rows and the merge folds them
			// together.
			name:  "mixed_temporality",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:01:00', 9), [2, 4, 6], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:02:00', 9), [5, 8, 13], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:01:00', 9), [1, 3, 4], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:02:00', 9), [2, 2, 5], [0.1, 0.5, 1.0], 1)`,
		},
		{
			// A non-`sum` across-series fold. `max by(le)` is where a uniform
			// per-rung scale factor would still cancel but a per-SERIES one
			// would not, so it separates the two.
			name:  "max_by_le",
			query: q("max"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [1, 5, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), [2, 9, 17], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:30', 9), [7, 8, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:03:00', 9), [11, 13, 15], [0.1, 0.5, 1.0])`,
		},
		{
			// `count by(le)` is the scale-INVARIANT fold: its ladder counts
			// contributing series, so a rung fabricated at any anchor changes
			// the answer outright rather than by a float ulp.
			name:  "count_by_le",
			query: q("count"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [4, 5, 6], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [2.0, 4.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), [3, 8], [2.0, 4.0])`,
		},
	}
}

// hqCell keys an answered quantile by (series-label JSON, anchor timestamp).
type hqCell struct {
	labels string
	anchor string
}

// maxHQNativeUlp is the divergence budget between the two evaluation orders.
// It is NOT an epsilon tolerance: 2 ULP is the WORST divergence actually
// measured across every case below (most cells are bit-identical), so a real
// arithmetic error — which moves a quantile by a bucket width, not by a last
// bit — still fails this outright. The residue comes from the two paths
// computing the same Prometheus extrapolatedRate in different orders: the
// native aggregate accumulates in compiled C++ and divides by the window
// seconds (which the emitter multiplies back out), the fan-out evaluates the
// same arithmetic as an array expression, and the quantile interpolation then
// divides through the resulting ladder, carrying the last-bit difference into
// the answer.
const maxHQNativeUlp = 2

func TestNativeClassicHistogramLadder_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	hasFn := hqNativeFnsPresent(t, db)

	for _, tc := range hqNativeCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			seed(t, db, hqNativeSeedDDL)
			cols := "(MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds"
			if strings.Contains(tc.rows, "], 1)") || strings.Contains(tc.rows, "], 2)") {
				cols += ", AggregationTemporality"
			}
			cols += ")"
			seed(t, db, "INSERT INTO otel_metrics_histogram "+cols+" VALUES "+tc.rows)

			fanout := runHQEmit(t, db, tc.query, false)
			if !hasFn {
				t.Logf("NOTICE: %s/%s absent on this chDB substrate — native parity assertion "+
					"bypassed (fan-out half still validated).", hqNativeRateFn, hqNativeSeenFn)
				return
			}
			native := runHQEmit(t, db, tc.query, true)

			if len(native) != len(fanout) {
				t.Fatalf("row-count divergence: native=%d fanout=%d cells\nnative=%v\nfanout=%v",
					len(native), len(fanout), native, fanout)
			}
			var diverged int
			var maxUlp uint64
			for cell, fv := range fanout {
				nv, ok := native[cell]
				if !ok {
					t.Errorf("cell %+v present in fan-out but absent in native", cell)
					continue
				}
				if math.Float64bits(nv) == math.Float64bits(fv) {
					continue
				}
				if math.IsNaN(nv) && math.IsNaN(fv) {
					continue
				}
				u := ulpDistance(nv, fv)
				if u > maxHQNativeUlp {
					t.Errorf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (> %d — an arithmetic "+
						"divergence, not float-order noise)", cell, nv, fv, u, maxHQNativeUlp)
					continue
				}
				if u > maxUlp {
					maxUlp = u
				}
				diverged++
			}
			t.Logf("%s: %d/%d cells bit-identical, %d differ (worst %d ULP, budget %d)",
				tc.name, len(fanout)-diverged, len(fanout), diverged, maxUlp, maxHQNativeUlp)
		})
	}
}

// runHQEmit lowers + emits query at the shared range grid with the native
// classic-histogram strategy on or off, runs it, and returns the answered
// quantile per cell.
func runHQEmit(t *testing.T, db *sql.DB, query string, native bool) map[hqCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	if native {
		// Window-slide has no chopt entry of its own (chopt.AlwaysAvailable),
		// so it is composed into the chain unconditionally, matching
		// cmd/cerberus's own nativeRangeLowerers — a no-op for every query
		// this suite runs (all `rate(...)`, never sum_over_time, so
		// windowSlideEligible always declines here), kept for consistency
		// rather than because this suite exercises it.
		lowerers.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{
			Fallback: promql.WindowSlideClassicHistogramWindowLowerer{
				Fallback: promql.FanoutClassicHistogramWindowLowerer{},
			},
		}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		hqNativeGridStart, hqNativeGridEnd, hqNativeGridStep,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS lbl, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[hqCell]float64)
	for rows.Next() {
		var lbl string
		var ts time.Time
		var v float64
		if err := rows.Scan(&lbl, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[hqCell{labels: lbl, anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — every case must yield a populated grid", native)
	}
	return out
}

// The two native aggregates this lowering needs: the rate grid itself and the
// presence signal that keeps a rung out of an anchor whose window never carried
// it (see internal/chsql's bucketGridSeenFn).
const (
	hqNativeRateFn = "timeSeriesRateToGrid"
	hqNativeSeenFn = "timeSeriesResetsToGrid"
)

// hqNativeFnsPresent feature-detects both aggregates via system.functions.
func hqNativeFnsPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name IN (?, ?)",
		hqNativeRateFn, hqNativeSeenFn,
	).Scan(&n); err != nil {
		t.Fatalf("probe system.functions: %v", err)
	}
	return n == 2
}

// seed executes a semicolon-separated DDL/DML script against db.
func seed(t *testing.T, db *sql.DB, script string) {
	t.Helper()
	for _, stmt := range splitSeedStatements(script) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
}
