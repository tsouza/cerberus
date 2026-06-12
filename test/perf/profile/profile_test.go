//go:build chdb

package profile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// writeFixture writes a TXTAR fixture body to a temp file and loads it
// through the spec seam, returning the prepared round-trip.
func writeFixture(t *testing.T, body string) *spec.PreparedRoundTrip {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fx.txtar")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	c, err := spec.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	prep, ok, err := spec.PrepareRoundTrip(c)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !ok {
		t.Fatalf("fixture not recognised as round-trip")
	}
	return prep
}

// TestProfileFixture_ArrayJoinFanOut profiles a fixture whose SQL fans a
// single scanned row across an ARRAY JOIN, and asserts the profiler
// records the fan-out: array-join flag set, fan_factor > 1, scan_rows <
// peak_intermediate.
func TestProfileFixture_ArrayJoinFanOut(t *testing.T) {
	// Cerberus's emitter always nests the leaf Scan as the innermost
	// FROM-source subquery, with the fan-out operator (here ARRAY JOIN)
	// in an outer level — the same shape the histogram / topk range
	// fixtures emit. The inner level counts the pre-fan-out scan rows
	// (1); the outer level counts the post-array-join rows (5). The
	// decomposition recovers fan_factor = 5.
	const body = `-- seed --
CREATE TABLE arr (id UInt64, vals Array(UInt64)) ENGINE = MergeTree ORDER BY id;
INSERT INTO arr VALUES (1, [10, 20, 30, 40, 50]);
-- expected_rows --
[]
-- sql --
SELECT id, v FROM (SELECT id, vals FROM arr) ARRAY JOIN vals AS v
`
	prep := writeFixture(t, body)

	p, err := NewProfiler()
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	defer p.Close()

	rec := p.ProfileFixture("test/array_join", prep)
	if rec.Err != "" {
		t.Fatalf("unexpected profile error: %s", rec.Err)
	}
	if !rec.HasArrayJoin {
		t.Errorf("HasArrayJoin = false, want true")
	}
	if rec.HasCrossJoin || rec.HasRecursiveCTE {
		t.Errorf("unexpected join/cte flags: cross=%v rcte=%v", rec.HasCrossJoin, rec.HasRecursiveCTE)
	}
	// One scanned row fans to five via the array join.
	if rec.ScanRows != 1 {
		t.Errorf("ScanRows = %d, want 1", rec.ScanRows)
	}
	if rec.PeakIntermediate < 5 {
		t.Errorf("PeakIntermediate = %d, want >= 5", rec.PeakIntermediate)
	}
	if rec.FanFactor < 5 {
		t.Errorf("FanFactor = %.2f, want >= 5", rec.FanFactor)
	}
}

// TestProfileFixture_NoFanOut profiles a straight filtered scan and
// asserts fan_factor == 1 and no fan-out operators.
func TestProfileFixture_NoFanOut(t *testing.T) {
	const body = `-- seed --
CREATE TABLE flat (a UInt64) ENGINE = MergeTree ORDER BY a;
INSERT INTO flat SELECT number FROM numbers(100);
-- expected_rows --
[]
-- sql --
SELECT a FROM flat WHERE a >= 90
`
	prep := writeFixture(t, body)

	p, err := NewProfiler()
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	defer p.Close()

	rec := p.ProfileFixture("test/flat", prep)
	if rec.Err != "" {
		t.Fatalf("unexpected profile error: %s", rec.Err)
	}
	if rec.HasArrayJoin || rec.HasCrossJoin || rec.HasRecursiveCTE {
		t.Errorf("unexpected fan-out operators: array=%v cross=%v rcte=%v",
			rec.HasArrayJoin, rec.HasCrossJoin, rec.HasRecursiveCTE)
	}
	if rec.FanFactor != 1 {
		t.Errorf("FanFactor = %.2f, want 1.0 (no fan-out)", rec.FanFactor)
	}
}

// TestProfileFixture_RecursiveCTE profiles a recursive CTE and asserts the
// recursive flag fires and max_recursion_depth reflects the closure size.
func TestProfileFixture_RecursiveCTE(t *testing.T) {
	const body = `-- seed --
CREATE TABLE anchor (n UInt64) ENGINE = MergeTree ORDER BY n;
INSERT INTO anchor VALUES (1);
-- expected_rows --
[]
-- sql --
WITH RECURSIVE walk AS (SELECT n FROM anchor UNION ALL SELECT n + 1 FROM walk WHERE n < 5) SELECT n FROM walk
`
	prep := writeFixture(t, body)

	p, err := NewProfiler()
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	defer p.Close()

	rec := p.ProfileFixture("test/rcte", prep)
	if rec.Err != "" {
		t.Fatalf("unexpected profile error: %s", rec.Err)
	}
	if !rec.HasRecursiveCTE {
		t.Errorf("HasRecursiveCTE = false, want true")
	}
	// Closure walks n=1..5, five rows.
	if rec.MaxRecursionDepth != 5 {
		t.Errorf("MaxRecursionDepth = %d, want 5", rec.MaxRecursionDepth)
	}
}

// TestSortByFanFactor pins the descending ordering used by the nightly
// step-summary.
func TestSortByFanFactor(t *testing.T) {
	recs := []Record{
		{Fixture: "a", FanFactor: 2},
		{Fixture: "b", FanFactor: 10},
		{Fixture: "c", FanFactor: 5},
	}
	SortByFanFactor(recs)
	if recs[0].Fixture != "b" || recs[1].Fixture != "c" || recs[2].Fixture != "a" {
		t.Errorf("sort order = %s,%s,%s; want b,c,a",
			recs[0].Fixture, recs[1].Fixture, recs[2].Fixture)
	}
}
