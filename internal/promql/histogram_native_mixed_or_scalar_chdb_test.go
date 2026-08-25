//go:build chdb

// chDB-backed proof that `scalar()` directly wrapping a mixed
// float/histogram `or` (cerberus issue #2611,
// histogram_native_mixed_or_scalar.go's [scalarArgOverMixedExpHistogramSetOp] /
// [lowerScalarArgOverMixedExpHistogramSetOp]) reproduces reference
// Prometheus's funcScalar at real ClickHouse execution: a histogram-shaped
// row is invisible to the "exactly one sample" count entirely, the `or`'s
// own LHS-wins shadow rule composes correctly with that count, and zero
// surviving float rows answers NaN exactly like the non-mixed
// all-histogram case already does.
//
// Reuses tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go (same package,
// same build tag): a histogram-shaped "dup" row and a float-shaped "dup"
// row share an identical label signature (the shadow-collision case),
// plus a non-colliding float-shaped "solo" row.
package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// scScalarValue runs query — expected to lower to scalar()'s own
// single-row synthetic-vector contract — and returns its Value.
func scScalarValue(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) float64 {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query %q: expected exactly one row, got zero", query)
	}
	var val float64
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows.Next() {
		t.Fatalf("query %q: expected exactly one row, got more than one", query)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return val
}

// TestScalarOverMixedSetOpOr_ShadowCollision_ChDB proves scalar() over a
// mixed `or` counts only float-shaped rows, and that the `or`'s own
// LHS-wins shadow rule composes with that count correctly: when the
// histogram side is the source-AST LHS, the float "dup" row is shadowed
// out, leaving exactly one surviving float row ("solo"=7) — scalar()
// answers 7. When the float side is LHS instead, both "dup"=42 and
// "solo"=7 survive as float rows — more than one, so scalar() answers
// NaN, exactly as it would for a plain (non-histogram) two-element
// vector, and exactly as it would if the histogram "dup" row did not
// exist at all (proving the histogram row is truly invisible to the
// count, not merely excluded from being "the" answer).
func TestScalarOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		{
			"histLHS_dupShadowedOut_singleFloatSurvives",
			"scalar(" + tkShadowHistMetric + " or " + tkShadowFloatMetric + ")",
			7,
		},
		{
			"floatLHS_dupSurvives_twoFloatsAnswerNaN",
			"scalar(" + tkShadowFloatMetric + " or " + tkShadowHistMetric + ")",
			math.NaN(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scScalarValue(t, fixture, s, p, tc.query)
			if math.IsNaN(tc.want) {
				if !math.IsNaN(got) {
					t.Errorf("query %q: got %v, want NaN", tc.query, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("query %q: got %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestScalarOverMixedSetOpOr_ZeroFloatRows_ChDB proves scalar() over a
// mixed `or` whose float side matches ZERO rows answers NaN — the same
// "zero float samples" branch the non-mixed all-histogram case already
// exercises, now proven with a histogram-shaped row genuinely present
// (and genuinely ignored) alongside the empty float side, rather than
// the float side being entirely absent from the query.
func TestScalarOverMixedSetOpOr_ZeroFloatRows_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "scalar(" + tkShadowHistMetric + " or " + tkShadowFloatMetric + `{series="nonexistent"})`
	got := scScalarValue(t, fixture, s, p, query)
	if !math.IsNaN(got) {
		t.Errorf("query %q: got %v, want NaN (zero float-shaped rows survive, one histogram-shaped row does but must be invisible)", query, got)
	}
}

// TestScalarOverMixedSetOpOr_NestedUnderWrapper_ChDB proves the
// composition applies when scalar() is reached NESTED under a further
// wrapper, not only at the query root — mirroring
// TestSortOverMixedSetOpOr_NestedUnderWrapper_ChDB's own precedent.
// clamp_min's second argument is an arbitrary scalar-embedding position
// distinct from vector(scalar(...)) already covered by
// scalar_args_test.go's non-mixed cases.
func TestScalarOverMixedSetOpOr_NestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "clamp_min(" + tkShadowFloatMetric + `{series="solo"}, scalar(` + tkShadowHistMetric + " or " + tkShadowFloatMetric + "))"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query %q: expected exactly one row, got zero", query)
	}
	var val float64
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	// solo=7, hist "dup"=(count=2,sum=4.0) is invisible to scalar()'s
	// count, "dup" float row is shadowed out (hist is LHS) leaving
	// "solo"=7 as the only float sample: scalar(...) = 7, clamp_min(7,
	// 7) = 7.
	const want = 7.0
	if val != want {
		t.Errorf("query %q: got %v, want %v", query, val, want)
	}
}
