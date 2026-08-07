//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesChangesToGrid lowering (chplan.RangeWindowNative, Func="changes").
//
// The test lowers the SAME `sum by (host) (changes(load_state[5m]))`
// query_range expression TWICE against the SAME seed — once with the native
// changes strategy OFF (the arrayPopBack/arrayPopFront `c != p` count fan-out,
// RangeWindow) and once with it ON (the native timeSeriesChangesToGrid,
// RangeWindowNative) — runs BOTH on the same ephemeral chDB session, and
// compares the per-(series, anchor) change-count values.
//
// Why this is the parity proof. The fan-out's per-window count is the
// Prometheus-pinned funcChanges value (the spec corpus is reference-Prometheus-
// pinned), so native == fan-out transitively proves native == Prometheus on the
// changes shape. We compare the DECODED float64 (never a string render). Counts
// are exact integers in float64, so the assertion is BIT-IDENTICAL (no ULP
// tolerance, unlike the rate path's extrapolation arithmetic).
//
// Substrate: exercised on CI. timeSeriesChangesToGrid shipped in v25.9
// (PR #86010, floor-pinned 25.9 in internal/chopt). The chDB parity substrate is
// chdb-core v26.5.0 (CH 26.5.1.1, versions.yaml chdb_substrate), so the function
// is PRESENT and the native half fires in the `chdb` CI lane. The feature-detect
// guard below remains so the fan-out half still validates on an older local
// libchdb (a developer who has not run `just chdb-install` at the pinned
// version); the forbid-skip CI gate bans the test-skip API, so this is a
// documented runtime conditional that ALWAYS executes and never silently loses
// coverage.
//
// Semantic parity, exercised on the CI substrate: native timeSeriesChangesToGrid
// requires >= 1 sample/window and returns a 0 count for a single-sample window
// (NULL -> ABSENT only for an EMPTY window via WHERE grid_val IS NOT NULL),
// matching the fan-out's `length(window_vals) >= 1` + per-pair `c != p` count.
//
// #1489 (fixed): Prom's funcChanges carves out NaN-on-both-sides pairs (a pair
// only counts as a "change" when `c != p AND NOT (isNaN(c) AND isNaN(p))`).
// The fan-out kernel now implements that carve-out and is Prometheus-pinned
// by the spec corpus (test/spec/promql/changes_nan_run.txtar); the native
// timeSeriesChangesToGrid builtin does not, and cerberus can't patch a
// ClickHouse builtin from its own SQL emission. That is a real,
// evidence-backed fan-out/native divergence, characterized below rather than
// tolerated silently.
//
// #1707: the Prometheus-divergence carve-out above is exactly why this seed
// MUST exercise NaN input rather than avoid it. The host-c/host-d/host-e
// series below (an all-NaN run, a mixed NaN/finite run with transitions on
// both sides, and a single-sample window) mostly still feed the same
// bit-identical fan-out==native assertion below — no hand-derived expected
// count is needed for a cell whose window has no NaN-NaN adjacent pair,
// because the test only ever asserts the two kernels agree with each other,
// never a value pinned against Prometheus.
//
// #1721: native timeSeriesChangesToGrid diverges from the (now
// Prometheus-correct) fan-out on any window containing at least one NaN-NaN
// adjacent pair, by exactly `nanPairCount + leadNaNOvercount`:
//   - nanPairCount: native counts EVERY NaN-NaN adjacent pair as a change —
//     it implements no carve-out at all. This component was invisible before
//     #1489's fix, because the old, also-uncarved-out fan-out counted
//     NaN-NaN pairs the same way native does, masking the gap.
//   - leadNaNOvercount: an ADDITIONAL +1 whenever the window's
//     chronologically-earliest in-window sample is NaN — the narrower shape
//     originally pinned under #1721, before #1489's fix exposed the broader
//     nanPairCount gap it had been hiding.
//
// Both components are pinned per-cell and explicitly below — see
// changesKnownNativeCarveoutGap — rather than relaxed away, so the assertion
// stays honest about exactly what is and isn't known-divergent.
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
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// changesSeed mirrors the production OTel-CH default schema (ResourceAttributes
// present, DEFAULT map(), column-explicit INSERT). Two host series with an
// oscillating gauge so the change count is non-trivial and differs per series,
// on a clean 1-minute grid (off the staleness left edge is irrelevant for a
// COUNT, but the integer-minute samples keep the window membership unambiguous).
//
// Three further series (#1707) exercise the input classes the file-level
// comment documents as excluded from the Prometheus reference but NOT from
// the fan-out↔native self-consistency this test actually proves:
//   - host 'c': every sample in the window is NaN (the NaN-NaN pairs Prom's
//     funcChanges carves out).
//   - host 'd': NaN and finite samples alternate, exercising a NaN-to-finite
//     AND a finite-to-NaN transition within the same window.
//   - host 'e': exactly one sample lands inside the 5-minute seed span, so
//     every anchor whose window contains it sees a single-sample window (the
//     count=0, not-absent case the doc comment above already describes).
var changesSeed = metricsSeedDDL("otel_metrics_gauge") + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 0.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 1.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), 5.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:02:00', 9), 7.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:03:00', 9), 7.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:04:00', 9), 9.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:05:00', 9), 2.0),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:00:00', 9), nan),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:01:00', 9), nan),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:02:00', 9), nan),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:03:00', 9), nan),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:04:00', 9), nan),
    ('load_state', map('host', 'c'), toDateTime64('2026-01-01 00:05:00', 9), nan),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:01:00', 9), nan),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:02:00', 9), nan),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:03:00', 9), 3.0),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:04:00', 9), 4.0),
    ('load_state', map('host', 'd'), toDateTime64('2026-01-01 00:05:00', 9), nan),
    ('load_state', map('host', 'e'), toDateTime64('2026-01-01 00:03:00', 9), 1.0);
`

// changesQuery wraps the changes() matrix fn in a transparent `sum by` (each
// series is its own group), so the per-(series, anchor) change count is what
// both paths must agree on.
const changesQuery = `sum by(host) (changes(load_state[5m]))`

// changesKnownNativeCarveoutGap pins the exact native-minus-fan-out offset for
// every cell whose window contains at least one NaN-NaN adjacent pair (see
// #1721 above for the nanPairCount + leadNaNOvercount decomposition). Every
// offset here is hand-derived from the seed's own window membership — not an
// opaque "don't check this" entry — and every cell NOT listed here must still
// be bit-identical (see the assertion below). Reading
// internal/chsql/range_window_native.go confirms cerberus passes (ts, val)
// straight into the ClickHouse builtin with no NaN-specific pre/post
// processing anywhere in cerberus's own SQL — the gap lives inside the
// builtin itself, not in code this test's assertion could paper over.
//
// host 'c' is all-NaN, so every one of its 11 anchors (00:00:00..00:05:00 @
// 30s) has an all-NaN window: offset = nanPairCount(window) + 1 (its leading
// sample is always NaN too). host 'd' alternates finite/NaN
// (3.0, NaN, NaN, 3.0, 4.0, NaN at t=0..5m): exactly one NaN-NaN pair
// (t=00:01:00, t=00:02:00) is in-window from 00:02:00 onward, so those
// anchors get offset 1; at 00:05:00 the half-open (anchor-5m, anchor] left
// boundary additionally excludes host d's t=00:00:00 finite sample, promoting
// the still-in-window t=00:01:00 NaN sample to the window's leading position
// (the #1721 shape originally pinned) for a combined offset of 2. If a future
// ClickHouse release fixes the builtin, the dedicated assertion below will
// start failing (native no longer diverges) and this pin must be deleted, not
// widened.
var changesKnownNativeCarveoutGap = map[gridCell]int{
	{ql: "c", anchor: "2026-01-01T00:00:00Z"}: 1, // 0 nan-nan pairs (single-sample window) + 1 lead
	{ql: "c", anchor: "2026-01-01T00:00:30Z"}: 1, // 0 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:01:00Z"}: 2, // 1 pair + 1 lead
	{ql: "c", anchor: "2026-01-01T00:01:30Z"}: 2, // 1 pair + 1 lead
	{ql: "c", anchor: "2026-01-01T00:02:00Z"}: 3, // 2 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:02:30Z"}: 3, // 2 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:03:00Z"}: 4, // 3 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:03:30Z"}: 4, // 3 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:04:00Z"}: 5, // 4 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:04:30Z"}: 5, // 4 pairs + 1 lead
	{ql: "c", anchor: "2026-01-01T00:05:00Z"}: 5, // 4 pairs (t=0 excluded by left-open bound) + 1 lead (t=1 still NaN)
	{ql: "d", anchor: "2026-01-01T00:02:00Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:02:30Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:03:00Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:03:30Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:04:00Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:04:30Z"}: 1, // 1 nan-nan pair, finite lead
	{ql: "d", anchor: "2026-01-01T00:05:00Z"}: 2, // 1 nan-nan pair + 1 lead (t=0 excluded, t=1 NaN leads)
}

func TestNativeTSGridChanges_DualEmitParity(t *testing.T) {
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
	for _, stmt := range splitSeedStatements(changesSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runChangesEmit(t, db, false, false)
	if !changesFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesChangesToGrid absent on this local chDB substrate " +
			"(older libchdb than the pinned chdb-core v26.5.0) — native parity assertion " +
			"bypassed (fan-out half still validated). Run `just chdb-install` at the pinned " +
			"version to exercise the native half; it runs in the `chdb` CI lane on CH 26.5. " +
			"The always-on SQL-shape golden (native_changes_range_step.txtar) still pins the emit.")
		return
	}
	native := runChangesEmit(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan (ProjectionPushdown narrows the RangeWindowNative inner Scan to the
	// exact {Attributes, TimeUnix, Value} the emit reads).
	nativeOpt := runChangesEmit(t, db, true, true)
	if len(nativeOpt) != len(native) {
		t.Fatalf("optimized-native row-count divergence: opt=%d wide=%d cells", len(nativeOpt), len(native))
	}
	for cell, wv := range native {
		ov, ok := nativeOpt[cell]
		if !ok {
			t.Errorf("cell %+v present in wide native but absent in optimized native (a column was dropped)", cell)
			continue
		}
		if math.Float64bits(ov) != math.Float64bits(wv) {
			t.Errorf("cell %+v: optimized-native=%.20g wide-native=%.20g NOT bit-identical — "+
				"the scan narrowing changed a count (the narrowing is WRONG)", cell, ov, wv)
		}
	}

	if len(native) != len(fanout) {
		t.Fatalf("row-count divergence: native=%d fanout=%d cells\nnative=%v\nfanout=%v",
			len(native), len(fanout), native, fanout)
	}
	identical := 0
	for cell, fv := range fanout {
		nv, ok := native[cell]
		if !ok {
			t.Errorf("cell %+v present in fan-out but absent in native", cell)
			continue
		}
		if offset, known := changesKnownNativeCarveoutGap[cell]; known {
			// #1721: pinned nanPairCount+leadNaNOvercount gap. Assert the
			// EXACT known offset, not merely "don't check" — any other
			// relationship means the gap changed shape and this pin is stale.
			want := fv + float64(offset)
			if math.Float64bits(nv) != math.Float64bits(want) {
				t.Errorf("cell %+v: expected the pinned #1721 carve-out gap native=fanout+%d "+
					"(fanout=%.20g so native should be %.20g), got native=%.20g — the known "+
					"divergence shape changed; update or remove this pin", cell, offset, fv, want, nv)
			}
			continue
		}
		// changes() is an integer COUNT — native and fan-out must be
		// BIT-IDENTICAL (no ULP tolerance).
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native change count diverged from the fan-out", cell, nv, fv)
		}
		identical++
	}
	t.Logf("changes dual-emit parity: %d/%d cells bit-identical, %d cells hold the pinned "+
		"#1721 NaN-carve-out gap. native == fan-out == Prometheus everywhere else.",
		identical, len(fanout), len(changesKnownNativeCarveoutGap))
}

// runChangesEmit lowers + emits the changes query with the native-changes
// strategy set to `native`, optionally runs the default optimizer pipeline,
// runs the resulting SQL on db, and returns the per-cell change counts.
func runChangesEmit(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(changesQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, 30*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	if optimize {
		plan = optimizer.Default().Run(context.Background(), plan)
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
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the changes fixture must yield a populated grid", native)
	}
	return out
}

// changesFnPresent feature-detects timeSeriesChangesToGrid via system.functions
// (the gating fact the native changes path depends on; ABSENT below CH 25.9).
func changesFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesChangesToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesChangesToGrid: %v", err)
	}
	return n > 0
}

// extractHostLabel pulls the host value out of the JSON-encoded Attributes map
// (`{"host":"a"}`), reusing the indexOf helper from the rate dual-emit test.
func extractHostLabel(jsonStr string) string {
	const key = `"host":"`
	i := indexOf(jsonStr, key)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(key):]
	j := indexOf(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}
