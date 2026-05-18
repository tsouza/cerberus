package logql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestProjectSamples_MetricBranchRefsValueColumn pins the metric-branch
// wire-wrap against a post-#310 regression — the e2e
// `TestLokiQueryRangeCountOverTime` failure root cause.
//
// Before #310 the chsql RangeWindow emitter projected its windowed value
// under the lowercase alias `value`, and `Lang.ProjectSamples` ColumnRef'd
// `"value"` to match. #310 collapsed that lowercase-rename Project layer
// across PromQL / LogQL / chsql, leaving every RangeWindow emitter to
// expose its value column under the canonical PascalCase alias `Value`
// (kept in sync via `rangeAggSynthValueColumn` in
// `internal/logql/range_aggregation.go`).
//
// The LogQL adapter's `Lang.ProjectSamples` metric branch was the one
// site outside the instant-fn / aggregate paths that #310 missed; its
// ColumnRef stayed wired to the dead lowercase alias, so any LogQL
// metric query whose plan wraps a RangeWindow (rate / count_over_time /
// bytes_rate / bytes_over_time) emitted CH SQL that resolved
// `UNKNOWN_IDENTIFIER: 'value'. Maybe you meant: ['Value']` and returned
// a 500 on the wire.
//
// The test is parser/lowering-free on purpose — it constructs a synthetic
// `*chplan.RangeWindow` directly so the assertion pins the exact column
// the wire-wrap ColumnRefs without depending on the upstream LogQL parser
// or the lowering path that produces the RangeWindow.
func TestProjectSamples_MetricBranchRefsValueColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	// Synthetic plan shaped like LogQL's range-aggregation lowering output
	// (see `lowerRangeAggregation` in `range_aggregation.go`): a RangeWindow
	// whose ValueColumn is the canonical "Value" emitted at the outer
	// SELECT site since #310. ProjectSamples doesn't inspect the inner Scan,
	// so a `nil` Input field would be acceptable too — a synthetic Scan
	// just keeps the plan structurally valid for any future invariant
	// check that walks Children().
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: s.LogsTable},
		Func:            "count_over_time",
		Range:           time.Minute,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
	}

	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("ProjectSamples returned %d projections, want 4 (MetricName, Attributes, TimeUnix, Value); got %+v",
			len(proj.Projections), proj.Projections)
	}

	// The 4th projection (positional Sample.Value slot) must:
	//   - alias as "Value" (the chclient.Sample scanner reads positionally,
	//     but the alias is what surfaces in golden SQL — pin it explicitly)
	//   - reference the canonical column the inner RangeWindow / Aggregate
	//     emit (post-#310: literal "Value")
	valueSlot := proj.Projections[3]
	if valueSlot.Alias != "Value" {
		t.Errorf("value slot alias: got %q, want %q", valueSlot.Alias, "Value")
	}
	colRef, ok := valueSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("value slot expr: got %T, want *chplan.ColumnRef (a literal "+
			"or computation here would mean the wire-wrap is reshaping data "+
			"that should already be wired)", valueSlot.Expr)
	}
	// Post-#310: the inner emitter aliases its windowed value as
	// "Value" (PascalCase canonical). Pre-#310 callers ColumnRef'd
	// "value" — this assertion is the regression checkpoint.
	if colRef.Name != "Value" {
		t.Errorf("value slot ColumnRef.Name: got %q, want %q (post-#310 "+
			"canonical alias the inner RangeWindow / Aggregate emit at the "+
			"outer SELECT site; lowercase \"value\" is the dead pre-#310 alias)",
			colRef.Name, "Value")
	}

	// Sibling sanity: the Attributes slot must reference the schema's
	// ResourceAttributesColumn (not a hardcoded literal); pinning this
	// catches the symmetric "hardcoded constant drifted from schema"
	// failure mode in the same wire-wrap.
	attrsSlot := proj.Projections[1]
	attrsRef, ok := attrsSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("attributes slot expr: got %T, want *chplan.ColumnRef", attrsSlot.Expr)
	}
	if attrsRef.Name != s.ResourceAttributesColumn {
		t.Errorf("attributes slot ColumnRef.Name: got %q, want %q (schema's ResourceAttributesColumn)",
			attrsRef.Name, s.ResourceAttributesColumn)
	}
}

// TestProjectSamples_VectorAggregateRefsAttributes pins the metric-branch
// wire-wrap against the regression `sum(count_over_time({...}[5m]))`
// surfaced via the loki-compatibility harness as 502 'Unknown expression
// identifier ResourceAttributes' from ClickHouse.
//
// Root cause: a vector aggregation runs through `wrapVectorAggregateForSample`,
// which has already projected the row into the canonical (MetricName,
// Attributes, TimeUnix, Value) Sample contract before reaching the engine
// wire-wrap. At that point the raw `ResourceAttributes` column is gone (the
// Aggregate's GROUP BY consumed it) — the stream identity rides under the
// `Attributes` alias. The previous `Lang.ProjectSamples` blindly ColumnRef'd
// `s.ResourceAttributesColumn` regardless of inner shape, so the outer SELECT
// referenced a column that ClickHouse couldn't resolve.
//
// The fix mirrors the same `isVectorAggregateSampleShape` switch the binop
// lowering applies in `sampleShapeOverLogInner` (see internal/logql/binary.go).
// Construct a synthetic Project carrying the canonical-shape aliases and
// assert the wrap picks `Attributes` instead of `ResourceAttributes`.
func TestProjectSamples_VectorAggregateRefsAttributes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	// Synthetic plan shaped like `wrapVectorAggregateForSample`'s output:
	// a *chplan.Project whose Alias list includes "Attributes" — the
	// canonical-shape marker `isVectorAggregateSampleShape` keys on.
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: s.LogsTable},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.FuncCall{
				Name: "CAST",
				Args: []chplan.Expr{
					&chplan.FuncCall{Name: "map", Args: nil},
					&chplan.LitString{V: "Map(String,String)"},
				},
			}, Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}

	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}
	attrsSlot := proj.Projections[1]
	attrsRef, ok := attrsSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("attributes slot expr: got %T, want *chplan.ColumnRef", attrsSlot.Expr)
	}
	if attrsRef.Name != "Attributes" {
		t.Errorf("attributes slot ColumnRef.Name: got %q, want %q "+
			"(vector-aggregation already projected stream identity as the "+
			"`Attributes` alias via wrapVectorAggregateForSample — referencing "+
			"the raw `ResourceAttributes` column at the outer SELECT site "+
			"surfaces as 502 from ClickHouse since the Aggregate's GROUP BY "+
			"consumed it)", attrsRef.Name, "Attributes")
	}
}
