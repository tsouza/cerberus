//go:build chdb

// chDB-backed proof that absent() over a mixed float/histogram `or`
// (cerberus issue #2613) reaches a real row source instead of erroring.
//
// Only the MATCHING case is exercised here, deliberately: absent()'s
// "0 rows -> synthesise {} 1" wrapping (the DropEmptyOnNoGroup=false
// Aggregate + outer Filter/Project chain — see absent.go's own doc
// comment) is pre-existing logic this issue's fix never touches; only the
// INNER row-source lowering changed, from a bare `lower()` call that hit
// binary.go's mixed-or catch-all to `lowerVectorSetOpOperand`. Proving
// that change is "does the previously-erroring mixed-or inner now
// execute, and count real rows" — which the matching case answers
// completely: a bare `lower()` call over this exact expression errors at
// Emit() before any SQL reaches ClickHouse, so a clean 0-row result here
// is only reachable through the new dispatch. Re-deriving absent()'s own
// 0-row synthesis path (already exercised for other input shapes
// elsewhere in this package) would test old, unmodified machinery rather
// than this fix.
package promql_test

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

func TestAbsentOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, scaleWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := `absent(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `)`

	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, scaleWrappedEvalTS, scaleWrappedEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v (a bare lower() call — the pre-fix behaviour — must fail exactly here, before any SQL reaches ClickHouse)", query, err)
	}

	rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != 0 {
		t.Errorf("%s: got %d row(s), want 0 — both arms of the mixed-or match real seeded data, so absent() must report present", query, n)
	}
}
