//go:build chdb

// chDB-backed dual-emit parity pin for the fixed-accumulator decomposition
// (chplan.RangeWindow.FixedAccumulatorExtrapolated, cerberus issue #2760).
//
// Like laginframe_adjacency (range_window_lag_adjacency_chdb_test.go), the
// fixed-accumulator shape needs no server-version floor or experimental
// setting — chopt.FeatureFixedAccumulatorExtrapolated is AlwaysAvailable —
// so there is no feature-detection branch here: both arms always run.
//
// Each test lowers the SAME query TWICE against the SAME seed — once with
// the array-fold fan-out (chplan.RangeWindow,
// FixedAccumulatorExtrapolated=false) and once with the fixed-accumulator
// decomposition (FixedAccumulatorExtrapolated=true) — runs both on one
// ephemeral chDB session, and compares the per-(series, anchor) values.
//
// delta()'s raw result (`last_val - first_val`) is exactly order-independent
// — both emitters read the SAME extrapolatedValueExpr formula over the SAME
// picked first/last VALUES, with no intermediate summation to reorder — so
// its assertion is BIT-IDENTICAL (math.Float64bits equality), matching
// issue #2760's own claim ("Endpoint picks, counts, and delta()'s path are
// exactly order-independent").
//
// rate()/increase()'s counter-reset correction sums floats in ClickHouse's
// own non-deterministic aggregation order (sumIf), where the array-fold
// walks a strict time-ordered fold — issue #2760's own documented risk. A
// telescoping identity ((last-first)+reset_sum == the sequential fold) is
// exact in REAL arithmetic but not guaranteed bit-identical in float64, so
// that assertion is ULP-bounded, mirroring
// range_window_grid_native_increase_chdb_test.go's own measured-not-assumed
// budget.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// fixedAccumDeltaSeed covers delta()'s edge cases over a gauge table:
//   - 'walk': a normal up/down gauge walk (5 samples across the window).
//   - 'boundary': exactly 2 samples, one sitting right at the window's
//     opening edge (00:00:00, exclusive per PromQL's half-open range) and
//     one at its closing edge (00:05:00, inclusive) — proves the window
//     membership boundary (`ts > anchor-range AND ts <= anchor`) agrees
//     between both emitters, not just the count.
//   - 'single': one sample only (delta needs >= 2 — both arms must agree
//     it is ABSENT at every anchor).
//   - 'dup': two rows share timestamp 00:02:00 with different values (3.0
//     then 8.0 in arraySort's (ts, value) tuple order) — the
//     duplicate-timestamp dedup tie-break this shape's own doc calls out.
//   - 'nan': every sample NaN.
//   - 'nan-mixed': a NaN sandwiched between real samples, at the window's
//     first/last positions on different anchors.
var fixedAccumDeltaSeed = chsqltest.MetricsSeedDDL("otel_metrics_gauge") + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:00:00', 9), 40.0),
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:01:00', 9), 55.0),
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:02:00', 9), 45.0),
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:03:00', 9), 60.0),
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('cpu_temp_celsius', map('job', 'walk'), toDateTime64('2026-01-01 00:05:00', 9), 65.0),
    ('cpu_temp_celsius', map('job', 'boundary'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),
    ('cpu_temp_celsius', map('job', 'boundary'), toDateTime64('2026-01-01 00:05:00', 9), 120.0),
    ('cpu_temp_celsius', map('job', 'single'), toDateTime64('2026-01-01 00:02:00', 9), 4.0),
    ('cpu_temp_celsius', map('job', 'dup'), toDateTime64('2026-01-01 00:00:30', 9), 5.0),
    ('cpu_temp_celsius', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    ('cpu_temp_celsius', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    ('cpu_temp_celsius', map('job', 'dup'), toDateTime64('2026-01-01 00:04:00', 9), 6.0),
    ('cpu_temp_celsius', map('job', 'nan'), toDateTime64('2026-01-01 00:00:00', 9), nan),
    ('cpu_temp_celsius', map('job', 'nan'), toDateTime64('2026-01-01 00:05:00', 9), nan),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:01:00', 9), nan),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:03:00', 9), 2.0),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:04:00', 9), nan),
    ('cpu_temp_celsius', map('job', 'nan-mixed'), toDateTime64('2026-01-01 00:05:00', 9), 5.0);
`

func TestFixedAccumulatorDelta_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(fixedAccumDeltaSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	query := "sum by(job) (delta(cpu_temp_celsius[5m]))"
	var fanoutLowerers, fixedLowerers promql.RangeLowerers
	fanoutLowerers.Delta = promql.FanoutDeltaLowerer{}
	fixedLowerers.Delta = promql.FixedAccumulatorDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}

	fanout := runFixedAccumulatorRangeEmit(t, db, query, fanoutLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, false)
	fixed := runFixedAccumulatorRangeEmit(t, db, query, fixedLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, true)
	assertFixedAccumCellsBitIdentical(t, fanout, fixed, "delta")
}

// fixedAccumRateIncreaseSeed covers rate()/increase()'s edge cases over a
// Sum (counter) table:
//   - 'api': CUMULATIVE temporality (2, the schema default — see
//     testsql.BackfillMetricsColumns), sawtooth (exercises the
//     reset-correction sumIf term).
//   - 'web': CUMULATIVE, monotonic (no resets, exercises the plain
//     last_val-first_val telescoping path).
//   - 'delta-series': DELTA temporality (1), with prefix history before the
//     query window's own start (00:00:00) AND values crafted so the
//     reconstruction is OUTCOME-visible, not merely code-path-visible: a
//     tiny raw first-in-window sample (0.5) against a much larger
//     reconstructed prefix level (10.0+10.0=20.5 total) moves the counter
//     zero-clamp's `least(duration_to_start, sampled_interval*first_val/
//     counter_delta)` pick across its own threshold — confirmed by a manual
//     probe that hard-codes the clamp to the RAW first_val (skipping
//     deltaMatrixLevelSource/deltaFirstValFrag's reconstruction entirely):
//     that probe genuinely diverges from the array-fold on this seed
//     (00:04:00 and 00:05:00 anchors, several thousand ULP), where the real
//     (reconstructing) implementation does not. An earlier flat-value
//     version of this job passed even with the reconstruction disabled —
//     this shape replaces it precisely to close that gap.
//   - 'dup': CUMULATIVE, duplicate-timestamp dedup tie-break (two rows at
//     00:02:00, values 3.0 then 8.0).
//   - 'single': one sample only (rate/increase need >= 2 — ABSENT).
//   - 'nan': CUMULATIVE, every sample NaN.
var fixedAccumRateIncreaseSeed = `
CREATE OR REPLACE TABLE otel_metrics_sum (
    AggregationTemporality Int32,
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum (AggregationTemporality, MetricName, Attributes, TimeUnix, Value) VALUES
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 8.0),
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    (2, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 6.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:01:00', 9), 20.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    (2, 'http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:05:00', 9), 60.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2025-12-31 23:55:00', 9), 10.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:01:00', 9), 0.5),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:02:00', 9), 20.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:03:00', 9), 20.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:04:00', 9), 20.0),
    (1, 'http_requests_total', map('job', 'delta-series'), toDateTime64('2026-01-01 00:05:00', 9), 20.0),
    (2, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:00:30', 9), 5.0),
    (2, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    (2, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    (2, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:04:00', 9), 6.0),
    (2, 'http_requests_total', map('job', 'single'), toDateTime64('2026-01-01 00:02:00', 9), 4.0),
    (2, 'http_requests_total', map('job', 'nan'), toDateTime64('2026-01-01 00:00:00', 9), nan),
    (2, 'http_requests_total', map('job', 'nan'), toDateTime64('2026-01-01 00:05:00', 9), nan);
`

// maxFixedAccumRateUlp / maxFixedAccumRateUlpDivergentCells bound the
// counter-reset correction term's float64 reordering risk (see this file's
// own doc comment) for BOTH rate() and increase() — measured against the
// real chDB substrate, not assumed. The measured shape on this fixture
// (sawtooth resets, DELTA-temporality prefix reconstruction, duplicate
// timestamps) is 0 divergent cells at 0 ULP: chDB's sumIf/argMin/argMax
// evaluation order happened to agree bit-for-bit with the array-fold's
// strict time-ordered walk here, so both constants are pinned at the
// measured value — 0 — mirroring
// range_window_grid_native_increase_chdb_test.go's own "pinned at measured,
// never loosened for headroom" rule. A genuinely parallel-aggregation
// ClickHouse substrate (unlike chDB's embedded single session) COULD still
// reorder sumIf's accumulation and introduce real ULP drift; if CI's real-CH
// lanes (strict-scan / compat) ever observe that, bump these with the
// measured cell/ULP counts from that run, the same reviewed way the
// increase-native budget was pinned — never loosen speculatively.
const (
	maxFixedAccumRateUlp               = 0
	maxFixedAccumRateUlpDivergentCells = 0
)

func TestFixedAccumulatorRateIncrease_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(fixedAccumRateIncreaseSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	for _, fn := range []string{"rate", "increase"} {
		t.Run(fn, func(t *testing.T) {
			query := "sum by(job) (" + fn + "(http_requests_total[5m]))"
			var fanoutLowerers, fixedLowerers promql.RangeLowerers
			if fn == "rate" {
				fanoutLowerers.Rate = promql.FanoutRateLowerer{}
				fixedLowerers.Rate = promql.FixedAccumulatorRateLowerer{Fallback: promql.FanoutRateLowerer{}}
			} else {
				fanoutLowerers.Increase = promql.FanoutIncreaseLowerer{}
				fixedLowerers.Increase = promql.FixedAccumulatorIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}
			}

			fanout := runFixedAccumulatorRangeEmit(t, db, query, fanoutLowerers,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, false)
			fixed := runFixedAccumulatorRangeEmit(t, db, query, fixedLowerers,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, true)
			assertFixedAccumCellsWithinUlpBudget(t, fanout, fixed, fn)
		})
	}
}

// runFixedAccumulatorRangeEmit mirrors runLagAdjacencyRangeEmit exactly
// (range_window_lag_adjacency_chdb_test.go), wiring the caller-supplied
// promql.RangeLowerers instead. fixedAccumulator is used only for error
// messages.
func runFixedAccumulatorRangeEmit(
	t *testing.T,
	db *sql.DB,
	query string,
	lowerers promql.RangeLowerers,
	rangeStart time.Time,
	span time.Duration,
	step time.Duration,
	fixedAccumulator bool,
) map[resampleCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeEnd := rangeStart.Add(span)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, step,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (fixedAccumulator=%v): %v", fixedAccumulator, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (fixedAccumulator=%v): %v", fixedAccumulator, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS label_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (fixedAccumulator=%v): %v\nSQL: %s", fixedAccumulator, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[resampleCell]float64)
	for rows.Next() {
		var labelJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&labelJSON, &ts, &v); err != nil {
			t.Fatalf("scan (fixedAccumulator=%v): %v", fixedAccumulator, err)
		}
		out[resampleCell{job: extractJobLabel(labelJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (fixedAccumulator=%v): %v", fixedAccumulator, err)
	}
	return out
}

// assertFixedAccumCellsBitIdentical mirrors
// assertResampleCellsBitIdentical (range_window_lag_adjacency_chdb_test.go)
// exactly, for delta()'s exactly order-independent path.
func assertFixedAccumCellsBitIdentical(t *testing.T, fanout, fixed map[resampleCell]float64, fn string) {
	t.Helper()
	if len(fanout) == 0 {
		t.Fatalf("%s: fan-out produced zero rows — the fixture must yield a populated grid", fn)
	}
	if len(fixed) != len(fanout) {
		t.Fatalf("%s: row-count divergence: fanout=%d fixed=%d cells\nfanout=%v\nfixed=%v",
			fn, len(fanout), len(fixed), fanout, fixed)
	}
	for cell, fv := range fanout {
		xv, ok := fixed[cell]
		if !ok {
			t.Errorf("%s: cell %+v present in fan-out but absent in fixed-accumulator", fn, cell)
			continue
		}
		fBits, xBits := math.Float64bits(fv), math.Float64bits(xv)
		if fBits != xBits {
			t.Errorf("%s: cell %+v: fanout=%.20g fixed=%.20g NOT bit-identical", fn, cell, fv, xv)
		}
	}
	t.Logf("%s dual-emit parity: %d/%d cells bit-identical. fixed-accumulator == array-fold fan-out.",
		fn, len(fanout), len(fanout))
}

// assertFixedAccumCellsWithinUlpBudget mirrors
// range_window_grid_native_increase_chdb_test.go's own ULP-budget assertion
// for rate()/increase()'s counter-reset correction term — see this file's
// own doc comment for why this path is NOT asserted bit-identical.
func assertFixedAccumCellsWithinUlpBudget(t *testing.T, fanout, fixed map[resampleCell]float64, fn string) {
	t.Helper()
	if len(fanout) == 0 {
		t.Fatalf("%s: fan-out produced zero rows — the fixture must yield a populated grid", fn)
	}
	if len(fixed) != len(fanout) {
		t.Fatalf("%s: row-count divergence: fanout=%d fixed=%d cells\nfanout=%v\nfixed=%v",
			fn, len(fanout), len(fixed), fanout, fixed)
	}

	var (
		ulpDivergent int
		maxSeenUlp   uint64
	)
	for cell, fv := range fanout {
		xv, ok := fixed[cell]
		if !ok {
			t.Errorf("%s: cell %+v present in fan-out but absent in fixed-accumulator", fn, cell)
			continue
		}
		if math.Float64bits(fv) == math.Float64bits(xv) {
			continue // bit-identical — allowed, not required
		}
		if math.IsNaN(fv) && math.IsNaN(xv) {
			continue // both NaN — the payload bit pattern is not load-bearing here
		}
		ulps := ulpDistance(fv, xv)
		if ulps > maxSeenUlp {
			maxSeenUlp = ulps
		}
		if ulps > maxFixedAccumRateUlp {
			t.Errorf("%s: cell %+v: fanout=%.20g fixed=%.20g differ by %d ULP (> %d — arithmetic bug, not float-order noise)",
				fn, cell, fv, xv, ulps, maxFixedAccumRateUlp)
			continue
		}
		ulpDivergent++
		t.Logf("%s: cell %+v: fanout=%.20g fixed=%.20g differ by %d ULP (within budget %d)",
			fn, cell, fv, xv, ulps, maxFixedAccumRateUlp)
	}
	if ulpDivergent > maxFixedAccumRateUlpDivergentCells {
		t.Errorf("%s: ULP divergence grew to %d cells; documented bound is %d — "+
			"the fixed-accumulator arithmetic drifted further from the fan-out than expected, investigate",
			fn, ulpDivergent, maxFixedAccumRateUlpDivergentCells)
	}
	t.Logf("%s dual-emit parity: %d/%d cells bit-identical, %d cells differ by at most %d ULP (max seen %d), "+
		"within documented bounds (%d cells / %d ULP). fixed-accumulator == fan-out to full observable precision.",
		fn, len(fanout)-ulpDivergent, len(fanout), ulpDivergent, maxFixedAccumRateUlp, maxSeenUlp,
		maxFixedAccumRateUlpDivergentCells, maxFixedAccumRateUlp)
}
