//go:build chdb

// chDB-backed dual-emit parity pin for the lagInFrame annotation shape
// (chplan.RangeWindow.LagAdjacency, cerberus issue #2759).
//
// Unlike the timeSeries*ToGrid dual-emit tests (range_window_resets_chdb_test.go
// and siblings), lagInFrame/leadInFrame need no server-version floor or
// experimental setting — chopt.FeatureLagInFrameAdjacency is AlwaysAvailable —
// so there is no feature-detection branch here: both arms always run.
//
// Each test lowers the SAME query TWICE against the SAME seed — once with the
// array-fold fan-out (chplan.RangeWindow, LagAdjacency=false) and once with the
// lagInFrame annotation shape (LagAdjacency=true) — runs both on one ephemeral
// chDB session, and compares the per-(series, anchor) values. changes/resets
// counts and irate/idelta deltas are exact arithmetic over Float64 inputs (no
// interpolation), so every assertion is BIT-IDENTICAL
// (math.Float64bits equality), not a tolerance.
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

// lagAdjacencyChangesResetsSeed extends the resets sibling test's own
// resetsSeed (api sawtooth / web monotonic / nan / nan-mixed / single-sample
// jobs — see range_window_resets_chdb_test.go) with a DUPLICATE-TIMESTAMP job:
// two rows share timestamp 00:02:00 with different values (3.0 then 8.0 in
// arraySort's (ts, value) tuple order). The array-fold path counts EVERY
// adjacent array position, duplicates included, as its own pair; this is the
// scenario range_window_lag_adjacency.go's doc calls out as needing NO
// survivor filtering for changes/resets (unlike irate/idelta) — every row
// with a valid prev contributes independently via sumIf.
var lagAdjacencyChangesResetsSeed = resetsSeed + `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:00:30', 9), 5.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:04:00', 9), 6.0);
`

func TestLagAdjacencyChangesResets_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(lagAdjacencyChangesResetsSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	for _, fn := range []string{"changes", "resets"} {
		t.Run(fn, func(t *testing.T) {
			query := "sum by(job) (" + fn + "(http_requests_total[5m]))"
			fanout := runLagAdjacencyChangesResetsEmit(t, db, query, false)
			laginframe := runLagAdjacencyChangesResetsEmit(t, db, query, true)
			assertResampleCellsBitIdentical(t, fanout, laginframe, fn)
		})
	}
}

// runLagAdjacencyChangesResetsEmit lowers query with Changes/Resets wired to
// the lag-adjacency strategy (when lagAdjacency is set, Fallback = the plain
// fan-out) or the fan-out directly, runs the optimizer, executes on db, and
// returns the per-(job, anchor) value.
func runLagAdjacencyChangesResetsEmit(t *testing.T, db *sql.DB, query string, lagAdjacency bool) map[resampleCell]float64 {
	t.Helper()
	var lowerers promql.RangeLowerers
	if lagAdjacency {
		lowerers.Changes = promql.LagAdjacencyChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
		lowerers.Resets = promql.LagAdjacencyResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
	} else {
		lowerers.Changes = promql.FanoutChangesLowerer{}
		lowerers.Resets = promql.FanoutResetsLowerer{}
	}
	return runLagAdjacencyRangeEmit(t, db, query, lowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		5*time.Minute, 30*time.Second, lagAdjacency)
}

// lagAdjacencyIrateSeed reuses the exact DELTA-temporality counter-reset
// dataset irate_delta_temporality_range.txtar pins (job 'api': DELTA
// temporality, includes a reset 100->50->5 the DELTA branch reads as raw
// current-sample deltas, not counter-reset repairs), and adds:
//   - job 'cumulative': the SAME sample shape but AggregationTemporality=0
//     (CUMULATIVE), forcing CounterOrDeltaPairDelta's OTHER branch —
//     `if(curr < prev, curr, curr - prev)` — through the argMax tuple.
//   - job 'dup': duplicate-timestamp survivor tie-break coverage.
//   - job 'single': one sample only (irate needs >= 2, so this job produces
//     no row at either anchor its lone sample could cover — both arms must
//     agree it is ABSENT).
//   - job 'nan': every sample NaN (the pair delta is NaN either way; both
//     arms must still agree, including on any() reading NaN's own presence).
var lagAdjacencyIrateSeed = `
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
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:00:30', 9), 10.0),
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 40.0),
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:01:30', 9), 20.0),
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 100.0),
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:03:30', 9), 50.0),
    (1, 'http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 5.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:00:30', 9), 10.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:01:00', 9), 40.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:01:30', 9), 20.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:02:00', 9), 100.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:03:30', 9), 50.0),
    (0, 'http_requests_total', map('job', 'cumulative'), toDateTime64('2026-01-01 00:04:00', 9), 5.0),
    (0, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:00:30', 9), 5.0),
    (0, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    (0, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    (0, 'http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:04:00', 9), 6.0),
    (0, 'http_requests_total', map('job', 'single'), toDateTime64('2026-01-01 00:02:00', 9), 4.0),
    (0, 'http_requests_total', map('job', 'nan'), toDateTime64('2026-01-01 00:00:30', 9), nan),
    (0, 'http_requests_total', map('job', 'nan'), toDateTime64('2026-01-01 00:04:00', 9), nan);
`

func TestLagAdjacencyIrate_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(lagAdjacencyIrateSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	query := "sum by(job) (irate(http_requests_total[1m]))"
	var fanoutLowerers, lagLowerers promql.RangeLowerers
	fanoutLowerers.Irate = promql.FanoutIrateLowerer{}
	lagLowerers.Irate = promql.LagAdjacencyIrateLowerer{Fallback: promql.FanoutIrateLowerer{}}

	fanout := runLagAdjacencyRangeEmit(t, db, query, fanoutLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 1*time.Minute, false)
	laginframe := runLagAdjacencyRangeEmit(t, db, query, lagLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 1*time.Minute, true)
	assertResampleCellsBitIdentical(t, fanout, laginframe, "irate")
}

// lagAdjacencyIdeltaSeed is idelta's gauge-table sibling of
// lagAdjacencyIrateSeed: idelta never reads AggregationTemporality (see
// range_window.go's emitRangeWindowIDelta doc), so no temporality column or
// job is needed. Covers duplicate timestamps, a single-sample job (idelta
// needs >= 2), and an all-NaN job.
var lagAdjacencyIdeltaSeed = chsqltest.MetricsSeedDDL("otel_metrics_gauge") + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('temperature', map('job', 'a'), toDateTime64('2026-01-01 00:00:30', 9), 5.0),
    ('temperature', map('job', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    ('temperature', map('job', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 8.0),
    ('temperature', map('job', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 6.0),
    ('temperature', map('job', 'b'), toDateTime64('2026-01-01 00:02:00', 9), 4.0),
    ('temperature', map('job', 'c'), toDateTime64('2026-01-01 00:00:30', 9), nan),
    ('temperature', map('job', 'c'), toDateTime64('2026-01-01 00:04:00', 9), nan);
`

func TestLagAdjacencyIdelta_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range splitSeedStatements(lagAdjacencyIdeltaSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	query := "sum by(job) (idelta(temperature[5m]))"
	var fanoutLowerers, lagLowerers promql.RangeLowerers
	fanoutLowerers.Idelta = promql.FanoutIdeltaLowerer{}
	lagLowerers.Idelta = promql.LagAdjacencyIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}}

	fanout := runLagAdjacencyRangeEmit(t, db, query, fanoutLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 5*time.Minute, false)
	laginframe := runLagAdjacencyRangeEmit(t, db, query, lagLowerers,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 5*time.Minute, 5*time.Minute, true)
	assertResampleCellsBitIdentical(t, fanout, laginframe, "idelta")
}

// runLagAdjacencyRangeEmit lowers query as a query_range plan over
// [rangeStart, rangeStart+span] at step, with lowerers wired by the caller,
// runs the default optimizer pipeline, executes on db, and returns the
// per-(label, anchor) value keyed by resampleCell. lagAdjacency is used only
// for error messages.
func runLagAdjacencyRangeEmit(
	t *testing.T,
	db *sql.DB,
	query string,
	lowerers promql.RangeLowerers,
	rangeStart time.Time,
	span time.Duration,
	step time.Duration,
	lagAdjacency bool,
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
		t.Fatalf("lower (lagAdjacency=%v): %v", lagAdjacency, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (lagAdjacency=%v): %v", lagAdjacency, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS label_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (lagAdjacency=%v): %v\nSQL: %s", lagAdjacency, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[resampleCell]float64)
	for rows.Next() {
		var labelJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&labelJSON, &ts, &v); err != nil {
			t.Fatalf("scan (lagAdjacency=%v): %v", lagAdjacency, err)
		}
		out[resampleCell{job: extractJobLabel(labelJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (lagAdjacency=%v): %v", lagAdjacency, err)
	}
	return out
}

// assertResampleCellsBitIdentical fails t unless fanout and laginframe hold
// exactly the same (label, anchor) cell set with math.Float64bits-identical
// values — the "exact integer/boolean semantics, not interpolated floats"
// bar issue #2759 sets, not a tolerance.
func assertResampleCellsBitIdentical(t *testing.T, fanout, laginframe map[resampleCell]float64, fn string) {
	t.Helper()
	if len(fanout) == 0 {
		t.Fatalf("%s: fan-out produced zero rows — the fixture must yield a populated grid", fn)
	}
	if len(laginframe) != len(fanout) {
		t.Fatalf("%s: row-count divergence: fanout=%d laginframe=%d cells\nfanout=%v\nlaginframe=%v",
			fn, len(fanout), len(laginframe), fanout, laginframe)
	}
	for cell, fv := range fanout {
		lv, ok := laginframe[cell]
		if !ok {
			t.Errorf("%s: cell %+v present in fan-out but absent in lagInFrame annotation", fn, cell)
			continue
		}
		fBits, lBits := math.Float64bits(fv), math.Float64bits(lv)
		// NaN != NaN under IEEE-754, but every NaN bit pattern chDB emits for
		// these kernels is the canonical one (isNaN's own driving value never
		// differs by payload here), so bit-equality is still the right check —
		// a differing NaN payload would itself be a real, worth-catching
		// divergence between the two emitters.
		if fBits != lBits {
			t.Errorf("%s: cell %+v: fanout=%.20g laginframe=%.20g NOT bit-identical", fn, cell, fv, lv)
		}
	}
	t.Logf("%s dual-emit parity: %d/%d cells bit-identical. lagInFrame annotation == array-fold fan-out.",
		fn, len(fanout), len(fanout))
}
