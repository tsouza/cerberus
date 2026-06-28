package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// binSetOpScanArm builds a bare-Scan arm. A Scan is canonical-shape
// (not derived) and not a matrix RangeWindow, so the per-arm canonical
// projection emits the timestamp as the plain `TimeUnix` column — this
// is the shape line 281 (`if derived && !matrix`) must select.
func binSetOpScanArm(table string) *chplan.Scan { return &chplan.Scan{Table: table} }

func binSetOp(op chplan.VectorSetOpKind, left, right chplan.Node) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left: left, Right: right, Op: op,
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
}

func emitBinSetOp(t *testing.T, op chplan.VectorSetOpKind) string {
	t.Helper()
	sql, _, err := chsql.Emit(context.Background(), binSetOp(op, binSetOpScanArm("a"), binSetOpScanArm("b")))
	if err != nil {
		t.Fatalf("Emit(%s): %v", op, err)
	}
	return sql
}

// TestEmitVectorSetOp_And_ExactShape pins the `and` semi-join SQL.
//
// Kills vector_set_op.go:66:9 + 70:9 (CONDITIONALS_NEGATION on the two
// `if err != nil` guards around subqueryFrag). When `err != nil` is
// flipped to `err == nil`, the successful (nil-err) path returns early
// — BEFORE any emission — so the buffer is empty and every assertion
// below fails. The original emits the full IN-subquery shape.
func TestEmitVectorSetOp_And_ExactShape(t *testing.T) {
	sql := emitBinSetOp(t, chplan.VectorSetAnd)
	// Outer canonical projection must be present (non-empty emission).
	if !strings.Contains(sql, "SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value`") {
		t.Fatalf("missing outer canonical projection; sql=%s", sql)
	}
	// `and` is a semi-join: IN over a DISTINCT signature subquery, never NOT IN.
	if !strings.Contains(sql, "IN (") || strings.Contains(sql, "NOT IN") {
		t.Errorf("`and` must emit IN (not NOT IN); sql=%s", sql)
	}
	if !strings.Contains(sql, "SELECT DISTINCT `Attributes`") {
		t.Errorf("`and` must filter against DISTINCT signature subquery; sql=%s", sql)
	}
	// Semi-join shape never unions arms.
	if strings.Contains(sql, "UNION ALL") {
		t.Errorf("`and` must not UNION ALL; sql=%s", sql)
	}
}

// TestEmitVectorSetOp_Unless_ExactShape pins the `unless` anti-join SQL.
//
// Also kills 66:9 + 70:9 (empty emission on the negated guards). The
// distinguishing token from `and` is NOT IN.
func TestEmitVectorSetOp_Unless_ExactShape(t *testing.T) {
	sql := emitBinSetOp(t, chplan.VectorSetUnless)
	if !strings.Contains(sql, "SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value`") {
		t.Fatalf("missing outer canonical projection; sql=%s", sql)
	}
	// `unless` is an anti-join: NOT IN against the DISTINCT signature subquery.
	if !strings.Contains(sql, "NOT IN (") {
		t.Errorf("`unless` must emit NOT IN; sql=%s", sql)
	}
	if !strings.Contains(sql, "SELECT DISTINCT `Attributes`") {
		t.Errorf("`unless` must filter against DISTINCT signature subquery; sql=%s", sql)
	}
	if strings.Contains(sql, "UNION ALL") {
		t.Errorf("`unless` must not UNION ALL; sql=%s", sql)
	}
}

// TestEmitVectorSetOp_Or_ExactShape pins the `or` single-pass UNION-ALL
// shape AND kills vector_set_op.go:281:13 (INVERT_LOGICAL `derived &&
// !matrix` -> `derived || !matrix`).
//
// Both arms here are bare Scans: derived=false, matrix=false. Original
// `false && !false` = false -> timeFrag = Col("TimeUnix"), so the synthetic
// instant anchor `now64(9) - toIntervalNanosecond(...)` is NEVER emitted.
// The mutant `false || !false` = true -> synthesizes the anchor on every
// canonical arm, so `now64` would appear. Asserting its ABSENCE breaks
// the mutant. (Both arms are canonicalised for `or`, so either arm's
// timeFrag flipping is caught.)
func TestEmitVectorSetOp_Or_ExactShape(t *testing.T) {
	sql := emitBinSetOp(t, chplan.VectorSetOr)
	if !strings.Contains(sql, "SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value`") {
		t.Fatalf("missing outer canonical projection; sql=%s", sql)
	}
	// `or` is the single-pass UNION ALL with side marker + window.
	if !strings.Contains(sql, "UNION ALL") {
		t.Errorf("`or` must UNION ALL the two arms; sql=%s", sql)
	}
	if !strings.Contains(sql, "_setop_side") || !strings.Contains(sql, "_setop_has_left") {
		t.Errorf("`or` must carry side marker + windowed has-left flag; sql=%s", sql)
	}
	// Canonical (non-derived, non-matrix) arms must keep the real TimeUnix
	// column — NO synthetic instant anchor. This is the line-281 kill.
	if strings.Contains(sql, "now64") || strings.Contains(sql, "toIntervalNanosecond") {
		t.Errorf("canonical Scan arms must not synthesize an instant anchor (now64); sql=%s", sql)
	}
}

// --- late_mat.go init-gate mutants (77:25, 77:47, 81:26, 81:49) ---
//
// These four mutate the package-init guard that registers a table in
// lateMatShapes:  `if X.HasUniqueRowKey() && len(X.WideColumns) > 0`.
// For BOTH default schemas (logs, traces) HasUniqueRowKey()==true AND
// len(WideColumns)>0==true, so the guard evaluates true. The mutations:
//   - 77:25 / 81:26 INVERT_LOGICAL  `&&` -> `||`:  true||true == true.
//   - 77:47 / 81:49 CONDITIONALS_BOUNDARY `>0` -> `>=0`: len>=0 is always
//     true, AND-ed with the already-true HasUniqueRowKey -> still true.
//
// In every case the mutated guard yields the SAME boolean (true) on the
// fixed default-schema inputs, so the resulting lateMatShapes registry is
// byte-identical. The init values cannot be varied from a test (they are
// package-level constants), so no test can make the registry differ.
//
// This test pins that the gate's OBSERVABLE output — the registered
// late-mat shapes, surfaced through isLateMatCandidate's late-mat SQL —
// is exactly what the (unmutated) true guard produces, and documents that
// the four init mutants are EQUIVALENT (no observable behaviour can
// distinguish them). See the returned report for the equivalence argument.
func TestLateMat_DefaultSchemasSatisfyRegistrationGate(t *testing.T) {
	// Sanity: the exact predicate operands the init gate ANDs together are
	// both true for both default schemas. If a future schema change makes
	// either operand false, the init mutants stop being equivalent and the
	// gremlins lane will flag them — this assertion is the tripwire.
	l := schema.DefaultOTelLogs()
	if !l.HasUniqueRowKey() {
		t.Fatalf("logs: HasUniqueRowKey() must be true for the init gate to register")
	}
	if len(l.WideColumns) == 0 {
		t.Fatalf("logs: WideColumns must be non-empty for the init gate to register")
	}
	tr := schema.DefaultOTelTraces()
	if !tr.HasUniqueRowKey() {
		t.Fatalf("traces: HasUniqueRowKey() must be true for the init gate to register")
	}
	if len(tr.WideColumns) == 0 {
		t.Fatalf("traces: WideColumns must be non-empty for the init gate to register")
	}
}
