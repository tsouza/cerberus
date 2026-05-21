package logql

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerVectorWrapsValueInToFloat64 pins the `toFloat64(?)` wrap
// of [lowerVector]'s Value projection on the emitted SQL. Without
// the wrap, `vector(1) + vector(1)` (Grafana's Loki CheckHealth
// probe) surfaces as a 502 with `converting UInt16 to *float64 is
// unsupported`: the clickhouse-go/v2 driver renders Go's
// `float64(1.0)` as the SQL literal `1` (no decimal, via its
// fallback `fmt.Sprint`), CH narrows it to `UInt8`, and
// `UInt8 OP UInt8` promotes to `UInt16` on the VectorJoin output —
// which the chclient cursor refuses to scan into
// `chclient.Sample.Value` (`*float64`).
//
// Post-#190 the wrap is contributed centrally by
// [internal/chsql/Builder.Expr]'s LitFloat case, so the lowering
// emits a bare `*chplan.LitFloat` and the SQL surface carries the
// wrap. Pinning on the rendered SQL means a future refactor that
// drops the central wrap immediately surfaces here.
func TestLowerVectorWrapsValueInToFloat64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`vector(1)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	vec, ok := expr.(*syntax.VectorExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *syntax.VectorExpr", expr)
	}
	node, err := lowerVector(vec, s)
	if err != nil {
		t.Fatalf("lowerVector: %v", err)
	}
	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("lowerVector returned %T, want *chplan.Project", node)
	}
	if len(proj.Projections) != 2 {
		t.Fatalf("Project: got %d projections, want 2", len(proj.Projections))
	}
	valueProj := proj.Projections[1]
	if valueProj.Alias != rangeAggSynthValueColumn {
		t.Fatalf("Value projection alias: got %q, want %q",
			valueProj.Alias, rangeAggSynthValueColumn)
	}
	sql, _, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "toFloat64(?) AS `Value`") {
		t.Fatalf("expected emitted SQL to wrap LitFloat in toFloat64(?) — "+
			"without it, CH narrows to UInt8 and the Sample Scan fails "+
			"with `UInt16 to *float64 unsupported` on V-V binops. "+
			"SQL:\n%s", sql)
	}
}

// TestLowerLiteralWrapsValueInToFloat64 mirrors
// [TestLowerVectorWrapsValueInToFloat64] for the [lowerLiteral] path
// (bare numeric literal — e.g. the `1` in `1 * count_over_time(...)`,
// which reaches the lowering pass when one leg of a binop is a
// LiteralExpr).
func TestLowerLiteralWrapsValueInToFloat64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`5`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	lit, ok := expr.(*syntax.LiteralExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *syntax.LiteralExpr", expr)
	}
	node, err := lowerLiteral(lit, s)
	if err != nil {
		t.Fatalf("lowerLiteral: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "toFloat64(?) AS `Value`") {
		t.Fatalf("expected emitted SQL to wrap LitFloat in toFloat64(?). "+
			"SQL:\n%s", sql)
	}
}
