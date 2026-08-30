package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// mixedDropCollisionsPlan builds the minimal Mixed VectorSetOr shape
// internal/promql's combineMixedAggregateBranches emits: a histogram-shaped
// arm, a float-shaped arm, and the survival rule under test.
func mixedDropCollisionsPlan(drop bool) *chplan.VectorSetOp {
	histArm := &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		MetricNameColumn:           "MetricName",
		AttributesColumn:           "Attributes",
		TimestampColumn:            "TimeUnix",
	}
	floatArm := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}
	return &chplan.VectorSetOp{
		Left:                histArm,
		Right:               floatArm,
		Op:                  chplan.VectorSetOr,
		Mixed:               true,
		MixedDropCollisions: drop,
		StepAligned:         true,
		MetricNameColumn:    "MetricName",
		AttributesColumn:    "Attributes",
		TimestampColumn:     "TimeUnix",
		ValueColumn:         "Value",
	}
}

// TestEmitMixedVectorSetOp_DropCollisionsSurvivalRule pins the ONE thing
// [chplan.VectorSetOp.MixedDropCollisions] changes about the emitted SQL:
// the survival test.
//
//   - The left-biased `or` keeps a row when it came from the LEFT arm, or
//     when no left row shares its signature — so it projects the row's own
//     `_setop_side` and reads it back in the WHERE.
//   - The drop-collisions union keeps a row only when its signature is
//     owned by exactly ONE arm, which is a test on the two partition-wide
//     flags alone — so `_setop_side` never reaches the outer WHERE, and a
//     second `max(_setop_side = 1) OVER (…)` flag appears beside the first.
//
// Both shapes must still make a SINGLE pass over one UNION ALL of the two
// arms: that is what makes this node the one-reference spelling of the
// drop-the-disagreeing-group rule.
func TestEmitMixedVectorSetOp_DropCollisionsSurvivalRule(t *testing.T) {
	t.Parallel()

	biased, _, err := Emit(context.Background(), mixedDropCollisionsPlan(false))
	if err != nil {
		t.Fatalf("Emit(left-biased): %v", err)
	}
	dropped, _, err := Emit(context.Background(), mixedDropCollisionsPlan(true))
	if err != nil {
		t.Fatalf("Emit(drop-collisions): %v", err)
	}

	const (
		hasLeftFlag  = "max(`_setop_side` = 0) OVER"
		hasRightFlag = "max(`_setop_side` = 1) OVER"
		biasedWhere  = "WHERE `_setop_side` = 0 OR `_setop_has_left` = 0"
		droppedWhere = "WHERE `_setop_has_left` = 0 OR `_setop_has_right` = 0"
	)

	if !strings.Contains(biased, biasedWhere) {
		t.Errorf("left-biased union: missing %q\nSQL: %s", biasedWhere, biased)
	}
	if strings.Contains(biased, hasRightFlag) {
		t.Errorf("left-biased union: projects the right-side flag it never reads\nSQL: %s", biased)
	}

	if !strings.Contains(dropped, droppedWhere) {
		t.Errorf("drop-collisions union: missing %q\nSQL: %s", droppedWhere, dropped)
	}
	if strings.Contains(dropped, biasedWhere) {
		t.Errorf("drop-collisions union: still carries the left-biased survival test — a signature both arms claim would survive through the left arm instead of being dropped\nSQL: %s", dropped)
	}
	for _, want := range []string{hasLeftFlag, hasRightFlag} {
		if !strings.Contains(dropped, want) {
			t.Errorf("drop-collisions union: missing %q — the symmetric test needs both partition-wide flags\nSQL: %s", want, dropped)
		}
	}

	// One UNION ALL, one reference per arm, in BOTH shapes.
	for name, sql := range map[string]string{"left-biased": biased, "drop-collisions": dropped} {
		if got := strings.Count(sql, "UNION ALL"); got != 1 {
			t.Errorf("%s union: %d UNION ALL clauses, want exactly 1 (a single pass over both arms)", name, got)
		}
		for _, table := range []string{"otel_metrics_exponential_histogram", "otel_metrics_gauge"} {
			if got := strings.Count(sql, table); got != 1 {
				t.Errorf("%s union: table %s appears %d times, want exactly 1 (each arm read once)", name, table, got)
			}
		}
	}
}

// TestValidateVectorSetOpCols_DropCollisionsNeedsMixedOr pins that the
// flag is REJECTED wherever it has no meaning rather than silently
// ignored: only emitMixedVectorSetOp reads it, and only a Mixed `or`
// reaches that emitter. Ignoring it would let a mis-built plan emit a
// left-biased union where the caller asked for a symmetric difference —
// a wrong ANSWER, with no error to notice it by.
func TestValidateVectorSetOpCols_DropCollisionsNeedsMixedOr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*chplan.VectorSetOp)
		wantErr bool
	}{
		{name: "mixed or", mutate: func(*chplan.VectorSetOp) {}},
		{name: "mixed unless", mutate: func(s *chplan.VectorSetOp) { s.Op = chplan.VectorSetUnless }, wantErr: true},
		{name: "mixed and", mutate: func(s *chplan.VectorSetOp) { s.Op = chplan.VectorSetAnd }, wantErr: true},
		{name: "non-mixed or", mutate: func(s *chplan.VectorSetOp) { s.Mixed = false }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := mixedDropCollisionsPlan(true)
			tc.mutate(plan)
			_, _, err := Emit(context.Background(), plan)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("Emit = %v, want ErrUnsupported (MixedDropCollisions is meaningless here and must not be ignored)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Emit = %v, want success", err)
			}
		})
	}
}

// TestVectorSetOpArmTimestampCol_BroadcastCrossJoin pins the second half
// of the `@`-pinned broadcast fix. [chplan.IsDerivedShape] classifies a
// broadcast Project over `StepGrid × <reduced relation>` as DERIVED, so
// vectorSetOpCanonicalQuartetFrags synthesises MetricName for it — and
// then reaches for a timestamp. Without a matrix answer it would take the
// synthesised-INSTANT-anchor branch, stamping every broadcast step with
// one shared timestamp and collapsing a whole query_range matrix onto a
// single point. The grid's own per-row anchor is the right answer, and
// the broadcast Project republishes it under its own name.
func TestVectorSetOpArmTimestampCol_BroadcastCrossJoin(t *testing.T) {
	t.Parallel()

	setOp := &chplan.VectorSetOp{
		Op:               chplan.VectorSetOr,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	broadcast := &chplan.Project{
		Input: &chplan.CrossJoin{
			Left:  &chplan.StepGrid{Step: time.Minute},
			Right: &chplan.RangeWindow{Input: &chplan.Scan{Table: "otel_metrics_gauge"}, Func: "sum_over_time"},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: chplan.RangeWindowAnchorColumn},
			{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}

	if !chplan.IsDerivedShape(broadcast, vectorSetOpSampleColumns(setOp)) {
		t.Fatal("the broadcast Project must classify as derived — it publishes no MetricName and neither side of its cross join does")
	}
	col, matrix := vectorSetOpArmTimestampCol(broadcast, setOp)
	if !matrix {
		t.Fatalf("vectorSetOpArmTimestampCol reported no matrix answer; a derived arm with none takes the synthesised-instant-anchor branch and collapses every broadcast step onto one timestamp")
	}
	if col != chplan.RangeWindowAnchorColumn {
		t.Errorf("timestamp column = %q, want %q (the grid's own per-row anchor, which the broadcast Project republishes)", col, chplan.RangeWindowAnchorColumn)
	}
}
