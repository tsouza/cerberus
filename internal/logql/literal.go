package logql

import (
	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerLiteral handles LogQL's `LiteralExpr` — a bare numeric literal
// like `5`, `3.14`, or `-2.5` used as the leg of a binary expression
// (`5 * count_over_time({...}[5m])`) or as a standalone metric query
// (Grafana's `1+1` datasource health probe folds to a single
// LiteralExpr by Loki's parser-time `reduceBinOp`).
//
// LogQL doesn't have PromQL's `vector(scalar)` shortcut; the only way
// a literal reaches the lowering pass is through one of those paths.
// Mirror the PromQL `synthesizedScalarVector` shape: a Project over
// `chplan.OneRow` materialising a 1-row synthetic vector whose
// `Value` is the literal and whose stream-identity (ResourceAttributes)
// is the empty map. Lang.ProjectSamples re-shapes this into the canonical
// chclient.Sample contract on the engine side.
//
// The synthesised row carries no MetricName / TimeUnix columns — the
// LogQL pipeline only ever consumes ResourceAttributes + Value out of
// the inner plan, and Lang.ProjectSamples synthesises the wire-wrap
// MetricName + TimeUnix from `now64(9)` rather than from the row.
//
// Loki's parser stashes any "unparseable float" failure inside
// LiteralExpr's unexported `err` field; the public `Value()` accessor
// surfaces it, so we use that as the canonical readiness check
// instead of touching `Val` directly.
func lowerLiteral(e *syntax.LiteralExpr, s schema.Logs) (chplan.Node, error) {
	v, err := e.Value()
	if err != nil {
		return nil, err
	}
	return syntheticLogScalar(synthFloatValue(v), s), nil
}

// lowerVector handles LogQL's `VectorExpr` — `vector(5)` and friends.
// Loki's parser only accepts a scalar float literal in this position
// (`VectorExpr.Val float64`), so the lowering produces the same 1-row
// synthetic vector as [lowerLiteral] with the parsed value as Value.
func lowerVector(e *syntax.VectorExpr, s schema.Logs) (chplan.Node, error) {
	v, err := e.Value()
	if err != nil {
		return nil, err
	}
	return syntheticLogScalar(synthFloatValue(v), s), nil
}

// synthFloatValue wraps a float literal in `toFloat64(...)` so the
// emitted Value column is Float64 on the wire regardless of what CH
// types the bound parameter as. Without the wrap, the clickhouse-go/v2
// driver renders an integer-valued `float64(1.0)` as the SQL literal
// `1` (no decimal — its `format()` falls through to `fmt.Sprint(v)`
// which uses Go's `%v` for float64 and prints `1` for whole numbers).
// CH narrows that to `UInt8`, and `UInt8 OP UInt8` promotes to
// `UInt16`. Once it lands in `chclient.Sample.Value` (declared `float64`),
// the driver refuses the conversion with
// `converting UInt16 to *float64 is unsupported. try using *uint16`
// — surfaced as the Grafana Loki datasource health-probe 502 on
// `vector(1)+vector(1)`. Wrapping in `toFloat64(?)` forces CH to keep
// the column as Float64 through any downstream arithmetic.
//
// Mirrors the same wrap [internal/promql/absent.go] uses for the
// PromQL `absent(...)` Value(1) shape (added under the same UInt8 ->
// *float64 failure mode).
func synthFloatValue(v float64) chplan.Expr {
	return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitFloat{V: v}}}
}

// syntheticLogScalar builds a Project-over-OneRow that materialises a
// single LogQL metric-shape row with empty ResourceAttributes and the
// supplied Value. Used by [lowerLiteral] and [lowerVector] — both are
// LogQL constructs that produce one labelled-empty sample per
// evaluation, mirroring PromQL's `time()` / `vector(scalar)` plan
// shape but emitting only the LogQL-shape (ResourceAttributes, Value)
// the rest of the LogQL pipeline consumes.
//
// Lang.ProjectSamples wraps the engine output with synthetic
// MetricName + TimeUnix, so the inner plan only carries the columns
// the LogQL surface uses.
func syntheticLogScalar(valueExpr chplan.Expr, s schema.Logs) chplan.Node {
	return &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: emptyAttrsMap(), Alias: s.ResourceAttributesColumn},
			{Expr: valueExpr, Alias: rangeAggSynthValueColumn},
		},
	}
}
