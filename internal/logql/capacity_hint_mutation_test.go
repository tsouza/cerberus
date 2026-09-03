// Capacity-hint adjudication for internal/logql.
//
// Every hint in this file sizes a slice that becomes an EXPORTED field of a
// value its builder returns — a `chplan.FuncCall`'s `Args` or a
// `chplan.Project`'s `Projections` — and none of them grows past its
// pre-allocation, so `cap` reads the hint's arithmetic straight back and a
// plain assertion kills every `ARITHMETIC_BASE` substitution gremlins can
// make. `docs/test-strategy.md`'s "When a capacity mutant is equivalent"
// states the rule; the two hints in detected_level_test.go were the worked
// instances it was derived from, and this file applies the same check to the
// package's remaining capacity survivors.
//
// Each test names the escape surface it reads the capacity back through.
// [capmutant.AssertKilled] then replays every operator substitution against
// the builder's own append sequence and requires each one to move the finished
// capacity, so "the assertion discriminates" is re-run rather than asserted.
package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/capmutant"
)

// capHintExprArgs reads the `Args` slice of the [chplan.FuncCall] e is, failing
// the test when the expression is some other shape — a navigation that silently
// answered an empty slice would report a capacity nobody allocated.
func capHintExprArgs(t *testing.T, e chplan.Expr, what string) []chplan.Expr {
	t.Helper()

	fn, ok := e.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("%s is %T; want *chplan.FuncCall", what, e)
	}
	return fn.Args
}

// TestWrapLabelsWithMarks_CapHintMutantsKilled kills the two ARITHMETIC_BASE
// mutants gremlins reports on duration.go:`len(marks)*2+1`.
//
// Escape surface: [wrapLabelsWithMarks] returns a `mapMerge` FuncCall whose
// second argument is the `multiIf` cascade this hint sizes, so the slice is
// reachable as an exported `Args` field two levels down from the return value.
func TestWrapLabelsWithMarks_CapHintMutantsKilled(t *testing.T) {
	t.Parallel()

	// Four marks keeps the cascade on its multiIf branch (a single mark
	// switches wrapLabelsWithMarks to `if`, which builds the same slice but
	// stops being the shape the mutant's own callers exercise), and no mutated
	// hint's growth schedule lands back on the true capacity at this size.
	const marks = 4

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "duration.go:`len(marks)*2+1`",
		Positions: []capmutant.Position{
			{Name: "the `*2`", Op: "*"},
			{Name: "the trailing `+1`", Op: "+"},
		},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{marks, 2, 1}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			merged := wrapLabelsWithMarks(&chplan.ColumnRef{Name: "labels"}, capHintMarks(marks))
			outer := capHintExprArgs(t, merged, "wrapLabelsWithMarks result")
			if len(outer) != 2 {
				t.Fatalf("mapMerge carries %d args; want the labels expression and the cascade", len(outer))
			}
			args := capHintExprArgs(t, outer[1], "the mark cascade")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			// One (condition, value) pair per mark, then the trailing empty-map
			// branch: the grouping of the appends steers append's growth
			// schedule, so the replay mirrors it call for call.
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < marks; i++ {
				args = append(args, nil, nil)
			}
			args = append(args, nil)
			return len(args), cap(args)
		},
	})
}

// capHintMarks builds n distinct [labelFilterMark]s. The values are immaterial
// to a capacity — only the count is — but they are distinct so a shape
// assertion added later reads something meaningful.
func capHintMarks(n int) []labelFilterMark {
	marks := make([]labelFilterMark, 0, n)
	for i := 0; i < n; i++ {
		marks = append(marks, labelFilterMark{
			cond:    &chplan.LitInt{V: int64(i)},
			kind:    "SampleExtractionErr",
			details: &chplan.LitString{V: "detail"},
		})
	}
	return marks
}

// TestMergeParsedFields_CapHintMutantKilled kills the ARITHMETIC_BASE mutant
// gremlins reports on lower.go:`len(fields)*2`.
//
// Escape surface: [mergeParsedFields] returns a `mapMerge` FuncCall whose
// second argument is the `map(...)` literal this hint sizes, so the slice is
// its exported `Args`.
func TestMergeParsedFields_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const fields = 5

	s := schema.DefaultOTelLogs()

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "lower.go:`len(fields)*2`",
		Positions: []capmutant.Position{{Name: "the `*2`", Op: "*"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{fields, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			parsed := make([]parsedField, 0, fields)
			for i := 0; i < fields; i++ {
				parsed = append(parsed, parsedField{
					name:  string(rune('a' + i)),
					value: &chplan.LitString{V: "v"},
				})
			}
			merged := mergeParsedFields(&chplan.ColumnRef{Name: s.AttributesColumn}, s, parsed)
			outer := capHintExprArgs(t, merged, "mergeParsedFields result")
			if len(outer) != 2 {
				t.Fatalf("mapMerge carries %d args; want the previous labels and the map literal", len(outer))
			}
			args := capHintExprArgs(t, outer[1], "the parsed-field map literal")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < fields; i++ {
				args = append(args, nil, nil) // (renamed key, value)
			}
			return len(args), cap(args)
		},
	})
}

// TestJSONExtractStringExpr_CapHintMutantKilled kills the ARITHMETIC_BASE
// mutant gremlins reports on lower.go:`len(segments)+1`.
//
// Escape surface: the slice IS the returned `JSONExtractString` FuncCall's
// exported `Args`.
func TestJSONExtractStringExpr_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	// A five-segment path; the leading Body column makes six arguments.
	const (
		path     = "a.b.c.d.e"
		segments = 5
	)

	s := schema.DefaultOTelLogs()

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "lower.go:`len(segments)+1`",
		Positions: []capmutant.Position{{Name: "the `+1`", Op: "+"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{segments, 1}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			expr, err := jsonExtractStringExpr(s, path)
			if err != nil {
				t.Fatalf("jsonExtractStringExpr(%q): %v", path, err)
			}
			args := capHintExprArgs(t, expr, "jsonExtractStringExpr result")
			if len(args) != segments+1 {
				t.Fatalf("JSONExtractString carries %d args; want the Body column plus "+
					"%d path segments — the fixture path no longer parses into the "+
					"segment count this hint's operands assume", len(args), segments)
			}
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			args = append(args, nil) // the Body column
			for i := 0; i < segments; i++ {
				args = append(args, nil)
			}
			return len(args), cap(args)
		},
	})
}

// TestRangeAggregationGroupBy_CapHintMutantKilled kills the ARITHMETIC_BASE
// mutant gremlins reports on
// range_aggregation.go:`len(e.Grouping.Groups)*2`.
//
// Escape surface: the slice IS the returned `map(...)` FuncCall's exported
// `Args`.
func TestRangeAggregationGroupBy_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const (
		query  = `avg_over_time({app="api"} | logfmt | unwrap latency [5m]) by (region, tenant, env, zone, shard)`
		groups = 5
	)

	s := schema.DefaultOTelLogs()
	ra := parseRangeAggregationForCapHint(t, query, groups)

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "range_aggregation.go:`len(e.Grouping.Groups)*2`",
		Positions: []capmutant.Position{{Name: "the `*2`", Op: "*"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{groups, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			got, err := rangeAggregationGroupBy(ra, s, &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
				func() chplan.Expr { return detectedLevelIdentityExpr(s, ra.Left.Left) })
			if err != nil {
				t.Fatalf("rangeAggregationGroupBy: %v", err)
			}
			args := capHintExprArgs(t, got, "rangeAggregationGroupBy result")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < groups; i++ {
				args = append(args, nil, nil) // (label literal, group-key value)
			}
			return len(args), cap(args)
		},
	})
}

// TestWrapVectorAggregateForSample_CapHintMutantKilled kills the
// ARITHMETIC_BASE mutant gremlins reports on
// vector_aggregation.go:`len(e.Grouping.Groups)*2`.
//
// Escape surface: the slice becomes the `map(...)` FuncCall the returned
// [chplan.Project] projects as its Attributes column, so it is reachable as an
// exported `Args` under an exported `Projections`.
func TestWrapVectorAggregateForSample_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const (
		query  = `sum by (region, tenant, env, zone, shard) (rate({app="api"}[5m]))`
		groups = 5
	)

	s := schema.DefaultOTelLogs()

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	va, ok := expr.(*syntax.VectorAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.VectorAggregationExpr", query, expr)
	}
	if va.Grouping == nil || len(va.Grouping.Groups) != groups {
		t.Fatalf("fixture must group on exactly %d labels; got %v", groups, va.Grouping)
	}

	aliases := make([]string, groups)
	for i := range aliases {
		aliases[i] = string(rune('a' + i))
	}

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "vector_aggregation.go:`len(e.Grouping.Groups)*2`",
		Positions: []capmutant.Position{{Name: "the `*2`", Op: "*"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{groups, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			node := wrapVectorAggregateForSample(&chplan.Aggregate{}, va, s, aliases, false, "")
			proj, ok := node.(*chplan.Project)
			if !ok {
				t.Fatalf("wrapVectorAggregateForSample -> %T; want *chplan.Project", node)
			}
			attrs := findProjectionExpr(t, proj, sampleAttributesCol)
			args := capHintExprArgs(t, attrs, "the Attributes map literal")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < groups; i++ {
				args = append(args, nil, nil) // (label literal, alias column)
			}
			return len(args), cap(args)
		},
	})
}

// parseRangeAggregationForCapHint parses query and asserts it groups on
// exactly `groups` labels, so a fixture edit that changed the grouping is
// reported instead of quietly adjudicating a hint with different operands.
func parseRangeAggregationForCapHint(t *testing.T, query string, groups int) *syntax.RangeAggregationExpr {
	t.Helper()

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Grouping == nil || len(ra.Grouping.Groups) != groups {
		t.Fatalf("fixture must group on exactly %d labels; got %v", groups, ra.Grouping)
	}
	return ra
}

// findProjectionExpr answers the expression p projects under alias.
func findProjectionExpr(t *testing.T, p *chplan.Project, alias string) chplan.Expr {
	t.Helper()

	for _, pr := range p.Projections {
		if pr.Alias == alias {
			return pr.Expr
		}
	}
	t.Fatalf("no projection aliased %q in %d projections", alias, len(p.Projections))
	return nil
}
