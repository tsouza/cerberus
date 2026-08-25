//go:build chdb

// chDB-backed proof that `timestamp()` directly wrapping a mixed
// float/histogram `or` (cerberus issue #2611,
// histogram_native_mixed_or_timestamp.go's
// [timestampOverMixedExpHistogramSetOp] /
// [lowerTimestampOverMixedExpHistogramSetOp]) reproduces reference
// Prometheus's funcTimestamp at real ClickHouse execution: BOTH the
// float-shaped rows AND the histogram-shaped rows survive (the opposite
// composition from sort()/sort_desc(), which drop every histogram-shaped
// row), and every surviving row — regardless of shape — reports the
// SAME evaluation-instant value, since a mixed `or` is a BinaryExpr, never
// a bare VectorSelector.
//
// Reuses foSeed / foHistMetric / foFloatMetric / foEvalTS from
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go and
// tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go (same package,
// same build tag).
package promql_test

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// tsSeriesValues runs query and returns a map of `series` label to
// `Value`, asserting the plan root stays [chplan.SampleRowShape] —
// timestamp() always collapses to a plain float vector, even over a
// mixed `or` whose histogram-shaped rows survive (they carry no
// Histogram*Column output in timestamp()'s own answer, unlike
// sort_by_label()'s preserve-verbatim composition).
func tsSeriesValues(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]float64 {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (timestamp() always answers a plain float vector)", query, shape, chplan.SampleRowShape)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	got := map[string]float64{}
	for rows.Next() {
		var series string
		var val float64
		if err := rows.Scan(&series, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[series] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestTimestampOverMixedSetOpOr_ChDB proves timestamp(), wrapped directly
// around a mixed `or`, answers ALL FIVE rows — the three float-shaped
// rows (f1, f2, f3) AND the two histogram-shaped rows (h1, h2) alike —
// each reporting the SAME evaluation-instant value (foEvalTS, in
// fractional Unix seconds), for both source-AST operand orders. A
// histogram-blind bug (dropping h1/h2, mirroring sort()/sort_desc()'s
// OWN correct behaviour) would answer only three rows here instead of
// five; a bug reading a per-row raw sample timestamp instead of the
// shared evaluation instant would answer a DIFFERENT value per row
// instead of the same one.
func TestTimestampOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	wantTS := float64(foEvalTS.Unix())
	want := map[string]float64{
		"f1": wantTS, "f2": wantTS, "f3": wantTS,
		"h1": wantTS, "h2": wantTS,
	}

	for _, order := range []struct {
		name  string
		query string
	}{
		{"histLHS", "timestamp(" + foHistMetric + " or " + foFloatMetric + ")"},
		{"floatLHS", "timestamp(" + foFloatMetric + " or " + foHistMetric + ")"},
	} {
		t.Run(order.name, func(t *testing.T) {
			got := tsSeriesValues(t, fixture, s, p, order.query)
			if len(got) != len(want) {
				t.Fatalf("query %q: got %d rows %v, want %d rows %v", order.query, len(got), got, len(want), want)
			}
			for series, wantVal := range want {
				gotVal, ok := got[series]
				if !ok {
					t.Errorf("query %q: missing series %q", order.query, series)
					continue
				}
				if gotVal != wantVal {
					t.Errorf("query %q: series %q value = %v, want %v", order.query, series, gotVal, wantVal)
				}
			}
		})
	}
}

// TestTimestampOverMixedSetOpOr_ShadowCollision_ChDB proves the `or`'s
// own LHS-wins shadow rule composes correctly with timestamp()'s
// preserve-both-arms rule: when the histogram side is the source-AST
// LHS, its "dup" row survives (and reports the evaluation instant like
// every other row), the float side's own colliding "dup" row is
// shadowed out, and only "solo" survives on the float side.
func TestTimestampOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	wantTS := float64(foEvalTS.Unix())
	query := "timestamp(" + tkShadowHistMetric + " or " + tkShadowFloatMetric + ")"
	got := tsSeriesValues(t, fixture, s, p, query)
	want := map[string]float64{"dup": wantTS, "solo": wantTS}
	if len(got) != len(want) {
		t.Fatalf("query %q: got %d rows %v, want %d rows %v", query, len(got), got, len(want), want)
	}
	for series, wantVal := range want {
		gotVal, ok := got[series]
		if !ok {
			t.Errorf("query %q: missing series %q", query, series)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: series %q value = %v, want %v", query, series, gotVal, wantVal)
		}
	}
}

// TestTimestampOverMixedSetOpOr_NestedUnderWrapper_ChDB proves the
// composition applies when timestamp() is reached NESTED under a
// further wrapper, not only at the query root — mirroring
// TestSortOverMixedSetOpOr_NestedUnderWrapper_ChDB's own precedent.
func TestTimestampOverMixedSetOpOr_NestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "abs(timestamp(" + foHistMetric + " or " + foFloatMetric + "))"
	got := tsSeriesValues(t, fixture, s, p, query)
	wantTS := float64(foEvalTS.Unix())
	want := map[string]float64{
		"f1": wantTS, "f2": wantTS, "f3": wantTS,
		"h1": wantTS, "h2": wantTS,
	}
	if len(got) != len(want) {
		t.Fatalf("query %q: got %d rows %v, want %d rows %v", query, len(got), got, len(want), want)
	}
	for series, wantVal := range want {
		gotVal, ok := got[series]
		if !ok {
			t.Errorf("query %q: missing series %q", query, series)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: series %q value = %v, want %v", query, series, gotVal, wantVal)
		}
	}
}
