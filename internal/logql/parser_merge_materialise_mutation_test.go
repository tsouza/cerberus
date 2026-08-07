package logql

import (
	"context"
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file pins the mutants the SQL-side parser-stage extraction made
// reachable in [materialiseParserMergedLabels] and in the wire-path
// Sample reshape. Both functions gained branches that the existing plan
// -shape tests execute without asserting, which turns a NOT COVERED
// exclusion into a surviving mutant.

// scanNode is a stand-in for whatever the pipeline lowered to. The
// materialise step only ever wraps its input, so its shape is irrelevant
// and identity comparison is the sharpest available assertion.
func scanNode() chplan.Node { return &chplan.Scan{Table: "otel_logs"} }

// parserMergedExpr is a labels expression [hasParserMergedLabels]
// classifies as parser-merged: anything that is not a bare ColumnRef on
// the resource-attributes column.
func parserMergedExpr(s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
	}
}

// projectedColumnNames returns the column each projection carries, in
// order, using the alias where one is set. It fails the test if the node
// is not a Project, so a caller asserting "no Project was inserted" uses
// the identity check below instead.
func projectedColumnNames(t *testing.T, n chplan.Node) []string {
	t.Helper()

	p, ok := n.(*chplan.Project)
	if !ok {
		t.Fatalf("materialiseParserMergedLabels returned %T, want *chplan.Project", n)
	}
	names := make([]string, 0, len(p.Projections))
	for _, proj := range p.Projections {
		if proj.Alias != "" {
			names = append(names, proj.Alias)
			continue
		}
		ref, ok := proj.Expr.(*chplan.ColumnRef)
		if !ok {
			t.Fatalf("unaliased projection is %T, want *chplan.ColumnRef", proj.Expr)
		}
		names = append(names, ref.Name)
	}
	return names
}

// TestMaterialiseParserMergedLabels_NoUnwrapNoStructuredOuterBy pins that
// the materialise Project is inserted only when something downstream
// actually needs the merged map as a column.
//
// Kills the CONDITIONALS_BOUNDARY on the `> 0` in
//
//	outerByNeedsParsedLabels := len(structuredOuterByKeys(...)) > 0
//
// A flip to `>= 0` makes the flag unconditionally true, so the
// early-return two lines below never fires and every parser-stage query
// grows an intermediate Project it does not need — invisible to any test
// that only reads the outermost node. The labels expression here IS
// parser-merged, so the second gate cannot mask the first.
func TestMaterialiseParserMergedLabels_NoUnwrapNoStructuredOuterBy(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	inner := scanNode()

	got, merged := materialiseParserMergedLabels(inner, parserMergedExpr(s), s, lowerCtx{}, false)
	if got != inner {
		t.Errorf("input was wrapped in %T; without `| unwrap` and without a structured outer-by key "+
			"nothing reads the merged map as a column, so the input must be forwarded unchanged", got)
	}
	if merged != nil {
		t.Errorf("merged column ref = %#v, want nil — no Project was inserted to define it", merged)
	}
}

// TestMaterialiseParserMergedLabels_OuterByTopLevelColumnsAreNotDuplicated
// pins that a top-level column an outer `by (...)` names is carried
// through exactly once. The severity column is both a recognised
// top-level label and already in the fixed projection list above, so it
// is the one column where "append what the outer by needs" and "do not
// repeat what is already projected" disagree.
//
// A duplicate column in a Project is a ClickHouse-level ambiguity, not a
// cosmetic wart, so the assertion is on the exact list rather than on
// membership.
func TestMaterialiseParserMergedLabels_OuterByTopLevelColumnsAreNotDuplicated(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	if s.SeverityColumn == "" || s.TraceIDColumn == "" {
		t.Fatalf("fixture invalid: schema must name both a severity and a trace-id column, got %q / %q",
			s.SeverityColumn, s.TraceIDColumn)
	}

	lc := lowerCtx{OuterByLabels: []string{s.SeverityColumn, s.TraceIDColumn, s.SeverityColumn}}
	got, merged := materialiseParserMergedLabels(scanNode(), parserMergedExpr(s), s, lc, true)

	want := []string{
		s.ResourceAttributesColumn,
		s.TimestampColumn,
		s.BodyColumn,
		s.SeverityColumn,
		"_logql_merged_labels",
		s.AttributesColumn,
		s.TraceIDColumn,
	}
	if diff := projectedColumnNames(t, got); !reflect.DeepEqual(diff, want) {
		t.Errorf("projected columns\n got: %v\nwant: %v", diff, want)
	}
	if merged == nil || merged.Name != "_logql_merged_labels" {
		t.Errorf("merged column ref = %#v, want a ref to _logql_merged_labels", merged)
	}
}

// TestMaterialiseParserMergedLabels_AliasedLabelsColumnIsNotAddressable
// pins that only an UNALIASED column reference counts as already
// projected. When the merged labels expression is itself a plain column
// it goes into the Project under the merged alias, which means its own
// name no longer resolves downstream — an outer `by (...)` naming that
// column still needs its own projection.
//
// Kills the INVERT_LOGICAL on the `&&` in the `projected` scan: under
// `||` an aliased column ref is recorded under its pre-alias name, the
// outer-by column is skipped as already present, and ClickHouse aborts
// resolving an identifier the Project renamed away.
func TestMaterialiseParserMergedLabels_AliasedLabelsColumnIsNotAddressable(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	labelsExpr := &chplan.ColumnRef{Name: s.TraceIDColumn}
	if !hasParserMergedLabels(labelsExpr, s) {
		t.Fatalf("fixture invalid: %q must classify as parser-merged for this path to run", s.TraceIDColumn)
	}

	lc := lowerCtx{OuterByLabels: []string{s.TraceIDColumn}}
	got, _ := materialiseParserMergedLabels(scanNode(), labelsExpr, s, lc, true)

	names := projectedColumnNames(t, got)
	if names[len(names)-1] != s.TraceIDColumn {
		t.Errorf("projected columns %v do not end in a bare %q projection; the outer by-clause reads that "+
			"column by name, and the aliased labels projection does not answer to it", names, s.TraceIDColumn)
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

	projected := l.ProjectSamples(plan, meta)
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
