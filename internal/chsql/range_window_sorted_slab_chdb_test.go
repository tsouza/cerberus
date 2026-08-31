//go:build chdb

// chDB-backed dual-emit parity pin for the sorted-slab decomposition
// (chplan.RangeWindow.SortedSlabOverTime, cerberus issue #2761).
//
// Like laginframe_adjacency / fixed_accumulator_extrapolated, the sorted-slab
// shape needs no server-version floor or experimental setting —
// chopt.FeatureSortedSlabOverTime is AlwaysAvailable — so there is no
// feature-detection branch here: both arms always run.
//
// Each test lowers the SAME query TWICE against the SAME seed — once with
// the array-fold fan-out (chplan.RangeWindow, SortedSlabOverTime=false) and
// once with the sorted-slab decomposition (SortedSlabOverTime=true) — runs
// both on one ephemeral chDB session, and compares the per-(series, anchor)
// values.
//
// Unlike rate/increase's fixed-accumulator counter-reset term (which sums
// floats in ClickHouse's own non-deterministic sumIf order), sum_over_time /
// avg_over_time's reducers here are arraySum/arrayAvg applied to a slice that
// preserves the array-fold's own element order (arrayFilter over samples,
// samples arraySort-ascending — see range_window_sorted_slab.go's own doc).
// Both arms therefore fold the SAME elements in the SAME order, so this
// assertion is BIT-IDENTICAL (math.Float64bits equality), not merely
// ULP-bounded.
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

// sortedSlabOverTimeSeed covers sum_over_time() / avg_over_time()'s edge
// cases over a Gauge table:
//   - 'walk': a normal up/down gauge walk (6 samples across the window).
//   - 'boundary': exactly 2 samples, one sitting right at the window's
//     opening edge (00:00:00, exclusive per PromQL's half-open range) and
//     one at its closing edge (00:05:00, inclusive) — proves the window
//     membership boundary (`ts > anchor-range AND ts <= anchor`) agrees
//     between both emitters, not just the count.
//   - 'single': one sample only.
//   - 'dup': two rows share timestamp 00:02:00 with different values (3.0
//     then 8.0 in arraySort's (ts, value) tuple order) — pins the NO-DEDUP
//     contract (both samples count individually, in BOTH the sum and the
//     avg's divisor) this shape's own doc calls out.
//   - 'nan': every sample NaN.
//   - 'nan-mixed': a NaN sandwiched between real samples, at the window's
//     first/last positions on different anchors.
var sortedSlabOverTimeSeed = chsqltest.MetricsSeedDDL("otel_metrics_gauge") + `
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

func TestSortedSlabOverTime_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(sortedSlabOverTimeSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	for _, fn := range []string{"sum_over_time", "avg_over_time"} {
		t.Run(fn, func(t *testing.T) {
			query := "sum by(job) (" + fn + "(cpu_temp_celsius[5m]))"
			var fanoutLowerers, slabLowerers promql.RangeLowerers
			fanoutLowerers.OverTime = promql.FanoutOverTimeLowerer{}
			slabLowerers.OverTime = promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}}

			fanout := runSortedSlabRangeEmit(t, db, query, fanoutLowerers,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, false)
			slab := runSortedSlabRangeEmit(t, db, query, slabLowerers,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 30*time.Second, true)
			assertSortedSlabCellsBitIdentical(t, fanout, slab, fn)
		})
	}
}

// runSortedSlabRangeEmit mirrors runFixedAccumulatorRangeEmit exactly
// (range_window_fixed_accumulator_chdb_test.go), wiring the caller-supplied
// promql.RangeLowerers instead. sortedSlab is used only for error messages.
func runSortedSlabRangeEmit(
	t *testing.T,
	db *sql.DB,
	query string,
	lowerers promql.RangeLowerers,
	rangeStart time.Time,
	span time.Duration,
	step time.Duration,
	sortedSlab bool,
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
		t.Fatalf("lower (sortedSlab=%v): %v", sortedSlab, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (sortedSlab=%v): %v", sortedSlab, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS label_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (sortedSlab=%v): %v\nSQL: %s", sortedSlab, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[resampleCell]float64)
	for rows.Next() {
		var labelJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&labelJSON, &ts, &v); err != nil {
			t.Fatalf("scan (sortedSlab=%v): %v", sortedSlab, err)
		}
		out[resampleCell{job: extractJobLabel(labelJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (sortedSlab=%v): %v", sortedSlab, err)
	}
	return out
}

// assertSortedSlabCellsBitIdentical mirrors assertFixedAccumCellsBitIdentical
// (range_window_fixed_accumulator_chdb_test.go) exactly, for
// sum_over_time/avg_over_time's exactly order-independent path (see this
// file's own doc comment for why bit-identical, not ULP-bounded, is the
// right assertion here).
func assertSortedSlabCellsBitIdentical(t *testing.T, fanout, slab map[resampleCell]float64, fn string) {
	t.Helper()
	if len(fanout) == 0 {
		t.Fatalf("%s: fan-out produced zero rows — the fixture must yield a populated grid", fn)
	}
	if len(slab) != len(fanout) {
		t.Fatalf("%s: row-count divergence: fanout=%d slab=%d cells\nfanout=%v\nslab=%v",
			fn, len(fanout), len(slab), fanout, slab)
	}
	for cell, fv := range fanout {
		xv, ok := slab[cell]
		if !ok {
			t.Errorf("%s: cell %+v present in fan-out but absent in sorted-slab", fn, cell)
			continue
		}
		if math.IsNaN(fv) && math.IsNaN(xv) {
			continue // both NaN — the payload bit pattern is not load-bearing here
		}
		fBits, xBits := math.Float64bits(fv), math.Float64bits(xv)
		if fBits != xBits {
			t.Errorf("%s: cell %+v: fanout=%.20g slab=%.20g NOT bit-identical", fn, cell, fv, xv)
		}
	}
	t.Logf("%s dual-emit parity: %d/%d cells bit-identical. sorted-slab == array-fold fan-out.",
		fn, len(fanout), len(fanout))
}
