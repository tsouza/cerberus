//go:build chdb

// chDB-backed dual-emit parity pin for the experimental instant-mode
// timeSeries*ToGrid lowering (chplan.RangeWindowGridNativeInstant, cerberus
// issue #2748).
//
// The test lowers the SAME instant (bare, non-query_range) `rate(m[5m])` /
// `changes(m[5m])` expression TWICE against the SAME seed — once with the
// fan-out (RangeWindow, OuterRange == 0) and once with the instant native
// lowering (RangeWindowGridNativeInstant, a degenerate one-point grid) —
// runs BOTH on the same ephemeral chDB session, and compares the per-series
// value.
//
// Grid length 1. Unlike the matrix dual-emit test (which compares a whole
// grid of (series, anchor) cells), an instant query has exactly one
// evaluation anchor, so this is inherently the "grid length 1" case the
// issue's own test-plan names explicitly — there is no wider grid to prove
// parity across.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// instantDualEmitSeed mirrors dualEmitSeed (range_window_grid_native_chdb_test.go)
// exactly — same fixture, same two `cerberus_ql` series — so a reader who
// already knows that fixture recognises this one immediately. Reused
// verbatim rather than shared as a package var: each _chdb_test.go file in
// this package owns its fixture so it stays self-contained (matching this
// package's existing convention — the matrix, delta, irate, idelta variants
// each keep their own seed too).
var instantDualEmitSeed = chsqltest.MetricsSeedDDL("otel_metrics_sum") + `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:01:00', 9), 12.0),
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:02:00', 9), 24.0),
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:03:00', 9), 36.0),
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:04:00', 9), 48.0),
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:05:00', 9), 60.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:01:00', 9), 6.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:02:00', 9), 6.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:03:00', 9), 18.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:04:00', 9), 18.0),
    ('cerberus_queries_total', map('cerberus_ql', 'logql'), toDateTime64('2026-01-01 00:05:00', 9), 30.0);
`

// instantDualEmitAnchor is the single instant eval anchor every case below
// evaluates at — the same timestamp the matrix dual-emit fixture's final
// grid point lands on, so the two tests' expected numbers are directly
// comparable by a reader who knows one already.
var instantDualEmitAnchor = time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

func TestNativeTSGridInstant_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(instantDualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
	hasFn := tsGridFnPresent(t, db)

	for _, fn := range []string{"rate", "changes"} {
		t.Run(fn, func(t *testing.T) {
			query := fn + "(cerberus_queries_total[5m])"
			fanout := runInstantDualEmit(t, db, query, false)
			if !hasFn {
				t.Logf("NOTICE: the native timeSeries*ToGrid aggregate for %s is absent on this chDB "+
					"substrate — native parity assertion bypassed (fan-out half still validated).", fn)
				return
			}
			native := runInstantDualEmit(t, db, query, true)

			if len(native) != len(fanout) {
				t.Fatalf("row-count divergence: native=%d fanout=%d series", len(native), len(fanout))
			}
			for ql, fv := range fanout {
				nv, ok := native[ql]
				if !ok {
					t.Errorf("series %q present in fan-out but absent in native", ql)
					continue
				}
				if math.Float64bits(nv) != math.Float64bits(fv) {
					t.Errorf("series %q: native=%.20g fanout=%.20g NOT bit-identical", ql, nv, fv)
				}
			}
			t.Logf("%s: %d/%d series bit-identical between instant native and fan-out (grid length 1)",
				fn, len(fanout), len(fanout))
		})
	}
}

// instantTemporalityDualEmitSeed seeds ONE CUMULATIVE-temporality series and
// ONE DELTA-temporality series under the SAME metric — mirroring
// native_rate_temporality_mixed.txtar's matrix fixture — so
// TestNativeTSGridInstant_DualEmitParity_Temporality exercises BOTH arms of
// NativeRateLowerer's instant temporality-union split (cerberus issue
// #2843) in one seed. Each series carries exactly 2 in-window samples —
// rate()'s >= 2-sample floor, no more — since the instant shape needs only
// enough points to compute one value, unlike the matrix fixture's six
// (which walk a whole step grid).
// The `cerberus_ql` label (rather than a `temporality` one) is reused
// verbatim from instantDualEmitSeed above so this test can key its result by
// the SAME extractQLLabel helper instead of a near-duplicate extractor.
//
// Hand-written CREATE TABLE rather than chsqltest.MetricsSeedDDL: that
// helper's fixed column list has no AggregationTemporality column, matching
// every OTHER temporality-bearing chDB fixture in this package (e.g.
// range_window_delta_prefix_aggregate_chdb_test.go), which all seed the
// column the same way.
var instantTemporalityDualEmitSeed = `
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, AggregationTemporality) VALUES
    ('cerberus_queries_total', map('cerberus_ql', 'cumulative'), toDateTime64('2026-01-01 00:01:00', 9), 0.0, 2),
    ('cerberus_queries_total', map('cerberus_ql', 'cumulative'), toDateTime64('2026-01-01 00:04:00', 9), 24.0, 2),
    ('cerberus_queries_total', map('cerberus_ql', 'delta'), toDateTime64('2026-01-01 00:01:00', 9), 0.0, 1),
    ('cerberus_queries_total', map('cerberus_ql', 'delta'), toDateTime64('2026-01-01 00:04:00', 9), 6.0, 1);
`

// TestNativeTSGridInstant_DualEmitParity_Temporality is
// TestNativeTSGridInstant_DualEmitParity's temporality-bearing sibling
// (cerberus issue #2843). Every case in that test — and every other
// #2748 differential in this codebase — clears AggregationTemporalityColumn
// specifically to keep the comparison orthogonal to issue #1628's
// DELTA-vs-CUMULATIVE runtime branch, which means NONE of them would catch
// a regression in the union-split path this test exists to pin: the
// "native" configuration here does NOT clear the column, so a
// temporality-bearing instant rate() genuinely reaches
// NativeRateLowerer's CUMULATIVE-native / DELTA-fan-out union (issue #2843)
// rather than declining and falling back to the plain fan-out — the exact
// gap the issue closes.
func TestNativeTSGridInstant_DualEmitParity_Temporality(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(instantTemporalityDualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
	hasFn := tsGridFnPresent(t, db)

	query := "rate(cerberus_queries_total[5m])"
	fanout := runInstantTemporalityDualEmit(t, db, query, false)
	if len(fanout) != 2 {
		t.Fatalf("fan-out: expected 2 series (cumulative + delta), got %d: %+v", len(fanout), fanout)
	}
	if !hasFn {
		t.Logf("NOTICE: the native timeSeriesRateToGrid aggregate is absent on this chDB " +
			"substrate — native parity assertion bypassed (fan-out half still validated).")
		return
	}
	// Activation pin: the native configuration must actually lower to the
	// CUMULATIVE-native / DELTA-fan-out UnionAll split (cerberus issue
	// #2843), not silently decline and fall back to the plain fan-out
	// RangeWindow — which would make the bit-identical comparison below
	// vacuous (comparing the fan-out against itself).
	assertInstantTemporalityUnionActivated(t, query)

	native := runInstantTemporalityDualEmit(t, db, query, true)

	if len(native) != len(fanout) {
		t.Fatalf("row-count divergence: native=%d fanout=%d series", len(native), len(fanout))
	}
	for ql, fv := range fanout {
		nv, ok := native[ql]
		if !ok {
			t.Errorf("series %q present in fan-out but absent in native", ql)
			continue
		}
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("series %q: native=%.20g fanout=%.20g NOT bit-identical", ql, nv, fv)
		}
	}
	t.Logf("temporality union: %d/%d series bit-identical between instant native and fan-out",
		len(fanout), len(fanout))
}

// assertInstantTemporalityUnionActivated lowers query (with Instant: true,
// AggregationTemporalityColumn wired) and fails unless the result is the
// two-arm UnionAll{*chplan.RangeWindowGridNativeInstant, *chplan.RangeWindow}
// NativeRateLowerer.LowerRate's instant temporality-union split builds
// (cerberus issue #2843) — the same "prove it actually fired" pin
// AssertNativeFunctionFired gives the real-CH nightly integration test
// (test/perf/nightly/realch_ts_grid_instant_memory_integration_test.go),
// at the plan level rather than query_log.
func assertInstantTemporalityUnionActivated(t *testing.T, query string) {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		instantDualEmitAnchor, instantDualEmitAnchor, 0,
		promql.LowerOpts{Lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{}, Instant: true,
		}}})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("temporality-bearing instant rate() plan = %T, want *chplan.UnionAll — "+
			"the union split did not activate, so the parity comparison below is vacuous", plan)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality union has %d arms, want 2", len(union.Inputs))
	}
	if _, ok := union.Inputs[0].(*chplan.RangeWindowGridNativeInstant); !ok {
		t.Fatalf("cumulative union arm = %T, want *chplan.RangeWindowGridNativeInstant", union.Inputs[0])
	}
	if _, ok := union.Inputs[1].(*chplan.RangeWindow); !ok {
		t.Fatalf("delta union arm = %T, want *chplan.RangeWindow", union.Inputs[1])
	}
}

// runInstantTemporalityDualEmit mirrors runInstantDualEmit exactly EXCEPT it
// leaves AggregationTemporalityColumn wired — the whole point of this
// file's temporality test, and the axis every OTHER instant differential in
// this package deliberately clears (see instantDualEmitSeed's own runner).
func runInstantTemporalityDualEmit(t *testing.T, db *sql.DB, query string, native bool) map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Rate = promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}, Instant: true}
	}
	s := schema.DefaultOTelMetrics()
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		instantDualEmitAnchor, instantDualEmitAnchor, 0,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS ql_json, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]float64)
	for rows.Next() {
		var qlJSON string
		var v float64
		if err := rows.Scan(&qlJSON, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[extractQLLabel(qlJSON)] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	return out
}

// TestNativeTSGridInstant_StalenessGap pins the "no sample -> no row"
// contract at the single instant anchor: a series with fewer than 2
// in-window samples (rate/deriv/predict_linear's own NULL threshold) is
// ABSENT from the result, on BOTH the fan-out and the native path — never a
// NULL-valued row. This is the shape ARRAY JOIN + WHERE grid_val IS NOT NULL
// exists to guarantee (see emitRangeWindowGridNativeInstant's own doc).
func TestNativeTSGridInstant_StalenessGap(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	seed := chsqltest.MetricsSeedDDL("otel_metrics_sum") + `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('cerberus_queries_total', map('cerberus_ql', 'promql'), toDateTime64('2026-01-01 00:00:00', 9), 5.0);
`
	for _, stmt := range splitSeedStatements(seed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
	hasFn := tsGridFnPresent(t, db)

	// A single sample sits far outside the 5m lookback ending at
	// instantDualEmitAnchor (00:05:00): the window has ZERO in-window
	// samples, well under rate()'s >= 2 floor.
	fanout := runInstantDualEmit(t, db, "rate(cerberus_queries_total[5m])", false)
	if len(fanout) != 0 {
		t.Errorf("fan-out: expected 0 rows for a staleness gap, got %d: %+v", len(fanout), fanout)
	}
	if !hasFn {
		t.Logf("NOTICE: timeSeriesRateToGrid absent on this chDB substrate — " +
			"native staleness-gap assertion bypassed (fan-out half still validated).")
		return
	}
	native := runInstantDualEmit(t, db, "rate(cerberus_queries_total[5m])", true)
	if len(native) != 0 {
		t.Errorf("native: expected 0 rows for a staleness gap, got %d: %+v", len(native), native)
	}
}

// runInstantDualEmit lowers + emits query as an INSTANT (Step == 0)
// expression evaluated at instantDualEmitAnchor, with the experimental
// instant flag set to `native`, runs the resulting SQL on db, and returns
// the per-series value keyed by the `cerberus_ql` label.
func runInstantDualEmit(t *testing.T, db *sql.DB, query string, native bool) map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Rate = promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}, Instant: true}
		lowerers.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}, Instant: true}
	}
	// AggregationTemporalityColumn cleared for the same reason
	// range_window_grid_native_chdb_test.go's own runDualEmit clears it: a
	// concern orthogonal to this parity proof (see that function's comment).
	s := schema.DefaultOTelMetrics()
	s.AggregationTemporalityColumn = ""
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		instantDualEmitAnchor, instantDualEmitAnchor, 0,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS ql_json, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]float64)
	for rows.Next() {
		var qlJSON string
		var v float64
		if err := rows.Scan(&qlJSON, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[extractQLLabel(qlJSON)] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	return out
}
