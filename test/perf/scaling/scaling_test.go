//go:build chdb

package scaling

import (
	"database/sql"
	"strconv"
	"testing"
	"time"
)

// TestScaling_ChDB is the single, generic driver that replaces the four
// standalone *_scaling_chdb guards. It iterates the package registry and,
// for each registered [Construct], seeds a FIXED row set once, sweeps the
// construct's REAL fan-out parameter across >=3 points, and asserts the
// two compute-scaling invariants:
//
//	(a) wall-time grows SUB-LINEARLY in the parameter, AND
//	(b) peak intermediate-row cardinality stays <= bound x scan_rows.
//
// Adding a construct is a registry entry; this driver never changes.
func TestScaling_ChDB(t *testing.T) {
	if len(registry) == 0 {
		t.Fatal("scaling registry is empty — no constructs registered; the harness would silently pass")
	}
	for _, c := range registry {
		t.Run(c.Name, func(t *testing.T) {
			runConstruct(t, c)
		})
	}
}

// measured is one sweep point's result: its parameter, its wall (the
// minimum over wallRounds interleaved rounds) and its peak intermediate
// cardinality.
type measured struct {
	param   int64
	wall    time.Duration
	peakCar int64
}

// sweep seeds c, samples every point of its sweep and returns the
// per-point measurements plus the scan-row count the cardinality bound
// divides by.
func sweep(t *testing.T, c Construct) ([]measured, int64) {
	t.Helper()
	if (c.Seed == nil) == (c.Reseed == nil) {
		t.Fatalf("construct %q: set exactly one of Seed / Reseed", c.Name)
	}

	// Each construct gets its own connection / session so a seed or DROP in
	// one never bleeds into another's row set.
	db := openChDB(t)
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}

	// Static-seed constructs build the row set once here; reseed constructs
	// rebuild it per point inside the sweep loop below.
	var scanRows int64
	if c.Seed != nil {
		for _, stmt := range splitSQL(c.Seed()) {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("seed: %v\n%s", err, stmt)
			}
		}
		if err := db.QueryRow(c.ScanRowsSQL).Scan(&scanRows); err != nil {
			t.Fatalf("scan-row count (%s): %v", c.ScanRowsSQL, err)
		}
		if scanRows <= 0 {
			t.Fatalf("construct %q: scan-row count is %d — the seed produced no rows, the guard is mis-seeded",
				c.Name, scanRows)
		}
	}

	points := c.Points(t)
	if len(points) < 3 {
		t.Fatalf("construct %q: a compute-scaling sweep needs >=3 parameter points, got %d", c.Name, len(points))
	}

	rows := make([]measured, len(points))

	// Sample the sweep in wallRounds interleaved rounds and keep each point's
	// minimum wall (see wallRounds). The intermediate cardinality is
	// deterministic, so it is counted once, in the first round. A Reseed
	// construct builds each point's row set once, in the first round, inside
	// that point's own database, and later rounds switch back to it.
	if c.Reseed != nil {
		t.Cleanup(func() { execAll(t, db, "USE default") })
	}
	for round := range wallRounds {
		for i, p := range points {
			if c.Reseed != nil {
				usePointDatabase(t, db, c, i, round == 0)
				if round == 0 {
					reseedPoint(t, db, c, p.Param, i == 0, &scanRows)
				}
			}

			wall := timeWall(t, db, p.SQL, p.Args)
			if round > 0 {
				rows[i].wall = min(rows[i].wall, wall)
				continue
			}

			levels := p.LevelSQLs
			if len(levels) == 0 {
				levels = []string{p.SQL}
			}
			var peak int64
			for _, lvl := range levels {
				if car := cardinalityOf(t, db, lvl, p.Args); car > peak {
					peak = car
				}
			}
			rows[i] = measured{p.Param, wall, peak}
		}
	}
	for _, m := range rows {
		t.Logf("%-8s=%-8d  wall=%-12v  peak_intermediate=%-10d  (%.2fx scan_rows=%d)",
			c.Param, m.param, m.wall.Round(time.Microsecond), m.peakCar,
			float64(m.peakCar)/float64(scanRows), scanRows)
	}

	return rows, scanRows
}

// wallGate returns the sweep's parameter growth, its wall growth (last point
// over first) and the gate the wall growth must stay under:
// paramGrowth x slack + wallJitterMargin.
func wallGate(c Construct, rows []measured) (paramGrowth, wallGrowth, gate float64) {
	first, last := rows[0], rows[len(rows)-1]
	paramGrowth = fratio(float64(last.param), float64(first.param))
	wallGrowth = ratio(last.wall, first.wall)
	return paramGrowth, wallGrowth, paramGrowth*c.slack() + wallJitterMargin
}

// slack is the construct's SubLinearSlack, or defaultSubLinearSlack when it
// sets none.
func (c Construct) slack() float64 {
	if c.SubLinearSlack == 0 {
		return defaultSubLinearSlack
	}
	return c.SubLinearSlack
}

// wallJitterMargin is the additive headroom the wall gate leaves on top of
// paramGrowth x slack, for the residual difference between two endpoint
// walls that are each a minimum over wallRounds rounds. It is ADDITIVE and
// bounded: it absorbs a flat shape's residual noise but is dwarfed by a
// genuinely super-linear construct, whose wall growth overshoots the gate by
// a multiple (see TestScalingWallGate_CatchesSuperlinear). A LINEAR fan-out
// is a different matter: when a fixed scan cost dominates the first point,
// wall grows by less than the parameter even for rows x N work, so the wall
// axis is not what catches it. The deterministic cardinality invariant (b)
// is the primary anti-fan-out gate and carries no margin.
const wallJitterMargin = 0.25

// defaultSubLinearSlack is the SubLinearSlack a construct gets when it sets
// none: wall may grow by at most three quarters of the parameter's growth.
const defaultSubLinearSlack = 0.75

func runConstruct(t *testing.T, c Construct) {
	if c.CardinalityBound <= 0 {
		t.Fatalf("construct %q: CardinalityBound must be > 0", c.Name)
	}
	if c.KnownSuperlinear != "" && c.WallAxisLinearByDesign != "" {
		t.Fatalf("construct %q: set at most one of KnownSuperlinear / WallAxisLinearByDesign — "+
			"the first quarantines a tracked super-linear bug, the second records a permanently "+
			"uninformative wall axis; a construct is one or the other, not both.", c.Name)
	}

	rows, scanRows := sweep(t, c)
	first, last := rows[0], rows[len(rows)-1]

	// --- Invariant (a): wall grows SUB-LINEARLY in the parameter ----------
	//
	// Contrast the measured wall-growth ratio against the parameter's own
	// growth across the sweep. A true compute fan-out makes wall track the
	// parameter 1:1 (or worse); a bounded shape grows far flatter. The gate
	// is `wallGrowth < paramGrowth x slack + wallJitterMargin` — runner-portable
	// (a ratio of ratios, no absolute-ms threshold).
	paramGrowth, wallGrowth, gate := wallGate(c, rows)
	t.Logf("%s: param grew %.2fx (%d->%d), wall grew %.2fx (%v->%v)",
		c.Name, paramGrowth, first.param, last.param, wallGrowth,
		first.wall.Round(time.Microsecond), last.wall.Round(time.Microsecond))

	if paramGrowth <= 1.0 {
		t.Fatalf("construct %q: parameter did not grow across the sweep (%.2fx) — the sweep is mis-built; "+
			"a sub-linearity assertion is meaningless without a growing parameter.", c.Name, paramGrowth)
	}
	switch {
	case c.WallAxisLinearByDesign != "":
		// The wall axis is uninformative for this construct by construction:
		// its bounded shape and its fan-out shape are BOTH linear in the
		// parameter (see WallAxisLinearByDesign), so no wall gate separates
		// them. Measure and log for visibility, but delegate the fan-out gate
		// entirely to the deterministic cardinality invariant (b) below.
		t.Logf("WALL-AXIS-DELEGATED %s: wall grew %.2fx while %s grew %.2fx (reference gate %.2fx) — "+
			"wall cannot discriminate the bounded shape from a fan-out here; the cardinality axis (b) is the gate.",
			c.Name, wallGrowth, c.Param, paramGrowth, gate)
		t.Logf("  reason: %s", c.WallAxisLinearByDesign)
	case wallGrowth >= gate:
		msg := func(verb string) string {
			return verb + ": compute-scaling violation in " + c.Name + " (" + c.Why + "): wall-time grew " +
				ftoa(wallGrowth) + "x while " + c.Param + " grew only " + ftoa(paramGrowth) + "x (slack=" +
				ftoa(c.slack()) + ", jitter=" + ftoa(wallJitterMargin) + " -> gate " + ftoa(gate) +
				"x). Wall is tracking the parameter."
		}
		if c.KnownSuperlinear != "" {
			// Quarantined: log the finding loudly but do NOT fail the wall
			// axis. The cardinality axis below still gates.
			t.Logf("KNOWN-SUPERLINEAR %s", msg("FINDING"))
			t.Logf("  tracked: %s", c.KnownSuperlinear)
			t.Logf("  first: %s=%d wall=%v   last: %s=%d wall=%v",
				c.Param, first.param, first.wall, c.Param, last.param, last.wall)
		} else {
			t.Errorf("%s The %s fan-out regressed back in. first: %s=%d wall=%v   last: %s=%d wall=%v",
				msg("regression"), c.Why, c.Param, first.param, first.wall, c.Param, last.param, last.wall)
		}
	case c.KnownSuperlinear != "":
		// The quarantined construct is now sub-linear — the tracked bug may
		// be FIXED. Surface it so the flag gets removed (flip back to a hard
		// gate) rather than lingering as dead quarantine.
		t.Logf("NOTE: %q is flagged KnownSuperlinear (%s) but measured SUB-LINEAR this run "+
			"(wall %.2fx < gate %.2fx). If the tracked bug is fixed, remove the KnownSuperlinear flag to "+
			"restore the hard wall gate.", c.Name, c.KnownSuperlinear, wallGrowth, gate)
	}

	// --- Invariant (b): peak intermediate cardinality stays BOUNDED -------
	//
	// The decisive fan-out check: the max-over-levels intermediate row count
	// must stay <= bound x scan_rows at EVERY point, INDEPENDENT of the
	// parameter. A bounded shape (rows x lookback/step, rows, k+1 rows)
	// keeps a small constant multiple; a fan-out (rows x N) blows past it.
	for _, m := range rows {
		mult := float64(m.peakCar) / float64(scanRows)
		if mult > c.CardinalityBound {
			t.Errorf("compute-fan-out regression in %q (%s): at %s=%d the peak intermediate cardinality is "+
				"%d rows = %.2fx scan_rows (%d), over the %.2fx bound. An intermediate stage exploded — the "+
				"%s fan-out is materialising rows x parameter instead of a bounded multiple.",
				c.Name, c.Why, c.Param, m.param, m.peakCar, mult, scanRows, c.CardinalityBound, c.Why)
		}
	}
}

// usePointDatabase makes point i's database current for a Reseed construct,
// creating it on the first round. Each point keeps its row set in its own
// database so the interleaved rounds can return to it without rebuilding it:
// the constructs' SQL names its tables unqualified, so it resolves against
// whichever point database is current. Rebuilding per round instead churns a
// DROP per round, and chDB's Atomic database keeps a dropped table's data
// until a delayed cleanup, so that grows memory with every round.
func usePointDatabase(t *testing.T, db *sql.DB, c Construct, i int, create bool) {
	t.Helper()
	name := "scaling_" + c.Name + "_p" + strconv.Itoa(i)
	if create {
		execAll(t, db, "CREATE DATABASE IF NOT EXISTS "+name)
	}
	execAll(t, db, "USE "+name)
}

// scanRowDriftTolerance is how far a reseeded point's scan-row count may
// move from the sweep's fixed denominator, as a fraction. Every registered
// reseed holds the count exactly at its swept parameters (the recursion-depth
// row math divides evenly at D = 4, 16, 48); the tolerance admits the
// whole-trace rounding a new depth could introduce and rejects a reseed whose
// row count actually follows the parameter.
const scanRowDriftTolerance = 0.25

// reseedPoint builds a Reseed construct's row set for one parameter value
// and checks its scan-row count. The first point's count fixes the
// denominator the cardinality bound uses; every later point must stay within
// scanRowDriftTolerance of it so the parameter remains the only variable.
func reseedPoint(t *testing.T, db *sql.DB, c Construct, param int64, first bool, scanRows *int64) {
	t.Helper()
	c.Reseed(t, db, param)
	var pointScan int64
	if err := db.QueryRow(c.ScanRowsSQL).Scan(&pointScan); err != nil {
		t.Fatalf("scan-row count (%s) at %s=%d: %v", c.ScanRowsSQL, c.Param, param, err)
	}
	if pointScan <= 0 {
		t.Fatalf("construct %q: scan-row count is %d at %s=%d — the reseed produced no rows",
			c.Name, pointScan, c.Param, param)
	}
	if first {
		*scanRows = pointScan
		return
	}
	if drift := float64(pointScan) / float64(*scanRows); drift > 1+scanRowDriftTolerance ||
		drift < 1/(1+scanRowDriftTolerance) {
		t.Fatalf("construct %q: scan rows drifted to %d at %s=%d (vs fixed %d, %.2fx) — the reseed "+
			"must hold scan rows ~constant so the parameter is the only variable.",
			c.Name, pointScan, c.Param, param, *scanRows, drift)
	}
}

// splitSQL splits a multi-statement seed string on `;` boundaries,
// dropping empty fragments so each statement runs as its own db.Exec.
func splitSQL(s string) []string {
	out := make([]string, 0, 8)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			if p := trimSpace(s[start:i]); p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	if p := trimSpace(s[start:]); p != "" {
		out = append(out, p)
	}
	return out
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && isSpace(s[i]) {
		i++
	}
	for j > i && isSpace(s[j-1]) {
		j--
	}
	return s[i:j]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// stripTrailingSemi drops a trailing `;` / whitespace so SQL can be
// embedded as a subquery. Shared by recursion-shaped constructs whose
// emitted SQL ends in a statement terminator.
func stripTrailingSemi(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ';' || isSpace(s[len(s)-1])) {
		s = s[:len(s)-1]
	}
	return s
}

// execAll runs every statement in stmts against db, failing the test on
// the first error. Shared by constructs that rebuild their table per
// point (structural recursion's depth sweep).
func execAll(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		for _, one := range splitSQL(s) {
			if _, err := db.Exec(one); err != nil {
				t.Fatalf("exec: %v\n%s", err, one)
			}
		}
	}
}
