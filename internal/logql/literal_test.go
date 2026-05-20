package logql

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerVectorWrapsValueInToFloat64 pins the [synthFloatValue]
// wrap in [lowerVector]'s Value projection. Without the wrap,
// `vector(1) + vector(1)` (Grafana's Loki CheckHealth probe) surfaces
// as a 502 with `converting UInt16 to *float64 is unsupported`: the
// clickhouse-go/v2 driver renders Go's `float64(1.0)` as the SQL
// literal `1` (no decimal, via its fallback `fmt.Sprint`), CH narrows
// it to `UInt8`, and `UInt8 OP UInt8` promotes to `UInt16` on the
// VectorJoin output — which the chclient cursor refuses to scan into
// `chclient.Sample.Value` (`*float64`).
//
// Pin the wrap at the lowering site so a future refactor can't drop
// it without breaking the Grafana datasource health probe. Mirrors
// [internal/promql/absent.go]'s identical toFloat64 wrap of the
// absent-value spec literal.
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
	fc, ok := valueProj.Expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("Value expr: got %T, want *chplan.FuncCall — the "+
			"toFloat64 wrap is missing and CH will type the column as "+
			"UInt8, surfacing as `UInt16 to *float64 unsupported` on "+
			"any V-V binop over synthetic scalars (Grafana Loki "+
			"CheckHealth probe `vector(1)+vector(1)`)", valueProj.Expr)
	}
	if fc.Name != "toFloat64" {
		t.Errorf("Value expr func name: got %q, want %q", fc.Name, "toFloat64")
	}
	if len(fc.Args) != 1 {
		t.Fatalf("toFloat64 args: got %d, want 1", len(fc.Args))
	}
	if lf, ok := fc.Args[0].(*chplan.LitFloat); !ok {
		t.Fatalf("toFloat64 arg: got %T, want *chplan.LitFloat", fc.Args[0])
	} else if lf.V != 1.0 {
		t.Errorf("toFloat64 arg LitFloat.V: got %v, want 1.0", lf.V)
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
	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("lowerLiteral returned %T, want *chplan.Project", node)
	}
	valueProj := proj.Projections[1]
	fc, ok := valueProj.Expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("Value expr: got %T, want *chplan.FuncCall — toFloat64 wrap missing", valueProj.Expr)
	}
	if fc.Name != "toFloat64" {
		t.Errorf("Value expr func name: got %q, want %q", fc.Name, "toFloat64")
	}
}
