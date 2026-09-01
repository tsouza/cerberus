//go:build integration

// Real-ClickHouse verification of chopt.FeatureTSGridGroupArray's three hard
// preconditions (cerberus issue #2749) — see that registry entry's own doc
// for the full empirical writeup this file pins.
//
//  1. Timestamp type: timeSeriesGroupArray(DateTime64(9), Float64) is
//     accepted directly and losslessly, contradicting the function's own
//     documented UInt32/DateTime/UInt64 signature (TestTimeSeriesGroupArray_
//     AcceptsDateTime64Losslessly_RealCH).
//  2. Duplicate-timestamp dedup: the native aggregate collapses a
//     duplicate-timestamp pair to the max-valued sample, insertion-order
//     INDEPENDENT for finite values — proven end to end through cerberus's
//     own lowering + emitter, matching the fan-out's own dedup exactly
//     (TestRate_NativeGroupArray_DuplicateTimestamp_MatchesFanoutRealCH).
//  3. NaN edge case: unlike the finite case, a duplicate timestamp carrying a
//     NaN sample is insertion-order DEPENDENT — the reason
//     chopt.FeatureTSGridGroupArray ships AutoSelect: false
//     (TestTimeSeriesGroupArray_NaNDuplicateIsInsertionOrderDependent_RealCH).
//
// Cerberus issue #2862 extends the same feature to the split-window
// assembly sites via ClickHouse's generic `-If` aggregate combinator
// (timeSeriesGroupArrayIf, nativeGroupArrayPairIfFrag). Combinator wrapping
// could plausibly change any of the three, so each is RE-RUN for that form
// rather than assumed to carry over — TestTimeSeriesGroupArray_IfCombinator*
// below, one per precondition, plus the combinator's own additional
// question (does the predicate filter BEFORE or AFTER the
// duplicate-timestamp collapse?) which the plain form cannot pose.
//
// Needs a real ClickHouse >= 25.9 (chopt.FeatureTSGridGroupArray's own
// floor, shared with the rest of the timeSeries*ToGrid family) — this lane
// pins CH_TEST_IMAGE (see the ts-grid-group-array-integration Justfile
// recipe) exactly at that floor rather than reusing CH_STRICT_SCAN_IMAGE's
// higher 26.6 pin, since nothing here depends on a fix that landed above
// 25.9. Requires Docker; gated behind the `integration` build tag, mirroring
// TestLastOverTime_NativeResample_WindowNarrowerThanStep_RealCH's own
// posture for chopt.FeatureTSGridLastOverTime's floor pin.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	promparser "github.com/prometheus/prometheus/promql/parser"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// tsGridGroupArrayImage pins a ClickHouse version at or above
// chopt.FeatureTSGridGroupArray's 25.9 floor (see that registry entry). A
// plain literal rather than an import of internal/chopt, mirroring
// tsGridLastOverTimeImage's own rationale in the sibling integration test —
// this test's only dependency on the feature is the version its own
// testcontainers image is pinned to.
const tsGridGroupArrayImage = "clickhouse/clickhouse-server:25.9-alpine"

func realCHConnect(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		tsGridGroupArrayImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
	})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

// TestTimeSeriesGroupArray_AcceptsDateTime64Losslessly_RealCH pins
// precondition 1: contrary to timeSeriesGroupArray's own documented
// UInt32/DateTime/UInt64 signature, a DateTime64(9) timestamp column is
// accepted directly, and full nanosecond precision round-trips — no
// toUnixTimestamp64Nano wrap-and-rebuild pass is needed. UInt64 is asserted
// REJECTED for contrast: the documented alternative is not even reachable
// as a fallback shape.
func TestTimeSeriesGroupArray_AcceptsDateTime64Losslessly_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	const settings = " SETTINGS " + chclient.SettingExperimentalTSGridAggregate + " = 1"

	var typeName string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toTypeName(timeSeriesGroupArray(t, v)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01.123456789', 9) AS t, 1.0 AS v)"+settings,
	).Scan(&typeName); err != nil {
		t.Fatalf("DateTime64(9) probe: %v", err)
	}
	const wantType = "Array(Tuple(DateTime64(9), Float64))"
	if typeName != wantType {
		t.Errorf("timeSeriesGroupArray(DateTime64(9), Float64) result type = %q, want %q", typeName, wantType)
	}

	var roundTripped string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArray(t, v)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01.123456789', 9) AS t, 1.0 AS v)"+settings,
	).Scan(&roundTripped); err != nil {
		t.Fatalf("precision round-trip probe: %v", err)
	}
	if !strings.Contains(roundTripped, "00:00:01.123456789") {
		t.Errorf("sub-second precision lost: got %q, want it to contain 00:00:01.123456789", roundTripped)
	}

	var uint64Err error
	err := db.QueryRowContext(
		ctx,
		"SELECT timeSeriesGroupArray(t, v) FROM "+
			"(SELECT toUInt64(1767225601123456789) AS t, 1.0 AS v)"+settings,
	).Scan(&uint64Err)
	if err == nil {
		t.Fatal("timeSeriesGroupArray(UInt64, Float64) unexpectedly succeeded — the documented UInt64 " +
			"alternative was expected to be rejected (ILLEGAL_TYPE_OF_ARGUMENT) on this server")
	}
}

// TestTimeSeriesGroupArray_NaNDuplicateIsInsertionOrderDependent_RealCH pins
// precondition 3's NaN finding: the native aggregate's duplicate-timestamp
// collapse is a running "replace only when candidate > current-best" fold.
// IEEE754 makes every comparison against NaN false, so whichever value a
// (possibly multi-threaded, multi-part) scan visits FIRST at a duplicate
// timestamp survives when it is NaN, and can never be dislodged — while a
// NaN visited SECOND can never displace a non-NaN first value. This is the
// reason chopt.FeatureTSGridGroupArray ships AutoSelect: false: the fan-out's
// own dedupWindowPairsByTsFrag is deterministic here (arraySort ranks NaN
// greatest, so it always survives regardless of insertion order).
func TestTimeSeriesGroupArray_NaNDuplicateIsInsertionOrderDependent_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	const settings = " SETTINGS " + chclient.SettingExperimentalTSGridAggregate + " = 1"

	// NaN listed FIRST survives — nothing compares greater than NaN.
	var nanFirst string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArray(t, v)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, nan AS v "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), 3.0)"+settings,
	).Scan(&nanFirst); err != nil {
		t.Fatalf("nan-first probe: %v", err)
	}
	if !strings.Contains(strings.ToLower(nanFirst), "nan") {
		t.Errorf("NaN-first duplicate: got %q, want the surviving sample to be nan (nothing compares > nan)", nanFirst)
	}

	// The SAME two candidates, NaN listed SECOND: the finite value survives
	// instead — proving the outcome depends on encounter order, not on the
	// values alone.
	var nanSecond string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArray(t, v)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, 3.0 AS v "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), nan)"+settings,
	).Scan(&nanSecond); err != nil {
		t.Fatalf("nan-second probe: %v", err)
	}
	if strings.Contains(strings.ToLower(nanSecond), "nan") {
		t.Errorf("NaN-second duplicate: got %q, want the surviving sample to be 3 (nan never displaces a finite current-best)", nanSecond)
	}
}

// TestTimeSeriesGroupArray_IfCombinatorAcceptsDateTime64Losslessly_RealCH
// re-runs precondition 1 for the `-If` combinator form (cerberus issue
// #2862): wrapping the aggregate in `-If` must not change which timestamp
// types it accepts, nor cost sub-second precision. UInt64 is asserted
// REJECTED for the same contrast the plain form's probe draws — a
// combinator that RELAXED the argument types would be a different function
// than the one nativeGroupArrayPairIfFrag's doc describes.
func TestTimeSeriesGroupArray_IfCombinatorAcceptsDateTime64Losslessly_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	const settings = " SETTINGS " + chclient.SettingExperimentalTSGridAggregate + " = 1"

	var typeName string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toTypeName(timeSeriesGroupArrayIf(t, v, v > 0)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01.123456789', 9) AS t, 1.0 AS v)"+settings,
	).Scan(&typeName); err != nil {
		t.Fatalf("DateTime64(9) -If probe: %v", err)
	}
	const wantType = "Array(Tuple(DateTime64(9), Float64))"
	if typeName != wantType {
		t.Errorf("timeSeriesGroupArrayIf(DateTime64(9), Float64, UInt8) result type = %q, want %q", typeName, wantType)
	}

	var roundTripped string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArrayIf(t, v, v > 0)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01.123456789', 9) AS t, 1.0 AS v)"+settings,
	).Scan(&roundTripped); err != nil {
		t.Fatalf("precision round-trip -If probe: %v", err)
	}
	if !strings.Contains(roundTripped, "00:00:01.123456789") {
		t.Errorf("sub-second precision lost under -If: got %q, want it to contain 00:00:01.123456789", roundTripped)
	}

	var uint64Err error
	err := db.QueryRowContext(
		ctx,
		"SELECT timeSeriesGroupArrayIf(t, v, v > 0) FROM "+
			"(SELECT toUInt64(1767225601123456789) AS t, 1.0 AS v)"+settings,
	).Scan(&uint64Err)
	if err == nil {
		t.Fatal("timeSeriesGroupArrayIf(UInt64, Float64, UInt8) unexpectedly succeeded — the documented UInt64 " +
			"alternative was expected to be rejected (ILLEGAL_TYPE_OF_ARGUMENT) on this server, exactly as for " +
			"the plain form")
	}
}

// TestTimeSeriesGroupArray_IfCombinatorFiltersBeforeCollapse_RealCH re-runs
// precondition 2 for the `-If` combinator form (cerberus issue #2862), and
// asks the one question only the combinator form can pose: WHERE in the fold
// the predicate is applied.
//
// The two orderings are behaviourally different and both are a priori
// plausible. Filter-then-collapse (what an `-If` combinator is documented to
// do: it gates whether a row is fed to the aggregate at all) means a row
// failing cond is invisible, so the survivor at a duplicate timestamp is the
// max among PASSING rows. Collapse-then-filter would let a larger, EXCLUDED
// row win the timestamp and then be dropped, silently deleting the passing
// sample from the array. seriesArrayPairIfFrag's two complementary
// predicates split one scan into `window_pairs` and `delta_prefix_pairs`, so
// the second ordering would lose real samples from both arms wherever a
// timestamp is duplicated across the split boundary.
//
// The probe seeds a duplicate timestamp whose LARGER value fails cond and
// whose smaller value passes: filter-then-collapse yields the smaller,
// passing value; collapse-then-filter would yield an empty array. A second
// probe pins the max-collapse among two PASSING rows, so the first probe
// cannot pass merely because the aggregate ignores values entirely.
func TestTimeSeriesGroupArray_IfCombinatorFiltersBeforeCollapse_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	const settings = " SETTINGS " + chclient.SettingExperimentalTSGridAggregate + " = 1"

	// The excluded row carries the LARGER value, so a collapse that ran
	// before the filter would elect it and then drop it.
	var excludedLoses string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArrayIf(t, v, keep)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, 99.0 AS v, 0 AS keep "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), 3.0, 1)"+settings,
	).Scan(&excludedLoses); err != nil {
		t.Fatalf("filter-before-collapse probe: %v", err)
	}
	if !strings.Contains(excludedLoses, "3") || strings.Contains(excludedLoses, "99") {
		t.Errorf(
			"timeSeriesGroupArrayIf collapsed a duplicate timestamp across the predicate: got %q, want the "+
				"single passing sample (value 3). A cond-failing row must never take the timestamp from a "+
				"cond-passing one — seriesArrayPairIfFrag's two complementary predicates would lose samples "+
				"from both split arms.",
			excludedLoses,
		)
	}

	// Among rows that DO pass, the collapse is the same max-valued fold the
	// plain form performs — which is what makes dropping
	// dedupWindowPairsByTsFrag downstream of this aggregate answer-preserving.
	var maxAmongPassing string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArrayIf(t, v, keep)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, 3.0 AS v, 1 AS keep "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), 7.0, 1)"+settings,
	).Scan(&maxAmongPassing); err != nil {
		t.Fatalf("max-among-passing probe: %v", err)
	}
	if !strings.Contains(maxAmongPassing, "7") || strings.Contains(maxAmongPassing, "3") {
		t.Errorf(
			"timeSeriesGroupArrayIf duplicate collapse among passing rows: got %q, want the max-valued "+
				"sample (value 7), matching the plain form's own fold",
			maxAmongPassing,
		)
	}
}

// TestTimeSeriesGroupArray_IfCombinatorNaNDuplicateIsInsertionOrderDependent_RealCH
// re-runs precondition 3 for the `-If` combinator form (cerberus issue
// #2862). The finding is the plain form's, unchanged: because every IEEE754
// comparison against NaN is false, the running "replace only when candidate
// > current-best" fold keeps whichever NaN it visits FIRST and never lets a
// NaN visited SECOND dislodge a finite current-best. Pinning it here is what
// makes chopt.FeatureTSGridGroupArray's AutoSelect: false posture cover the
// split-window sites too, rather than resting on the assumption that a
// combinator cannot change a fold's comparison.
func TestTimeSeriesGroupArray_IfCombinatorNaNDuplicateIsInsertionOrderDependent_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	const settings = " SETTINGS " + chclient.SettingExperimentalTSGridAggregate + " = 1"

	// NaN listed FIRST survives — nothing compares greater than NaN. Both
	// rows pass cond, so the predicate is not what decides the outcome.
	var nanFirst string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArrayIf(t, v, keep)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, nan AS v, 1 AS keep "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), 3.0, 1)"+settings,
	).Scan(&nanFirst); err != nil {
		t.Fatalf("nan-first -If probe: %v", err)
	}
	if !strings.Contains(strings.ToLower(nanFirst), "nan") {
		t.Errorf("NaN-first duplicate under -If: got %q, want the surviving sample to be nan", nanFirst)
	}

	// The SAME two candidates, NaN listed SECOND: the finite value survives.
	var nanSecond string
	if err := db.QueryRowContext(
		ctx,
		"SELECT toString(timeSeriesGroupArrayIf(t, v, keep)) FROM "+
			"(SELECT toDateTime64('2026-01-01 00:00:01', 9) AS t, 3.0 AS v, 1 AS keep "+
			"UNION ALL SELECT toDateTime64('2026-01-01 00:00:01', 9), nan, 1)"+settings,
	).Scan(&nanSecond); err != nil {
		t.Fatalf("nan-second -If probe: %v", err)
	}
	if strings.Contains(strings.ToLower(nanSecond), "nan") {
		t.Errorf(
			"NaN-second duplicate under -If: got %q, want the surviving sample to be 3 (nan never displaces "+
				"a finite current-best)",
			nanSecond,
		)
	}
}

// TestRate_NativeGroupArray_DuplicateTimestamp_MatchesFanoutRealCH proves,
// end to end through cerberus's own lowering + emitter, that swapping
// rate()'s array-fold assembly to the native timeSeriesGroupArray aggregate
// (chplan.RangeWindow.NativeGroupArray) produces the IDENTICAL extrapolated
// rate as the unchanged fan-out, including on a series carrying a
// duplicate, non-NaN timestamp — the shape dedupWindowPairsByTsFrag exists
// to collapse today. Series "dup" duplicates a sample; series "plain" has
// no duplicates, so the test cannot pass merely because the dedup happened
// to be a no-op everywhere.
func TestRate_NativeGroupArray_DuplicateTimestamp_MatchesFanoutRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := realCHConnect(ctx, t)

	if _, err := db.ExecContext(ctx, `
CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)
`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:01:00', 9), 3.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:02:00', 9), 7.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:03:00', 9), 9.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:04:00', 9), 11.0),
    ('http_requests_total', map('job', 'dup'), toDateTime64('2026-01-01 00:05:00', 9), 13.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:01:00', 9), 20.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('http_requests_total', map('job', 'plain'), toDateTime64('2026-01-01 00:05:00', 9), 60.0)
`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	const step = 30 * time.Second

	fanout := runRateNativeGroupArrayRealCH(ctx, t, db, rangeStart, rangeEnd, step, false)
	native := runRateNativeGroupArrayRealCH(ctx, t, db, rangeStart, rangeEnd, step, true)

	for _, job := range []string{"dup", "plain"} {
		fv, nv := fanout[job], native[job]
		if len(fv) == 0 {
			t.Fatalf("job=%s: fan-out returned zero rows — fixture is broken", job)
		}
		if len(nv) != len(fv) {
			t.Fatalf("job=%s: row-count divergence: native=%d fanout=%d", job, len(nv), len(fv))
		}
		for anchor, f := range fv {
			n, ok := nv[anchor]
			if !ok {
				t.Errorf("job=%s anchor=%s present in fan-out but absent in native", job, anchor)
				continue
			}
			if math.Abs(n-f) > 1e-9 {
				t.Errorf("job=%s anchor=%s: native=%v fanout=%v NOT equal", job, anchor, n, f)
			}
		}
	}
}

// runRateNativeGroupArrayRealCH lowers + emits `rate(http_requests_total[5m])`
// over [start, end] at step with RangeLowerers.NativeGroupArray set per
// native, runs the resulting SQL on db (with the experimental setting
// scoped to this one query via a SETTINGS clause — a raw *sql.DB connection
// pool does not guarantee a session-level SET survives to the connection a
// later query reuses), and returns the per-job, per-anchor rate.
func runRateNativeGroupArrayRealCH(ctx context.Context, t *testing.T, db *sql.DB, start, end time.Time, step time.Duration, native bool) map[string]map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(`rate(http_requests_total[5m])`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	lowerers.NativeGroupArray = native
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, end, step,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := fmt.Sprintf(
		"SELECT toJSONString(`Attributes`) AS job_json, `TimeUnix`, `Value` FROM (%s) SETTINGS %s = 1",
		sqlStr, chclient.SettingExperimentalTSGridAggregate,
	)
	rows, err := db.QueryContext(ctx, wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]map[string]float64{"dup": {}, "plain": {}}
	for rows.Next() {
		var jobJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&jobJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		job := realCHGroupArrayExtractJobLabel(jobJSON)
		if out[job] == nil {
			out[job] = map[string]float64{}
		}
		out[job][ts.UTC().Format(time.RFC3339)] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	return out
}

// realCHGroupArrayExtractJobLabel pulls the job value out of the
// JSON-encoded Attributes map (`{"job":"a"}`). A local copy rather than a
// shared helper, mirroring realCHExtractHostLabel's own rationale: the
// chdb-tagged extractHostLabel helper is not compiled into an
// integration-only build.
func realCHGroupArrayExtractJobLabel(jsonStr string) string {
	const key = `"job":"`
	i := strings.Index(jsonStr, key)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}
