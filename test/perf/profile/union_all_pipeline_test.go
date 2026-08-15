//go:build chdb

package profile

import "testing"

// TestProfileFixture_UnionAllPipelineSumsArmScans reproduces issue #2051:
// each UNION arm has its own nested pipeline, but the old leftmost descent
// reaches only the first arm's leaf and records a hollow denominator of two.
func TestProfileFixture_UnionAllPipelineSumsArmScans(t *testing.T) {
	const body = `-- seed --
CREATE TABLE pipeline_union_left (a UInt64) ENGINE = MergeTree ORDER BY a;
INSERT INTO pipeline_union_left VALUES (1), (2);
CREATE TABLE pipeline_union_right (a UInt64) ENGINE = MergeTree ORDER BY a;
INSERT INTO pipeline_union_right VALUES (3), (4), (5);
-- expected_rows --
[]
-- sql --
SELECT a FROM (
  SELECT a FROM (SELECT a FROM pipeline_union_left)
  UNION ALL
  SELECT a FROM (SELECT a FROM pipeline_union_right)
)
`
	prep := writeFixture(t, body)

	p, err := NewProfiler()
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	defer p.Close()

	rec := p.ProfileFixture("test/union_all_pipeline", prep)
	if rec.Err != "" {
		t.Fatalf("unexpected profile error: %s", rec.Err)
	}
	if rec.ScanRows != 5 {
		t.Errorf("ScanRows = %d, want 5 (2 left-arm rows + 3 right-arm rows)", rec.ScanRows)
	}
	if rec.ResultRows != 5 || rec.PeakIntermediate != 5 {
		t.Errorf("(ResultRows, PeakIntermediate) = (%d, %d), want (5, 5)",
			rec.ResultRows, rec.PeakIntermediate)
	}
	if rec.UncountableLevels != 0 {
		t.Errorf("UncountableLevels = %d, want 0: %v", rec.UncountableLevels, rec.UncountableReasons)
	}
	if rec.FanFactor == nil || *rec.FanFactor != 1 {
		t.Errorf("FanFactor = %v, want measured 1.0", rec.FanFactor)
	}
}
