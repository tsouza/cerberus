//go:build chdb

// chDB-backed CHARACTERIZATION PIN for the sub-second window-membership gap of
// the experimental native regression path (timeSeriesDerivToGrid /
// timeSeriesPredictLinearToGrid). This is the test that converts the documented
// limitation into an enforced, self-describing fact.
//
// Why this test exists. The two dual-emit parity tests
// (range_window_{deriv,predict_linear}_chdb_test.go) prove native == fan-out
// bit-identically, but ONLY on whole-second-aligned seeds. The native path fits
// the per-window regression on a WHOLE-SECOND timestamp axis (toDateTime(ts) —
// see nativeGridTsAxisFrag), and that same argument also drives the aggregate's
// window-MEMBERSHIP bucketing. So a sample whose sub-second offset straddles a
// range-window boundary is bucketed by its FLOORED second in native, while the
// fan-out (sampleAnchorFanoutFrag on raw srcTs) and real Prometheus decide
// membership on the RAW timestamp. Whole-second seeds can't surface this — the
// floor is a no-op there. This test seeds a boundary-straddling sample on
// purpose and pins the resulting three-way split so it can neither regress
// silently nor be "fixed" without the goldens noticing.
//
// The three answers this fixture produces (single series, a 30s-step grid; the
// pin asserts on the anchor at 00:05:00 whose range-5m window (0s, 300s] has its
// left edge exactly at the second the +0.5s sample straddles):
//
//   - native (whole-second axis, CURRENT SHIPPING): floors the +0.5s sample to
//     second 0, which is OUTSIDE the left-open window (0,300], so the sample is
//     DROPPED. Floored membership + floored x.
//   - fan-out (portable, prod default): raw-ts membership KEEPS the +0.5s
//     sample, but the SLR x-axis is dateDiff('second', anchor, ts) — also
//     floored. Raw membership + floored x.
//   - true Prometheus: raw-ts membership + FRACTIONAL-second x. This is what the
//     native path computes if fed the raw DateTime64(9) axis and scaled to
//     per-second (proven empirically: raw-ns deriv * 1e9 == the closed-form
//     least-squares slope). NEITHER shipping path equals it on sub-second data.
//
// The pinned invariants (version-robust — relationships, not brittle float
// constants):
//
//   - native != fan-out by more than float-order noise: the membership
//     divergence is REAL, not a rounding artefact. This is the limitation, made
//     a hard fact.
//   - fan-out is STRICTLY CLOSER to true Prometheus than native is: fan-out only
//     floors the x-axis, while native ALSO drops a whole sample, so the fan-out
//     (the prod default) is the more-correct path here. This is the reassurance
//     that the default-off native path is the one carrying the gap, not the
//     shipping default.
//
// Substrate: exercised on CI. The native aggregates are present on the pinned
// chdb-core v26.5.0 (CH 26.5.1.1) `chdb` lane; the feature-detect guard keeps
// the fan-out half validating on an older local libchdb. The forbid-skip gate
// bans the test-skip API, so the guard is a documented runtime conditional that
// always executes.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// subSecondDerivSeed plants a single host series whose FIRST sample sits at
// +0.5s past the window's left edge. Under raw-ts membership the sample is
// inside (0s,300s]; under whole-second flooring it collapses to second 0 and
// falls outside. The remaining samples are whole-second so the divergence is
// attributable to exactly that one straddling point.
const subSecondDerivSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:00:00.5', 9), 5.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 10.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 22.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 33.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 47.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 58.0);
`

// subSecondDerivQuery anchors a single grid point at 00:05:00 (Start==End) with
// a 5-minute range, so the whole test reduces to one window (0s,300s] and one
// regression slope per path — the cleanest possible frame for the membership
// split.
const subSecondDerivQuery = `sum by(host) (deriv(load_state[5m]))`

// subSecondFloatCloseUlps bounds "float-order noise": two paths that compute the
// SAME math via different float operations may differ by a few ULP. A membership
// divergence is orders of magnitude larger, so this is the floor above which a
// difference counts as a real (not rounding) divergence. Expressed as an
// absolute epsilon on slopes of order ~0.2, a handful of ULP is ~1e-15.
const subSecondFloatCloseEps = 1e-12

func TestNativeTSGridDeriv_SubSecondMembershipPin(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(subSecondDerivSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runSubSecondRegressionEmit(t, db, subSecondDerivQuery, false)
	if !derivFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesDerivToGrid absent on this local chDB substrate " +
			"(older libchdb than the pinned chdb-core v26.5.0) — native membership pin " +
			"bypassed (fan-out half still computed). Run `just chdb-install` at the pinned " +
			"version; the pin runs in the `chdb` CI lane on CH 26.5.")
		return
	}
	native := runSubSecondRegressionEmit(t, db, subSecondDerivQuery, true)

	// The grid spans several anchors; the straddle only bites at the anchor whose
	// left window edge is second 0 — anchor 00:05:00, window (0s,300s], where the
	// +0.5s sample floors across the boundary. Assert on exactly that cell.
	cell := gridCell{ql: "a", anchor: "2026-01-01T00:05:00Z"}
	fv, okF := fanout[cell]
	nv, okN := native[cell]
	if !okF || !okN {
		t.Fatalf("straddle anchor cell %+v missing: fanout-present=%v native-present=%v", cell, okF, okN)
	}
	// true Prometheus = raw-ts membership + fractional-second x. Proven equal to
	// the native aggregate fed the raw DateTime64(9) axis and scaled to
	// per-second (per-nanosecond slope * 1e9). Computed directly so the reference
	// is not a hand-copied constant.
	truth := subSecondTrueDeriv(t, db)

	t.Logf("sub-second deriv three-way split at %s:", cell.anchor)
	t.Logf("  native (whole-second axis, floored membership) = %.20g", nv)
	t.Logf("  fan-out (raw membership, floored x)            = %.20g", fv)
	t.Logf("  true Prometheus (raw membership, fractional x) = %.20g", truth)

	// Invariant 1: the membership divergence is REAL, not float-order noise.
	if math.Abs(nv-fv) <= subSecondFloatCloseEps {
		t.Fatalf("native (%.20g) and fan-out (%.20g) agree within float noise (%g) — the "+
			"documented sub-second membership gap did NOT reproduce; either the fixture no "+
			"longer straddles a boundary or the native path changed its membership rule",
			nv, fv, subSecondFloatCloseEps)
	}

	// Invariant 2: the prod-default fan-out is STRICTLY closer to true Prometheus
	// than the default-off native path — native carries the gap, not the default.
	distNative := math.Abs(nv - truth)
	distFanout := math.Abs(fv - truth)
	if !(distFanout < distNative) {
		t.Fatalf("fan-out is NOT closer to true Prometheus than native: "+
			"|fanout-truth|=%.3g |native-truth|=%.3g — the pinned relationship (fan-out is "+
			"the more-correct path on sub-second data) no longer holds", distFanout, distNative)
	}
}

// subSecondPredictLinearQuery mirrors the deriv pin's frame for predict_linear
// (horizon 3600s, a whole-second literal so the native path is eligible).
const subSecondPredictLinearQuery = `sum by(host) (predict_linear(load_state[5m], 3600))`

// TestNativeTSGridPredictLinear_SubSecondMembershipPin locks that predict_linear
// carries the SAME whole-second membership gap as deriv. Unlike deriv there is
// no raw-ns "true Prometheus" reference to compare against — feeding the
// aggregate the raw nanosecond axis makes its ABSOLUTE forecast (intercept +
// slope*(anchor+offset), evaluated at ~1.77e18 ns) lose all precision to
// catastrophic cancellation (empirically ~6.6e11 for a true value near the
// hundreds). So this pin asserts the membership divergence between the two
// SHIPPING paths is real; the fan-out (raw membership) is the more-correct
// default, and the native path stays experimental/default-off.
func TestNativeTSGridPredictLinear_SubSecondMembershipPin(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(subSecondDerivSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runSubSecondRegressionEmit(t, db, subSecondPredictLinearQuery, false)
	if !predictLinearFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesPredictLinearToGrid absent on this local chDB substrate " +
			"— native membership pin bypassed (fan-out half still computed). Run " +
			"`just chdb-install` at the pinned version; the pin runs in the `chdb` CI lane.")
		return
	}
	native := runSubSecondRegressionEmit(t, db, subSecondPredictLinearQuery, true)

	cell := gridCell{ql: "a", anchor: "2026-01-01T00:05:00Z"}
	fv, okF := fanout[cell]
	nv, okN := native[cell]
	if !okF || !okN {
		t.Fatalf("straddle anchor cell %+v missing: fanout-present=%v native-present=%v", cell, okF, okN)
	}
	t.Logf("sub-second predict_linear at %s: native(floored membership)=%.20g fan-out(raw membership)=%.20g",
		cell.anchor, nv, fv)
	// The forecast projects 3600s ahead, so the dropped boundary sample moves the
	// slope and the forecast by far more than float noise.
	if math.Abs(nv-fv) <= subSecondFloatCloseEps {
		t.Fatalf("native (%.20g) and fan-out (%.20g) agree within float noise (%g) — the "+
			"predict_linear sub-second membership gap did NOT reproduce", nv, fv, subSecondFloatCloseEps)
	}
}

// runSubSecondRegressionEmit lowers + emits a deriv/predict_linear query against
// the sub-second seed over the 30s-step grid with the native strategy on/off and
// returns the per-cell values. When native, BOTH regression lowerers are wired
// (only the one matching the query's function fires), so one helper serves both
// pins.
func runSubSecondRegressionEmit(t *testing.T, db *sql.DB, query string, native bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
		lowerers.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, 30*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS host_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[gridCell]float64)
	for rows.Next() {
		var hostJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&hostJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[gridCell{ql: extractHostLabel(hostJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := tolerantSentinel(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the sub-second fixture must yield a populated grid", native)
	}
	return out
}

// subSecondTrueDeriv computes the reference Prometheus slope for the window
// (0s,300s] directly from the aggregate on the RAW nanosecond axis, scaled to
// per-second. Feeding raw DateTime64(9) makes the aggregate regress in
// per-nanosecond units (1e9x too small); multiplying by 1e9 recovers the
// per-second slope with FULL raw-ts membership and fractional-second x — i.e.
// exactly Prometheus's deriv. (The scale survives production ns magnitude
// ~1.77e18 because the least-squares slope is a centered difference, not an
// absolute-magnitude sum.)
func subSecondTrueDeriv(t *testing.T, db *sql.DB) float64 {
	t.Helper()
	const q = "SELECT arrayElement(" +
		"arrayMap(x -> x * 1e9, " +
		"timeSeriesDerivToGrid(toDateTime('2026-01-01 00:05:00'), toDateTime('2026-01-01 00:05:00'), 30, 300)" +
		"(TimeUnix, Value)), 1) " +
		"FROM otel_metrics_gauge WHERE MetricName = 'load_state'"
	var v sql.NullFloat64
	if err := db.QueryRow(q).Scan(&v); err != nil {
		t.Fatalf("true-deriv reference: %v", err)
	}
	if !v.Valid {
		t.Fatalf("true-deriv reference produced NULL — the window must contain >= 2 samples")
	}
	return v.Float64
}
