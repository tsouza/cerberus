package logql

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file pins the mutants the SQL-side parser-stage extraction made
// reachable in [mergedLabelsColumn] and in the wire-path Sample reshape.
// Both functions gained branches that the existing plan-shape tests
// execute without asserting, which turns a NOT COVERED exclusion into a
// surviving mutant.

// pipelineLabelsColumn is the labels column the row stage defines for a
// pipeline's first parser stage, the shape [mergedLabelsColumn] sees
// whenever the pipeline carries one.
func pipelineLabelsColumn() *chplan.ColumnRef {
	return &chplan.ColumnRef{Name: pipelineLabelsAlias(0)}
}

// TestMergedLabelsColumn_NoUnwrapNoStructuredOuterBy pins that the
// merged map is read as a whole only when something downstream needs it.
//
// Kills the CONDITIONALS_BOUNDARY on the `> 0` in
//
//	outerByNeedsParsedLabels := len(structuredOuterByKeys(...)) > 0
//
// A flip to `>= 0` makes the flag unconditionally true, so the
// early-return never fires and every parser-stage query reads its
// identity off the merged map instead of the raw columns. The labels
// column here IS parser-merged, so the second guard cannot mask the
// first.
func TestMergedLabelsColumn_NoUnwrapNoStructuredOuterBy(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	if merged := mergedLabelsColumn(pipelineLabelsColumn(), s, lowerCtx{}, false); merged != nil {
		t.Errorf("merged column = %#v, want nil — without `| unwrap` and without a structured outer-by key "+
			"nothing reads the merged map as a whole", merged)
	}
}

// TestMergedLabelsColumn_UnwrapReadsTheMergedColumn pins the positive
// half: an unwrap over a parser-merged labels column reads that column.
//
// Kills the INVERT_LOGICAL on the first guard's `&&`: with hasUnwrap
// true and no outer-by key the original reads `!true && !false` → false
// and falls through, while the `||` mutant reads `!true || !false` →
// true and returns nil.
func TestMergedLabelsColumn_UnwrapReadsTheMergedColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	labels := pipelineLabelsColumn()
	if merged := mergedLabelsColumn(labels, s, lowerCtx{}, true); merged != labels {
		t.Errorf("merged column = %#v, want the pipeline labels column %s", merged, labels.Name)
	}
}

// TestMergedLabelsColumn_RawResourceAttributesIsNotMerged pins the
// second guard: with no parser stage the labels map is the raw
// ResourceAttributes column, which carries no parsed label, so an outer
// `by (...)` naming a non-top-level label still resolves off the raw
// column rather than a merged map.
//
// Kills the CONDITIONALS_NEGATION on `col.Name !=
// s.ResourceAttributesColumn` in [hasParserMergedLabels]: the `==`
// mutant reports the raw column as parser-merged and the guard returns
// it instead of nil.
func TestMergedLabelsColumn_RawResourceAttributesIsNotMerged(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	raw := &chplan.ColumnRef{Name: s.ResourceAttributesColumn}
	lc := lowerCtx{OuterByLabels: []string{"category"}}
	if len(structuredOuterByKeys(lc.OuterByLabels, s)) == 0 {
		t.Fatalf("fixture invalid: %v must carry a structured outer-by key so the first guard falls through", lc.OuterByLabels)
	}
	if merged := mergedLabelsColumn(raw, s, lc, false); merged != nil {
		t.Errorf("merged column = %#v, want nil — the raw ResourceAttributes column is not a parser-merged map", merged)
	}
}

// TestProjectSamples_UnpackReplacesTheLineWithTheUnpackedEntry pins that
// the wire-path Sample reshape projects `| unpack`'s rewritten line
// rather than the raw body column.
//
// Kills the CONDITIONALS_NEGATION on the `err == nil` guarding the
// [PipelineLineExpr] rewrite: negated, the rewrite is discarded on every
// success and callers get the packed JSON payload as the log line, under
// labels that describe its contents.
func TestProjectSamples_UnpackReplacesTheLineWithTheUnpackedEntry(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &Lang{Schema: s}

	plan, meta, err := l.Parse(context.Background(), `{app="api"} | unpack`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	projected, err := l.ProjectSamples(plan, meta)
	if err != nil {
		t.Fatalf("ProjectSamples: %v", err)
	}
	p, ok := projected.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", projected)
	}

	var line chplan.Expr
	for _, proj := range p.Projections {
		if proj.Alias == LogLineColumn {
			line = proj.Expr
		}
	}
	if line == nil {
		t.Fatalf("no projection aliased %q in %#v", LogLineColumn, p.Projections)
	}
	if ref, ok := line.(*chplan.ColumnRef); ok && ref.Name == s.BodyColumn {
		t.Fatalf("`| unpack` projected the raw %q column as the log line; the unpacked `_entry` member "+
			"is the line, and the body still holds the packed payload", s.BodyColumn)
	}
}
