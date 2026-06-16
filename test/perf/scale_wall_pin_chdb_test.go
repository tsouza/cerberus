//go:build chdb

// The SCALE-WALL pin: the perf guard for the regression classes the
// cardinality ratchet is structurally BLIND to.
//
// # Why this exists (the gap the cardinality ratchet leaves open)
//
// cardinality_ratchet_test.go pins fan_factor = peak_intermediate/scan_rows
// over the TXTAR corpus — a DETERMINISTIC structural signal that
// DELIBERATELY EXCLUDES timing as "environment-noisy". That blind spot let
// two real, separately-catalogued perf regressions through:
//
//	(a) #97 (ASOF rate path): fan_factor DROPPED to 1.0 — cardinality fell —
//	    while wall time went 6.31s -> 36.90s. A CPU-bound blow-up with FLAT
//	    or FALLING cardinality is invisible to a cardinality-only ratchet.
//	(b) the anchor-grid sharding regression: per-statement cardinality stayed
//	    flat, but the query read 8x the rows (40M vs 5M). A scan-amplification
//	    that holds the FINAL result cardinality constant is likewise invisible
//	    to a peak_intermediate/scan_rows ratio measured on tiny fixtures.
//
// This pin closes both gaps with a representative OOM-class query — a
// `sum(rate(http_requests_total[5m]))` query_range over a counter table
// seeded at scale — run through the REAL pipeline (parse -> lower ->
// optimizer.Default().Run -> emit -> chDB exec). It asserts two invariants,
// each targeting one of the two regression classes above:
//
//	PRONG 1 — deterministic total-work / scan-amplification pin (ZERO
//	tolerance, catches the SHARDING class). Decomposes the EMITTED SQL into
//	its FROM-source pipeline levels (via profile.FromSourceLevels — the SAME
//	decomposition the corpus ratchet uses) and counts each over the
//	seeded-at-scale table. Asserts the PEAK intermediate cardinality stays
//	<= scanAmplificationBound x scan_rows. An 8x scan amplification (more
//	rows read at flat result cardinality) blows straight past the bound and
//	fails immediately. This is the PRIMARY guard: pure row counts, fully
//	deterministic, reproduces run-to-run.
//
//	PRONG 2 — ratio-based wall pin (catches the GROSS-WALL class like #97's
//	6x). Times the real query best-of-N (the floor strips GC / scheduler
//	jitter) and divides by an IN-RUN yardstick measured in the SAME process:
//	a bare groupArray/arraySort window-pairing aggregate over the SAME scan
//	that does comparable per-row array machinery but carries NO anchor
//	fan-out. Because both timings ride the same chDB engine on the same
//	machine in the same process, machine speed AND the CGO/chDB call
//	overhead CANCEL in the ratio — the gate is a ratio-of-walls with NO
//	absolute-ms threshold, so it does not flap across CI runners. The
//	committed bound (wallRatioBound) has ~2.5x headroom over the measured
//	floor; a #97-class 6x CPU regression pushes the ratio ~6x past its
//	baseline and trips it.
//
// # Robustness (a flaky perf gate is worse than none)
//
// The bounds below were calibrated on the chDB runner: over 36 measurements
// (3 separate processes x 12 trials, best-of-5 each) the wall ratio stayed
// in 4.2-5.8 (max 5.77), spread <=1.39x within a process. wallRatioBound is
// set generously above that envelope so 2-3x CI variance passes but a 6x
// regression FAILS. PRONG 1 is deterministic and is the primary guard;
// PRONG 2 adds the CPU-bound coverage PRONG 1 cannot see, with a bound wide
// enough that it never flaps. Both bounds live in scale-wall-baseline.json;
// `just update-scale-wall-baseline` regenerates them (mirrors
// `just update-cardinality-baseline`).
//
// Build-tagged chdb; rides the already-required `perf-guards` job
// (`just perf-chdb` = `go test -tags chdb ./test/perf/...`).
package perf

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/perf/profile"
)

// scaleWallBaselinePath is the committed bound file, relative to this package.
const scaleWallBaselinePath = "scale-wall-baseline.json"

// scaleWallUpdateEnv, when "1", rewrites the baseline from the current
// measurement instead of asserting against it. Driven by
// `just update-scale-wall-baseline`; mirrors UPDATE_CARDINALITY_BASELINE.
const scaleWallUpdateEnv = "UPDATE_SCALE_WALL_BASELINE"

// --- seeded-at-scale fixture parameters --------------------------------
//
// A counter table large enough to expose a 6x wall regression / 8x scan
// amplification, but capped so the perf-guards job stays fast (the seed +
// both prongs run in ~1s on the chDB runner). 300k rows over 300 series is
// ~1000 samples/series across 1h — plenty of per-window samples for the
// rate machinery to do real work, plenty of total rows for an amplification
// to show.
const (
	scaleSeedRows     = 300_000
	scaleSeedSeries   = 300
	scaleSampleStepS  = 15 // seconds between samples per series
	scaleRateMetric   = "http_requests_total"
	scaleQueryRangeQL = "sum(rate(" + scaleRateMetric + "[5m]))"
	// best-of-N: the floor strips GC / scheduler jitter. N=9 (not the
	// scaling harness's 5) because under whole-process load a single GC
	// pause can occasionally land on a best-of-5 minimum and inflate the
	// numerator — the floor over 9 trials is far harder to perturb, which
	// tightens PRONG 2's ratio spread (verified: max observed ratio drops
	// well back under the calibration envelope).
	scaleWallIters = 9
)

// scaleWallBaseline is the committed bound pair. Kept tiny + JSON so the
// diff is a one-line review when a bound is deliberately moved.
type scaleWallBaseline struct {
	// ScanAmplificationBound is the max PEAK intermediate cardinality, as a
	// multiple of scan_rows, PRONG 1 tolerates. The seeded rate query's
	// bounded anchor fan-out measures ~4.6x scan_rows on main; the committed
	// bound carries headroom over that while an 8x scan-amplification
	// regression blows past it. ZERO tolerance above this number.
	ScanAmplificationBound float64 `json:"scan_amplification_bound"`

	// WallRatioBound is the max (query wall / yardstick wall) ratio PRONG 2
	// tolerates. Calibrated floor ~5; bound carries ~2.5x headroom so CI
	// variance passes but a #97-class 6x CPU regression fails. A ratio, so
	// machine speed cancels and it does not flap across runners.
	WallRatioBound float64 `json:"wall_ratio_bound"`
}

func TestScaleWallPin(t *testing.T) {
	db := openScaleWallDB(t)
	scanRows := seedCounterAtScale(t, db)

	sqlText, args := emitRateRangeSQL(t)

	// --- PRONG 1: deterministic scan-amplification / total-work pin -------
	//
	// Decompose the emitted SQL into its pipeline levels and count each over
	// the seeded table. The MAX is the peak intermediate cardinality — the
	// widest the row set gets anywhere in the pipeline. A scan amplification
	// (more rows pulled through at flat result cardinality) shows up here as
	// a peak that overshoots scan_rows x bound.
	levels := profile.FromSourceLevels(stripTrailingScaleSemi(sqlText))
	var peak int64
	for _, lvl := range levels {
		c, ok := countLevel(t, db, lvl, args)
		if !ok {
			// A level that references a name only in scope at an outer level
			// can't be counted standalone — the corpus profiler excludes
			// these too. The outer level (depth 0) and the leaf scan still
			// anchor the decomposition.
			continue
		}
		if c > peak {
			peak = c
		}
	}
	if peak == 0 {
		t.Fatalf("PRONG 1: no countable pipeline level — the decomposition is broken, the pin would silently pass")
	}
	amplification := float64(peak) / float64(scanRows)
	t.Logf("PRONG 1 (deterministic): peak_intermediate=%d = %.2fx scan_rows(%d)", peak, amplification, scanRows)

	// --- PRONG 2: ratio-based wall pin -----------------------------------
	//
	// best-of-N wall of the real query / best-of-N wall of the in-run
	// yardstick. Both ride the same engine in the same process, so machine
	// speed + CGO overhead cancel — the ratio is portable.
	queryWall := bestOfWall(t, db, "SELECT count() FROM ("+stripTrailingScaleSemi(sqlText)+")", args, scaleWallIters)
	yardWall := bestOfWall(t, db, yardstickSQL(), nil, scaleWallIters)
	wallRatio := float64(queryWall) / float64(yardWall)
	t.Logf("PRONG 2 (wall ratio): query=%v yard=%v ratio=%.2f",
		queryWall.Round(time.Microsecond), yardWall.Round(time.Microsecond), wallRatio)

	if os.Getenv(scaleWallUpdateEnv) == "1" {
		writeScaleWallBaseline(t, scaleWallBaseline{
			// Round the measured values UP to a stable, generous bound so the
			// committed number is a deliberate ceiling, not a brittle
			// transcription of one run's float. The recipe prints the diff
			// for review.
			ScanAmplificationBound: ceilTo(amplification*scanAmplificationHeadroom, 0.5),
			WallRatioBound:         ceilTo(wallRatio*wallRatioHeadroom, 1.0),
		})
		t.Logf("wrote %s (amplification=%.2f -> bound %.2f, wall_ratio=%.2f -> bound %.2f)",
			scaleWallBaselinePath, amplification, ceilTo(amplification*scanAmplificationHeadroom, 0.5),
			wallRatio, ceilTo(wallRatio*wallRatioHeadroom, 1.0))
		return
	}

	base := loadScaleWallBaseline(t)

	if amplification > base.ScanAmplificationBound {
		t.Errorf("PRONG 1 FAIL (scan-amplification): peak intermediate cardinality is %.2fx scan_rows "+
			"(%d / %d), over the committed %.2fx bound. The pipeline is pulling more rows through than the "+
			"seeded scan justifies — the anchor-grid SHARDING class (8x scan amplification at flat result "+
			"cardinality). Root-cause the extra rows; only run `just update-scale-wall-baseline` if the "+
			"increase is genuinely intended.", amplification, peak, scanRows, base.ScanAmplificationBound)
	}

	if wallRatio > base.WallRatioBound {
		t.Errorf("PRONG 2 FAIL (wall ratio): the rate query took %.2fx the yardstick's wall (query=%v, "+
			"yard=%v), over the committed %.2fx bound. The query got CPU-bound-slower relative to a "+
			"same-scan reference that did comparable array machinery — the #97 class (wall 6x UP while "+
			"cardinality fell). Because this is a ratio measured in-process, machine speed cancels, so a "+
			"breach is a real compute regression, not runner noise. Profile the rate path; only run "+
			"`just update-scale-wall-baseline` if the cost is genuinely intended.",
			wallRatio, queryWall.Round(time.Microsecond), yardWall.Round(time.Microsecond), base.WallRatioBound)
	}
}

// scanAmplificationHeadroom / wallRatioHeadroom are the multipliers the
// update recipe applies to a fresh measurement to derive the committed
// bound. The headroom must clear normal runner variance (PRONG 2's ratio
// spread was <=1.39x within a process across the calibration runs) while
// staying comfortably under the regression each prong targets (8x scan
// amplification / 6x wall). 1.5x on the deterministic amplification (it
// barely varies) and 2.5x on the noisier wall ratio.
const (
	scanAmplificationHeadroom = 1.5
	wallRatioHeadroom         = 2.5
)

// openScaleWallDB opens an in-process chDB database/sql handle, pinged.
func openScaleWallDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	return db
}

// seedCounterAtScale builds the fixed-scale counter table and returns its
// scan-row count (the PRONG-1 denominator). scaleSeedSeries instances each
// carry a monotonically rising counter sampled every scaleSampleStepS.
func seedCounterAtScale(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	stmts := []string{
		`DROP TABLE IF EXISTS otel_metrics_sum`,
		// ResourceAttributes mirrors the OTel-CH default schema: the rc.5
		// read path projects mapUpdate(sanitize(ResourceAttributes), …), so
		// the seed table must carry the column (left empty via DEFAULT) or
		// the chDB round-trip 502s with UNKNOWN_IDENTIFIER.
		`CREATE TABLE otel_metrics_sum (
		  MetricName String, Attributes Map(String,String),
		  ResourceAttributes Map(String,String) DEFAULT map(),
		  TimeUnix DateTime64(9), Value Float64
		) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix)`,
		`INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) SELECT
		  '` + scaleRateMetric + `',
		  map('instance', concat('i', toString(number % ` + itoa(scaleSeedSeries) + `))),
		  toDateTime64('2026-01-01 00:00:00', 9)
		    + toIntervalSecond((intDiv(number, ` + itoa(scaleSeedSeries) + `)) * ` + itoa(scaleSampleStepS) + `),
		  toFloat64(number)
		FROM numbers(` + itoa(scaleSeedRows) + `)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	var scanRows int64
	if err := db.QueryRow(`SELECT count() FROM otel_metrics_sum`).Scan(&scanRows); err != nil {
		t.Fatalf("scan-row count: %v", err)
	}
	if scanRows <= 0 {
		t.Fatalf("seed produced no rows — the pin is mis-seeded")
	}
	return scanRows
}

// emitRateRangeSQL lowers the representative query_range query through the
// REAL parse -> lower -> optimizer.Default().Run -> emit chain (the same
// chain the HTTP path runs), so the SQL the pin times is the production
// shape. The eval grid is 1h at scaleSampleStepS step.
func emitRateRangeSQL(t *testing.T) (string, []any) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(1 * time.Hour)
	step := time.Duration(scaleSampleStepS) * time.Second

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(scaleQueryRangeQL)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText, args
}

// yardstickSQL is the in-run PRONG-2 reference: a bare groupArray/arraySort
// window-pairing aggregate over the SAME counter scan. It exercises the same
// array machinery (groupArray of (TimeUnix, Value) tuples + arraySort per
// series) the rate query's inner window stage does, but carries NO anchor
// fan-out — so a #97-style fan-out / CPU regression in the rate path widens
// the (query / yardstick) ratio without moving the yardstick. Bare SQL is
// acceptable here: it is a test-local timing reference, NOT part of the
// lowering path under test (which goes through the typed chsql emitter).
func yardstickSQL() string {
	return `SELECT max(length(window_pairs)) FROM (
	  SELECT Attributes, arraySort(groupArray((TimeUnix, Value))) AS window_pairs
	  FROM otel_metrics_sum WHERE MetricName = '` + scaleRateMetric + `'
	  GROUP BY Attributes)`
}

// countLevel runs `count() FROM (<level>)` and returns the count. ok=false
// when the level can't be counted standalone (e.g. it references an
// out-of-scope CTE name) — the caller skips it, exactly as the corpus
// profiler does.
func countLevel(t *testing.T, db *sql.DB, level string, args []any) (int64, bool) {
	t.Helper()
	var c int64
	if err := db.QueryRow("SELECT count() FROM ("+level+")", args...).Scan(&c); err != nil {
		return 0, false
	}
	return c, true
}

// bestOfWall times `q` `iters` times and returns the fastest wall (the floor
// strips GC / scheduler jitter — the floor is what the ratio compares).
func bestOfWall(t *testing.T, db *sql.DB, q string, args []any, iters int) time.Duration {
	t.Helper()
	if iters <= 0 {
		iters = scaleWallIters
	}
	// One untimed warm-up so first-call codegen / page faults / mark-tables
	// don't land inside the sampled minimum.
	var warm int64
	if err := db.QueryRow(q, args...).Scan(&warm); err != nil {
		t.Fatalf("warm-up query: %v\nSQL: %s", err, q)
	}
	best := time.Hour
	for i := 0; i < iters; i++ {
		s := time.Now()
		var c int64
		if err := db.QueryRow(q, args...).Scan(&c); err != nil {
			t.Fatalf("wall query: %v\nSQL: %s", err, q)
		}
		if d := time.Since(s); d < best {
			best = d
		}
	}
	return best
}

// stripTrailingScaleSemi drops a trailing `;` / whitespace so the emitted
// statement embeds as a `count() FROM (...)` subquery.
func stripTrailingScaleSemi(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ';' || s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// itoa is strconv.Itoa for an int — local helper so the seed builder reads
// without an import alias collision in this package.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ceilTo rounds x UP to the nearest multiple of `unit` so a regenerated
// bound is a clean, stable ceiling rather than a brittle float transcription.
func ceilTo(x, unit float64) float64 {
	if unit <= 0 {
		return x
	}
	n := x / unit
	r := float64(int64(n))
	if n > r {
		r++
	}
	return r * unit
}

// writeScaleWallBaseline serialises the bound pair as pretty JSON (trailing
// newline) so the committed file diffs cleanly.
func writeScaleWallBaseline(t *testing.T, b scaleWallBaseline) {
	t.Helper()
	buf, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(scaleWallBaselinePath, buf, 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

// loadScaleWallBaseline reads the committed bound pair.
func loadScaleWallBaseline(t *testing.T) scaleWallBaseline {
	t.Helper()
	buf, err := os.ReadFile(scaleWallBaselinePath)
	if err != nil {
		t.Fatalf("read baseline %s: %v — run `just update-scale-wall-baseline` to create it",
			scaleWallBaselinePath, err)
	}
	var b scaleWallBaseline
	if err := json.Unmarshal(buf, &b); err != nil {
		t.Fatalf("parse baseline %s: %v", scaleWallBaselinePath, err)
	}
	if b.ScanAmplificationBound <= 0 || b.WallRatioBound <= 0 {
		t.Fatalf("baseline %s has a non-positive bound (amplification=%.2f, wall_ratio=%.2f) — regenerate it",
			scaleWallBaselinePath, b.ScanAmplificationBound, b.WallRatioBound)
	}
	return b
}
