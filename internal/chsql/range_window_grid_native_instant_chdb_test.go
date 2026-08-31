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
