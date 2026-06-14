package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// parseLimitRatio parses a `limit_ratio(...)` query with the
// experimental-aggregator gate open (as the prom handler does).
func parseLimitRatio(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// TestLowerLimitRatio_Structure pins the lowered shape: a top-level
// Filter whose predicate is the ratio comparison over the per-series
// hash offset. The offset must be reconstructed from xxHash64 over the
// canonical label encoding (the parity-critical detail), and the
// `__name__` map key must be an inline literal (not a `?` placeholder,
// which would break CH's concat/arrayConcat overload resolution).
func TestLowerLimitRatio_Structure(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	expr := parseLimitRatio(t, `limit_ratio(0.5, up)`)
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("top node = %T, want *chplan.Filter", plan)
	}
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok {
		t.Fatalf("predicate = %T, want *chplan.Binary", f.Predicate)
	}
	if b.Op != chplan.OpLt {
		t.Errorf("ratio>=0 predicate op = %q, want %q (offset < r)", b.Op, chplan.OpLt)
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{"xxHash64", "arrayStringConcat", "mapKeys", "mapConcat"} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q (offset reconstruction)\nSQL: %s", want, sql)
		}
	}
	// `__name__` must be inline (`'__name__'`), never a bound `?` key:
	// a placeholder key leaves the map type indeterminate and CH
	// mis-dispatches the downstream concat to arrayConcat.
	if !strings.Contains(sql, "'__name__'") {
		t.Errorf("emitted SQL must carry the inline '__name__' literal\nSQL: %s", sql)
	}
}

// TestLowerLimitRatio_NegativeIsComplement pins that a negative ratio
// lowers to the complement predicate `offset >= 1 + r` (Prometheus's
// negative-ratio "complement" semantics) rather than the `offset < r`
// form used for non-negative ratios.
func TestLowerLimitRatio_NegativeIsComplement(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	plan, err := promql.Lower(context.Background(), parseLimitRatio(t, `limit_ratio(-0.5, up)`), s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	f := plan.(*chplan.Filter)
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok {
		t.Fatalf("predicate = %T, want *chplan.Binary", f.Predicate)
	}
	if b.Op != chplan.OpGe {
		t.Errorf("negative-ratio predicate op = %q, want %q (offset >= 1+r)", b.Op, chplan.OpGe)
	}
	rhs, ok := b.Right.(*chplan.LitFloat)
	if !ok {
		t.Fatalf("predicate RHS = %T, want *chplan.LitFloat", b.Right)
	}
	if rhs.V != 0.5 {
		t.Errorf("complement threshold = %v, want 0.5 (1 + (-0.5))", rhs.V)
	}
}

// TestLowerLimitRatio_ZeroEmpty pins that `limit_ratio(0, ...)` folds to
// a constant-false Filter (Prometheus returns early on an all-zero
// ratio), keeping the canonical column shape.
func TestLowerLimitRatio_ZeroEmpty(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	plan, err := promql.Lower(context.Background(), parseLimitRatio(t, `limit_ratio(0, up)`), s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("top node = %T, want *chplan.Filter", plan)
	}
	lb, ok := f.Predicate.(*chplan.LitBool)
	if !ok || lb.V {
		t.Errorf("ratio=0 predicate = %#v, want LitBool{false}", f.Predicate)
	}
}

// TestLowerLimitRatio_NaNRejected pins that a NaN ratio is rejected at
// lowering, matching reference Prometheus (`Ratio value is NaN`).
func TestLowerLimitRatio_NaNRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	_, err := promql.Lower(context.Background(), parseLimitRatio(t, `limit_ratio(NaN, up)`), s)
	if err == nil {
		t.Fatal("Lower(limit_ratio(NaN, up)): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "NaN") {
		t.Errorf("error = %q, want it to mention NaN", err.Error())
	}
}

// TestLowerLimitRatio_ComputedRatio pins the computed-ratio path: a
// non-literal scalar ratio (here `scalar(vector(0.5))`) lowers to the
// full runtime predicate `(r>=0 AND off<r) OR (r<0 AND off>=1+r)` —
// an OR of two AND arms — rather than a single sign-resolved comparison.
// The ratio must NOT be rejected (Prometheus accepts any scalar-valued
// ratio), so this also guards against re-introducing a wrong-rejection.
func TestLowerLimitRatio_ComputedRatio(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	plan, err := promql.Lower(context.Background(), parseLimitRatio(t, `limit_ratio(scalar(vector(0.5)), up)`), s)
	if err != nil {
		t.Fatalf("Lower(computed ratio): %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("top node = %T, want *chplan.Filter", plan)
	}
	or, ok := f.Predicate.(*chplan.Binary)
	if !ok || or.Op != chplan.OpOr {
		t.Fatalf("computed-ratio predicate = %#v, want top-level OR", f.Predicate)
	}
	pos, okp := or.Left.(*chplan.Binary)
	neg, okn := or.Right.(*chplan.Binary)
	if !okp || pos.Op != chplan.OpAnd || !okn || neg.Op != chplan.OpAnd {
		t.Errorf("computed-ratio arms = (%#v, %#v), want two AND arms", or.Left, or.Right)
	}
}

// TestLowerLimitRatio_OutOfRangeKeepsAll pins that |r| >= 1 keeps every
// series: Prometheus only warns (no clamp) for ratios outside [-1, 1],
// so the raw comparison `offset < r` (r > 1) is always true. The
// lowering must NOT special-case these — the predicate is the plain
// comparison, which selects all rows at query time.
func TestLowerLimitRatio_OutOfRangeKeepsAll(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	plan, err := promql.Lower(context.Background(), parseLimitRatio(t, `limit_ratio(2, up)`), s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("top node = %T, want *chplan.Filter", plan)
	}
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok {
		t.Fatalf("predicate = %T, want *chplan.Binary (not a constant fold)", f.Predicate)
	}
	rhs, ok := b.Right.(*chplan.LitFloat)
	if !ok || rhs.V != 2.0 {
		t.Errorf("r>1 threshold = %#v, want LitFloat{2}", b.Right)
	}
}
