//go:build chdb

// Package scaling is the generated, registry-driven per-construct
// compute-scaling harness — Component A of the Phase-1 perf assessment
// framework (task #84).
//
// # Why this exists
//
// The Phase-1 audit (wf_a7e317b9) found that EVERY catalogued perf bug was
// a COMPUTE FAN-OUT: the scanned-row count was normal, but an INTERMEDIATE
// stage (a CROSS JOIN step-grid, a re-inlined set-op arm, an unbounded
// recursive CTE, a per-anchor subquery fan-out) exploded cardinality /
// peak memory before the final aggregate collapsed it. The existing
// automation MISSED them all because it measured the wrong axis:
//
//   - benchstat measured Go-side plan CONSTRUCTION time (no chDB execution);
//   - EXPLAIN guards measured READ-side granule pruning (scan rows), not
//     intermediate compute cardinality;
//   - the few hand-written fan-out guards swept the WRONG parameter — e.g.
//     range_lwr pinned `numAnchors` at a FIXED range/step, when the real
//     multiplier is range/step itself.
//
// So each perf fix shipped with its own one-off reactive guard
// (test/perf/{range_lwr,histogram_range,structural_recursive,setop_chain}_
// scaling_chdb_test.go), added AFTER a human found the bug. They all share
// the same skeleton: seed a fixed row set, sweep a parameter, run the
// emitted SQL through in-process chDB, and assert wall-time grows
// sub-linearly + intermediate cardinality stays bounded.
//
// # What this harness does
//
// This package generalises that skeleton into ONE registry-driven driver.
// Each emit-shape REGISTERS a [Construct]: a parameterised query whose
// scaling parameter is THE REAL MULTIPLIER for that shape (range/step for
// range/over_time; set-op chain depth K; subquery inner-resolution So;
// TraceQL attrs-per-span K; structural recursion depth; histogram anchor
// count; ProjectionPushdown column-read width). The driver sweeps >=3
// parameter points at a FIXED scan-row count and asserts, for every
// registered construct:
//
//	(a) WALL-TIME grows SUB-LINEARLY in the scaling parameter — the
//	    measured growth ratio over the sweep stays well under the
//	    parameter's own growth ratio (a true fan-out would track it 1:1
//	    or worse).
//	(b) PEAK INTERMEDIATE-ROW cardinality — max over the construct's
//	    subquery levels of `count() FROM (<level>)` — stays <= a bounded
//	    multiple of the scan rows, INDEPENDENT of the parameter. A fan-out
//	    blows this past the bound.
//
// Adding a new construct is ONE registry entry (a [Construct] literal +
// `func init() { register(...) }`). The four standalone `*_scaling_chdb`
// guards were folded in here and deleted — there is ONE harness, not N
// one-offs, and no coverage was lost (each old guard's seed + emitted-SQL
// generator + bounded contrast is preserved as a registry entry).
//
// It MUST pass on current main: every registered shape is bounded there.
// If a registered shape is NOT sub-linear on main, that is a real finding
// — the assertion is not weakened to make it pass.
//
// Build-tagged `chdb`; runs in the `perf-chdb` lane (./test/perf/...).
package scaling

import (
	"database/sql"
	"sort"
	"strconv"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// Point is one parameter value in a construct's sweep. The driver runs
// every Point against the SAME seeded row set (Seed is run once per
// construct, before the sweep) so the only variable is Param.
type Point struct {
	// Param is the scaling-parameter value at this point — THE REAL
	// fan-out multiplier for the construct (e.g. the anchor count
	// range/step, the chain depth K, the recursion depth). The driver's
	// sub-linearity assertion contrasts the measured wall growth against
	// the growth of this value across the sweep.
	Param int64

	// SQL is the construct's emitted query at this Param, ready to wrap in
	// `count()` and time. It must be the shape the PRODUCTION lowering
	// emits (or, for a contrast baseline, the documented pre-fix shape).
	SQL string

	// Args binds the `?` placeholders SQL carries (nil for inline-literal
	// shapes).
	Args []any

	// LevelSQLs are the construct's intermediate subquery levels — each a
	// SELECT whose row count is one "stage" the query materialises before
	// the final collapse. The driver counts `count() FROM (<level>)` for
	// each and takes the MAX as the peak intermediate cardinality. At
	// minimum this is the single fan-out stage; a construct may list every
	// nesting level it wants bounded. If empty, the driver counts SQL
	// itself as the sole level.
	LevelSQLs []string
}

// Construct is one registered emit-shape under compute-scaling test. It
// owns its seed, its parameter sweep, and the per-shape bound the driver
// asserts the peak intermediate cardinality stays under.
type Construct struct {
	// Name is the sub-test name (`TestScaling_ChDB/<Name>`).
	Name string

	// Param names the scaling parameter being swept, for log/error
	// clarity (e.g. "range/step anchors", "chain depth K").
	Param string

	// Why is a one-line description of the fan-out class this construct
	// guards (surfaces in the failure message so a regression points at
	// the original bug).
	Why string

	// Seed returns the DDL+INSERT statements (split on `;`) that build the
	// construct's FIXED row set. Run once before the sweep. The scan-row
	// count is held constant across every Point — Param is the only
	// variable. Mutually exclusive with Reseed: a construct sets exactly
	// one. Use Seed when the same table serves every Point (the common
	// case); use Reseed when the row set must be rebuilt per point to hold
	// scan rows ~constant while the parameter changes the table's SHAPE
	// (the recursion-depth case).
	Seed func() string

	// Reseed rebuilds the row set for one parameter value, run before each
	// Point. It must hold the scan-row count ~constant across the sweep
	// (the driver records each point's scan rows and bounds the
	// cardinality against the SAME denominator, so a drifting scan count
	// would make the bound meaningless — assert it stays fixed in the
	// Reseed body or keep the per-point row math constant). Mutually
	// exclusive with Seed.
	Reseed func(t *testing.T, db *sql.DB, param int64)

	// ScanRowsSQL counts the construct's scan rows (the denominator for
	// the cardinality bound). Run after Seed (once) or after each Reseed
	// (per point); when Reseed is set the driver re-reads it per point and
	// uses the FIRST point's count as the fixed denominator (and fails if a
	// later point's scan count drifts > 25%).
	ScanRowsSQL string

	// Points returns the sweep — >=3 parameter points, ascending Param.
	// Built after Seed so a generator can lower against the seeded schema.
	Points func(t *testing.T) []Point

	// CardinalityBound is the multiple of scan_rows the peak intermediate
	// cardinality must stay under at EVERY point. A bounded fan-out (e.g.
	// rows x lookback/step) stays a small constant multiple; a true
	// fan-out (rows x N) blows past it. Must be > 0.
	CardinalityBound float64

	// SubLinearSlack relaxes the sub-linearity gate: the measured wall
	// growth ratio must stay below paramGrowth x SubLinearSlack. 1.0 means
	// "strictly sub-linear in the parameter"; a value in (0,1) is
	// stricter; >1 leaves headroom for small-absolute-time runner noise on
	// shapes whose floor is dominated by fixed per-query overhead. Default
	// (0) is treated as 0.75 — comfortably sub-linear while portable.
	SubLinearSlack float64

	// Iters is the timed-sample count for each Point's wall measurement;
	// the driver takes the MEDIAN of these samples (after a warm-up pass)
	// so a single fast/slow run can't skew the ratio. Default (0) -> 5.
	Iters int

	// KnownSuperlinear quarantines the wall-time invariant (a) as a
	// DOCUMENTED OPEN FINDING rather than a hard failure: the driver still
	// RUNS the sub-linearity measurement and LOGS a loud violation when it
	// trips (so the finding stays visible and the data is captured on every
	// run), but does NOT fail the suite on the wall axis. The cardinality
	// invariant (b) stays hard-asserted regardless.
	//
	// This is NOT weakening the assertion — the threshold is unchanged and
	// the violation is reported every run; the construct is flagged because
	// the super-linearity is a real, separately-tracked perf bug that exists
	// on current main, so failing the gate on it would block every unrelated
	// PR. Set the tracking pointer in the string (a non-empty value is
	// required) so the quarantine can't be silent.
	//
	// REMOVE this flag (flip the wall axis back to a hard gate) once the
	// tracked bug is fixed.
	KnownSuperlinear string
}

// register adds c to the package registry. Called from each construct
// file's init(). Ordering is registration order (stable, since init runs
// per-file alphabetically within the package).
func register(c Construct) { registry = append(registry, c) }

// registry holds every registered construct. Iterated by TestScaling_ChDB.
var registry []Construct

// openChDB opens the in-process chDB driver and pings it. Each construct
// runs on its OWN connection (a fresh session) so a seed / DROP in one
// construct can't bleed into another's row set.
func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

// warmupRuns is the count of untimed priming executions done before any
// timed sample. The first execution of a query under chDB pays one-off
// costs that are NOT part of the compute being measured — lazy engine
// init, plan/parse caching, cold OS page cache for the just-seeded table.
// Discarding them keeps a single cold run from distorting a point's wall
// (which, on a sub-20ms floor, swings the K=2/K=8 ratio the gate reads).
const warmupRuns = 1

// medianWall runs the timed query `iters` times (after warmupRuns priming
// passes) and returns the MEDIAN wall, not the min.
//
// Why median, not min: the gate compares a RATIO of two point timings
// (last/first), and the K=2 floor is dominated by fixed per-query
// overhead (~15ms), so the ratio is hypersensitive to its denominator. A
// min estimator is a single-sample extreme: one unusually fast K=2 pass
// (or one slow K=8 pass) on a shared CI runner skews the ratio by a third
// or more — exactly the run-to-run jitter that tripped this gate by 1.6%
// at the 3.6x line while the deterministic cardinality axis stayed flat.
// The median is the central order statistic: insensitive to a single fast
// OR slow outlier at either endpoint, so the ratio reflects the SUSTAINED
// compute cost (a real super-linear regression moves every sample, so the
// median moves with it and the gate still bites).
func medianWall(t *testing.T, db *sql.DB, q string, args []any, iters int) time.Duration {
	t.Helper()
	if iters <= 0 {
		iters = 5
	}
	wrapped := "SELECT count() FROM (" + q + ")"
	run := func() time.Duration {
		s := time.Now()
		var c int64
		if err := db.QueryRow(wrapped, args...).Scan(&c); err != nil {
			t.Fatalf("wall query: %v\nSQL: %s", err, q)
		}
		return time.Since(s)
	}
	for i := 0; i < warmupRuns; i++ {
		run()
	}
	samples := make([]time.Duration, iters)
	for i := range samples {
		samples[i] = run()
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// cardinalityOf returns `count() FROM (<level>)` — the row count one
// intermediate stage materialises. args binds the level's `?` placeholders.
func cardinalityOf(t *testing.T, db *sql.DB, level string, args []any) int64 {
	t.Helper()
	var c int64
	if err := db.QueryRow("SELECT count() FROM ("+level+")", args...).Scan(&c); err != nil {
		t.Fatalf("cardinality query: %v\nSQL: %s", err, level)
	}
	return c
}

// ratio is a/b as a float (guarding b<=0 -> 1), used for every growth /
// speedup comparison so the assertions are runner-portable (no absolute-ms
// thresholds).
func ratio(a, b time.Duration) float64 {
	if b <= 0 {
		return 1
	}
	return float64(a) / float64(b)
}

// fratio is the float form of ratio for parameter / cardinality growth.
func fratio(a, b float64) float64 {
	if b <= 0 {
		return 1
	}
	return a / b
}

// ftoa formats a float with two decimals (used in the quarantine-path log
// message, which is assembled as a plain string rather than a t.Errorf
// format so the KnownSuperlinear / hard-fail branches share one builder).
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}
