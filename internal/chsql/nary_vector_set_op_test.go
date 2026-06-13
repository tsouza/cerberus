package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func narySetOpArm(table string) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Scan{Table: table},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}
}

func naryOp(op chplan.VectorSetOpKind, tables ...string) *chplan.NaryVectorSetOp {
	arms := make([]chplan.Node, len(tables))
	for i, tbl := range tables {
		arms[i] = narySetOpArm(tbl)
	}
	return &chplan.NaryVectorSetOp{
		Arms: arms, Op: op,
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
}

// TestEmitNaryVectorSetOp_OrThreeArms pins the `or` single-pass shape:
// ONE UNION ALL over all three side-tagged arms, ONE min-side window
// partitioned by the signature, and the earliest-arm-wins WHERE.
func TestEmitNaryVectorSetOp_OrThreeArms(t *testing.T) {
	t.Parallel()
	sql, _, err := chsql.Emit(context.Background(), naryOp(chplan.VectorSetOr, "a", "b", "c"))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Exactly two UNION ALLs join three arms into one pass (not nested).
	if got := strings.Count(sql, "UNION ALL"); got != 2 {
		t.Errorf("UNION ALL count = %d, want 2 (single flat 3-arm union); sql=%s", got, sql)
	}
	// One min-side window, one survival predicate.
	if !strings.Contains(sql, "min(`_setop_side`) OVER (PARTITION BY `Attributes`)") {
		t.Errorf("missing min-side window over the signature; sql=%s", sql)
	}
	if !strings.Contains(sql, "WHERE `_setop_side` = `_setop_min_side`") {
		t.Errorf("missing earliest-arm-wins survival predicate; sql=%s", sql)
	}
	// Each arm tags a distinct side index 0/1/2.
	for _, side := range []string{"0 AS `_setop_side`", "1 AS `_setop_side`", "2 AS `_setop_side`"} {
		if !strings.Contains(sql, side) {
			t.Errorf("missing side tag %q; sql=%s", side, sql)
		}
	}
}

// TestEmitNaryVectorSetOp_AndThreeArms pins the `and` single-pass shape:
// ONE UNION ALL, ONE groupBitOr window, and the present-in-every-arm
// WHERE that keeps arm-0 rows whose mask is all-ones (`(1<<3)-1 = 7`).
func TestEmitNaryVectorSetOp_AndThreeArms(t *testing.T) {
	t.Parallel()
	sql, _, err := chsql.Emit(context.Background(), naryOp(chplan.VectorSetAnd, "a", "b", "c"))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := strings.Count(sql, "UNION ALL"); got != 2 {
		t.Errorf("UNION ALL count = %d, want 2; sql=%s", got, sql)
	}
	if !strings.Contains(sql, "groupBitOr(") || !strings.Contains(sql, "OVER (PARTITION BY `Attributes`)") {
		t.Errorf("missing groupBitOr signature window; sql=%s", sql)
	}
	if !strings.Contains(sql, "`_setop_side` = 0") || !strings.Contains(sql, "`_setop_sides_mask` = 7") {
		t.Errorf("missing present-in-every-arm survival predicate (mask 7); sql=%s", sql)
	}
}

// TestEmitNaryVectorSetOp_RejectsUnlessOp proves `unless` can never reach
// the N-ary emitter — it is not associative, so the validator rejects it.
func TestEmitNaryVectorSetOp_RejectsUnlessOp(t *testing.T) {
	t.Parallel()
	n := naryOp(chplan.VectorSetUnless, "a", "b")
	if _, _, err := chsql.Emit(context.Background(), n); err == nil {
		t.Fatal("expected error emitting a non-associative `unless` N-ary node")
	}
}

// TestEmitNaryVectorSetOp_RejectsSingleArm proves a degenerate 1-arm node
// is rejected — the flatten rule never mints one.
func TestEmitNaryVectorSetOp_RejectsSingleArm(t *testing.T) {
	t.Parallel()
	n := naryOp(chplan.VectorSetOr, "a")
	if _, _, err := chsql.Emit(context.Background(), n); err == nil {
		t.Fatal("expected error emitting a single-arm N-ary node")
	}
}
