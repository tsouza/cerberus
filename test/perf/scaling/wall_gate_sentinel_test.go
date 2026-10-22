//go:build chdb

package scaling

import (
	"fmt"
	"testing"
)

// wallGateSentinelRows is the row count of the sentinel's table and its
// largest sweep point. At 8000 the N^2 pair work dominates the fixed
// per-query cost even at the smallest point (N/4): measured wall growth over
// the 4x sweep was 9.7x-12.4x against a 3.25x gate, where 4000 rows gave
// 5.8x-7.2x.
const wallGateSentinelRows = 8000

// wallGateSentinel is a deliberately quadratic construct: a self CROSS JOIN
// over the first N rows of a fixed table, N^2 pairs for N rows read. It is
// never registered; TestScalingWallGate_CatchesSuperlinear runs it through
// the same sweep and gate as every registered construct.
var wallGateSentinel = Construct{
	Name:             "wall_gate_sentinel",
	Param:            "joined rows N",
	Why:              "deliberate N x N self cross join",
	ScanRowsSQL:      "SELECT count() FROM wall_gate_sentinel",
	CardinalityBound: 1,
	Seed: func() string {
		return fmt.Sprintf(`DROP TABLE IF EXISTS wall_gate_sentinel;
			CREATE TABLE wall_gate_sentinel (v UInt64) ENGINE = MergeTree() ORDER BY v;
			INSERT INTO wall_gate_sentinel SELECT number FROM numbers(%d);`, wallGateSentinelRows)
	},
	Points: func(*testing.T) []Point {
		ns := []int64{wallGateSentinelRows / 4, wallGateSentinelRows / 2, wallGateSentinelRows}
		pts := make([]Point, 0, len(ns))
		for _, n := range ns {
			pts = append(pts, Point{
				Param: n,
				SQL: fmt.Sprintf(`SELECT a.v FROM
					(SELECT v FROM wall_gate_sentinel ORDER BY v LIMIT %[1]d) AS a CROSS JOIN
					(SELECT v FROM wall_gate_sentinel ORDER BY v LIMIT %[1]d) AS b
					WHERE a.v >= b.v`, n),
				LevelSQLs: []string{fmt.Sprintf("SELECT v FROM wall_gate_sentinel ORDER BY v LIMIT %d", n)},
			})
		}
		return pts
	},
}

// TestScalingWallGate_CatchesSuperlinear proves the wall gate (invariant (a))
// can fail: a quadratic construct, measured by the same min-of-rounds sweep
// the registered constructs use, must grow past its gate. A sampler change
// that flattens real growth (or a gate loosened until nothing trips it)
// turns this red.
func TestScalingWallGate_CatchesSuperlinear(t *testing.T) {
	rows, _ := sweep(t, wallGateSentinel)
	paramGrowth, wallGrowth, gate := wallGate(wallGateSentinel, rows)
	t.Logf("%s: param grew %.2fx, wall grew %.2fx, gate %.2fx",
		wallGateSentinel.Name, paramGrowth, wallGrowth, gate)
	if wallGrowth < gate {
		t.Errorf("the wall gate did not trip on a quadratic construct: wall grew %.2fx while %s grew %.2fx "+
			"(gate %.2fx). The sub-linearity gate cannot catch a super-linear regression as measured.",
			wallGrowth, wallGateSentinel.Param, paramGrowth, gate)
	}
}
