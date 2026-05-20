package logql_test

import (
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

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

// TestProjectSamples_MatrixRangeWindowRefsAnchorTs pins the metric-branch
// wire-wrap against the loki-compatibility 0/55 regression where every
// LogQL metric query (`count_over_time` / `rate` / `*_over_time`) over a
// /loki/api/v1/query_range request returned an empty matrix.
//
// Root cause: LogQL's range-aggregation lowering left RangeWindow.Start /
// End / Step / OuterRange zero, so the chsql emitter took the instant
// path and anchored the windowed-array filter at `now64(9)`. Any query
// whose seeded data lay outside the last 5 minutes of wall-clock (the
// compat harness seeds day-old data) had every sample filtered out by
// `arrayFilter(p -> tupleElement(p,1) > now64(9) - <range>, ...)`.
//
// The fix wires `lc.Step` + `lc.Start` / `lc.End` into the RangeWindow
// so the matrix path fires (one row per anchor across [Start, End]
// spaced by Step). The matrix RangeWindow exposes the per-row anchor
// under the literal column `anchor_ts`; ProjectSamples must forward it
// into the canonical TimeUnix slot — otherwise the outer synth
// `now64(9) - 5s` collapses every per-step row into one point and the
// matrix pivot drops everything but one sample per series.
//
// Mirrors the PromQL side's matrix-shape handling in
// `wrapWithSampleProjection` (internal/api/prom/handler.go).
func TestProjectSamples_MatrixRangeWindowRefsAnchorTs(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	// Synthetic matrix-shape RangeWindow: OuterRange + Step set the same
	// way logql.LowerAtRange does when handed a non-zero step. The
	// emitter's matrix path projects `anchor_ts` per row; ProjectSamples
	// must rename it to TimeUnix on the outer SELECT.
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: s.LogsTable},
		Func:            "count_over_time",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		OuterRange:      time.Hour,
		Start:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
	}

	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}
	tsSlot := proj.Projections[2]
	if tsSlot.Alias != "TimeUnix" {
		t.Errorf("ts slot alias: got %q, want %q", tsSlot.Alias, "TimeUnix")
	}
	tsRef, ok := tsSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("ts slot expr: got %T, want *chplan.ColumnRef (matrix path "+
			"must forward the inner `anchor_ts` column; a synth now64-based "+
			"expression would collapse every per-step row into one point)",
			tsSlot.Expr)
	}
	if tsRef.Name != "anchor_ts" {
		t.Errorf("ts slot ColumnRef.Name: got %q, want %q (matrix-shape "+
			"RangeWindow exposes the per-row anchor under the literal column "+
			"`anchor_ts`; synthing `now64(9) - 5s` here would drop every "+
			"per-step row outside the matrix pivot's 5-minute lookback)",
			tsRef.Name, "anchor_ts")
	}
}

// TestProjectSamples_VectorAggregateOverMatrixForwardsTimeUnix pins the
// "vector aggregation over a matrix-shape RangeWindow" path against the
// sibling regression: `sum(count_over_time({...}[5m]))` over query_range.
//
// Inner shape: `wrapVectorAggregateForSample` re-aliases the Aggregate's
// per-anchor bucket column to `TimeUnix` so the canonical Sample contract
// carries one row per step. ProjectSamples must forward that existing
// `TimeUnix` column rather than overwrite it with `now64(9) - 5s`; the
// overwrite would collapse every per-step row into one point and the
// matrix pivot would emit one sample per series instead of one per step.
func TestProjectSamples_VectorAggregateOverMatrixForwardsTimeUnix(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	// Synthetic vector-aggregate output (matches `wrapVectorAggregateForSample`
	// in range mode where bucket_ts → TimeUnix). The presence of an
	// "Attributes" alias is the canonical-shape marker; the TimeUnix slot
	// is a `bucket_ts` ColumnRef (re-aliased to TimeUnix by the inner
	// Project) carrying the per-anchor timestamp.
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
			{Expr: &chplan.ColumnRef{Name: "bucket_ts"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}

	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}
	tsSlot := proj.Projections[2]
	tsRef, ok := tsSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("ts slot expr: got %T, want *chplan.ColumnRef "+
			"(vector-aggregate path must forward the inner `TimeUnix` column; "+
			"a synth now64-based expression would collapse every per-step "+
			"row into one point)", tsSlot.Expr)
	}
	if tsRef.Name != "TimeUnix" {
		t.Errorf("ts slot ColumnRef.Name: got %q, want %q (vector-aggregate "+
			"already projected the per-anchor bucket alias as the canonical "+
			"`TimeUnix` slot via wrapVectorAggregateForSample — overwriting "+
			"with synth now64 would drop per-step rows)", tsRef.Name, "TimeUnix")
	}
}

// TestProjectSamples_VectorVectorBinopRefsAttributes pins the metric-branch
// wire-wrap against the Grafana Loki datasource health-check regression:
// `vector(1) + vector(1)` (Grafana's CheckHealth probe) lowered to a
// VectorJoin whose emitter projects `L.Attributes` / `L.TimeUnix` /
// `L.Value`. The previous Lang.ProjectSamples saw a top-level VectorJoin
// (not a *chplan.Project), fell through to the default branch, and
// ColumnRef'd the raw `ResourceAttributes` column at the outer wrap —
// ClickHouse returned `code: 47 Unknown expression identifier
// 'ResourceAttributes'` (and Grafana surfaced "Unable to connect with
// Loki" in red on every page load even though log queries worked).
//
// The fix extends `isVectorAggregateSampleShape` to recognise VectorJoin
// as canonical-Sample-shape since its emitter projects under the
// `Attributes` alias by construction. Pin that here so any future
// refactor of the helper can't silently drop the VectorJoin branch.
func TestProjectSamples_VectorVectorBinopRefsAttributes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	// Synthetic VectorJoin output — the exact node shape lowerVectorVector
	// produces for `vector(1) + vector(1)`. The legs themselves don't
	// matter for the ProjectSamples wrap; only the top-level type does.
	leftLeg := &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.LitString{V: ""}, Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.LitFloat{V: 1}, Alias: "Value"},
		},
	}
	plan := &chplan.VectorJoin{
		Left:             leftLeg,
		Right:            leftLeg,
		Op:               chplan.OpAdd,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
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
			"(VectorJoin's emitter projects `L.Attributes` / `L.TimeUnix` / "+
			"`L.Value` so the post-join scope exposes `Attributes`, not "+
			"`ResourceAttributes` — outer wrap must follow suit)",
			attrsRef.Name, "Attributes")
	}
	tsSlot := proj.Projections[2]
	tsRef, ok := tsSlot.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("ts slot expr: got %T, want *chplan.ColumnRef", tsSlot.Expr)
	}
	if tsRef.Name != "TimeUnix" {
		t.Errorf("ts slot ColumnRef.Name: got %q, want %q (VectorJoin "+
			"output exposes TimeUnix from L.TimeUnix; the default synth "+
			"`now64(9) - 5s` would land outside the join's per-row "+
			"timestamp scope)", tsRef.Name, "TimeUnix")
	}
}

// TestProjectSamples_LogQuerySurfacesDetectedLevelWhenReferenced pins
// the log-stream branch's Attributes slot to a `mapConcat(...)` that
// folds the synthesized `detected_level` label onto the row's
// ResourceAttributes when the query explicitly references the label.
//
// Reference Loki surfaces `detected_level` as a stream-identity label
// whenever severity is detectable in the underlying records (stream /
// structured-metadata labels, parser-stage extraction, or content
// scan). Cerberus mirrors that broadly — see
// [TestProjectSamples_BareLogQueryAlsoSurfacesDetectedLevel] for the
// bare-selector path and
// [TestProjectSamples_ParserStageQuerySurfacesDetectedLevel] for the
// parser-stage path. This test keeps the explicit-reference shape
// pinned so a regression that loses the wrap on the most common
// "user typed detected_level" path is caught synchronously.
func TestProjectSamples_LogQuerySurfacesDetectedLevelWhenReferenced(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	expr, err := syntax.ParseExpr(`{job="api"} | detected_level="error"`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	// Log-stream queries lower to a Scan (or Filter(Scan)) — no inner
	// Project layer. ProjectSamples wraps with the wire-shape projection.
	plan := &chplan.Scan{Table: s.LogsTable}
	wrapped := l.ProjectSamples(plan, engine.Meta{
		IsMetric: false,
		Extra:    map[string]any{"expr": expr},
	})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("got %d projections, want 4 (MetricName, Attributes, TimeUnix, Value)",
			len(proj.Projections))
	}

	attrsSlot := proj.Projections[1]
	if attrsSlot.Alias != "Attributes" {
		t.Fatalf("attributes slot alias: got %q, want %q", attrsSlot.Alias, "Attributes")
	}
	// Must be a mapConcat(...) — the helper that folds detected_level
	// onto the row's ResourceAttributes. A bare ColumnRef here would
	// mean the conditional wrap missed the explicit `|
	// detected_level=...` reference.
	fn, ok := attrsSlot.Expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall "+
			"(wire-wrap must wrap ResourceAttributes in a mapConcat that "+
			"adds the synthesized `detected_level` label when the query "+
			"explicitly references it)",
			attrsSlot.Expr)
	}
	if fn.Name != "mapConcat" {
		t.Errorf("attributes slot FuncCall.Name: got %q, want %q "+
			"(wire-wrap must fold detected_level via mapConcat when "+
			"referenced)", fn.Name, "mapConcat")
	}
}

// TestProjectSamples_BareLogQueryAlsoSurfacesDetectedLevel pins the
// bare-selector path: a query like `{service="api"}` with no
// parser stage, no `detected_level` / `level` reference, and no
// grouping clause MUST still wrap its Attributes projection in
// `mapConcat(...)` so the output stream identity carries the
// synthesized severity label.
//
// Reference Loki surfaces `detected_level` on every log query whose
// underlying records have detectable severity (stream /
// structured-metadata labels, parser-stage extraction, or content
// scan). The loki-compat `fast/basic-selectors.yaml` cases all return
// 3-4 Streams per query (one per detected_level value), so cerberus
// must split the same way. An earlier restrictive gate (PR #556) had
// the bare-selector path skip the wrap; that produced
// `streams length: expected=4 actual=1` regressions for every
// fast/basic-selectors case (~5 cases below baseline). This test
// pins the broad-wrap behavior so a re-narrowing of the trigger is
// caught synchronously rather than via the compat harness.
func TestProjectSamples_BareLogQueryAlsoSurfacesDetectedLevel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	expr, err := syntax.ParseExpr(`{service="api"}`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	plan := &chplan.Scan{Table: s.LogsTable}
	wrapped := l.ProjectSamples(plan, engine.Meta{
		IsMetric: false,
		Extra:    map[string]any{"expr": expr},
	})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples returned %T, want *chplan.Project", wrapped)
	}

	attrsSlot := proj.Projections[1]
	if attrsSlot.Alias != "Attributes" {
		t.Fatalf("attributes slot alias: got %q, want %q", attrsSlot.Alias, "Attributes")
	}
	// Must be a `mapConcat(...)` — the helper that folds the
	// synthesized `detected_level` label onto the row's
	// ResourceAttributes. A bare ColumnRef here would mean the
	// gate over-restricted again and the bare-selector path lost
	// stream-identity parity with reference Loki.
	fn, ok := attrsSlot.Expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall "+
			"(bare selector query must wrap Attributes in mapConcat — "+
			"reference Loki surfaces detected_level on every log query "+
			"with detectable severity, and cerberus mirrors that to keep "+
			"the loki-compat fast/basic-selectors `streams length` "+
			"comparison aligned)",
			attrsSlot.Expr)
	}
	if fn.Name != "mapConcat" {
		t.Errorf("attributes slot FuncCall.Name: got %q, want %q "+
			"(bare selector must fold detected_level via mapConcat)",
			fn.Name, "mapConcat")
	}
}

// TestProjectSamples_ParserStageQuerySurfacesDetectedLevel pins the
// parser-stage path: queries that use `| logfmt`, `| json`,
// `| regexp ...`, `| pattern ...`, or `| unpack` must also wrap their
// Attributes slot in `mapConcat(...)`.
//
// Reference Loki's detection pipeline reads `level` out of
// parser-extracted attributes when the line is structured (JSON /
// logfmt) and emits `detected_level` as part of stream identity. The
// loki-compat seeder writes both `level` and `detected_level` into
// `LogAttributes` for every row, and cerberus's SeverityText column
// is always populated, so a query like `{cluster="c1"} | logfmt`
// surfaces detected_level on every output stream. The wrap mirrors
// that. (Parser-extracted `level` continues to flow through the
// label-filter / grouping paths via the labels-map merge — see
// `internal/logql/lower.go::logfmtMergeLabels`.)
func TestProjectSamples_ParserStageQuerySurfacesDetectedLevel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	for _, q := range []string{
		// Bare logfmt parser — extracts all `key=value` pairs.
		`{cluster="c1"} | logfmt`,
		// Logfmt + non-level label filter.
		`{cluster="c1"} | logfmt | duration != ""`,
		// Bare JSON parser — extracts top-level keys.
		`{cluster="c1"} | json`,
		// Regexp parser with named captures.
		`{cluster="c1"} | regexp "(?P<method>\\w+) (?P<path>\\S+)"`,
		// Pattern parser — Go-side post-fetch stage.
		`{cluster="c1"} | pattern "<method> <path>"`,
		// Unpack parser — JSON-decode wrapper labels.
		`{cluster="c1"} | unpack`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan := &chplan.Scan{Table: s.LogsTable}
			wrapped := l.ProjectSamples(plan, engine.Meta{
				IsMetric: false,
				Extra:    map[string]any{"expr": expr},
			})
			proj := wrapped.(*chplan.Project)
			attrsSlot := proj.Projections[1]
			fn, ok := attrsSlot.Expr.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall (mapConcat) "+
					"for parser-stage query %q — parser stages should still surface "+
					"detected_level alongside their extracted keys", attrsSlot.Expr, q)
			}
			if fn.Name != "mapConcat" {
				t.Errorf("attributes slot FuncCall.Name: got %q, want %q for query %q",
					fn.Name, "mapConcat", q)
			}
		})
	}
}

// TestProjectSamples_LineFilterQuerySurfacesDetectedLevel pins the
// line-filter and label-filter paths: queries that combine a stream
// selector with a line filter (`|=`, `!=`, `|~`, `!~`) or a label
// filter on a non-level key (`| namespace="x"`) must also wrap their
// Attributes slot in `mapConcat(...)`.
//
// Reference Loki splits these queries into one Stream per
// detected_level value (or, for filters that select a single severity,
// emits a single Stream carrying the matched detected_level). The
// `fast/basic-selectors.yaml :: |~ "(?i)error"` case is the canonical
// regression — Loki returns 1 Stream with `detected_level: error`
// while cerberus previously returned 1 Stream without the label, so
// the `streams[0] labels differ` comparison failed.
func TestProjectSamples_LineFilterQuerySurfacesDetectedLevel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	for _, q := range []string{
		// Substring line filter.
		`{cluster="c1"} |= "level"`,
		// Case-insensitive regex line filter — the canonical compat
		// regression: Loki surfaces detected_level=error on lines
		// matching "(?i)error".
		`{cluster="c1"} |~ "(?i)error"`,
		// Negative regex line filter.
		`{cluster="c1"} !~ "(?i)debug"`,
		// Label filter on a non-level key.
		`{cluster="c1"} | namespace = "namespace-0"`,
		// Impossible-match line filter (cache test).
		`{cluster="c1"} |= "this will not hit any line"`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan := &chplan.Scan{Table: s.LogsTable}
			wrapped := l.ProjectSamples(plan, engine.Meta{
				IsMetric: false,
				Extra:    map[string]any{"expr": expr},
			})
			proj := wrapped.(*chplan.Project)
			attrsSlot := proj.Projections[1]
			fn, ok := attrsSlot.Expr.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall (mapConcat) "+
					"for line-filter / label-filter query %q — reference Loki "+
					"surfaces detected_level on these too", attrsSlot.Expr, q)
			}
			if fn.Name != "mapConcat" {
				t.Errorf("attributes slot FuncCall.Name: got %q, want %q for query %q",
					fn.Name, "mapConcat", q)
			}
		})
	}
}

// TestProjectSamples_ParserStageSurfacesExtractedLabels pins the
// parser-stage label surface — queries with `| logfmt`, `| json`,
// `| regexp ...`, or their typed-variants MUST project the merged
// (resource-label, extracted-key) map as `Attributes`, not just the
// raw ResourceAttributes column.
//
// Root cause this guards against: cerberus previously projected
// `mapConcat(ResourceAttributes, detected_level_map)` for every log
// query — losing the parser-extracted keys that the labels-merge
// (`| logfmt` → `mapConcat(ResourceAttributes, extractKeyValuePairs)`)
// only used for WHERE-side label filters. Downstream
// `toStreamsWithTransform` groups by the projected label set, so
// without the merged labels it collapsed reference Loki's hundreds
// of streams (one per unique extracted-key tuple) into single-digit
// counts in the loki-compat differential.
//
// The assertion drills into the nested mapConcat: the OUTER wrap is
// the `withDetectedLevel` mapConcat; its first argument MUST be the
// parser-merge mapConcat whose own first argument is the raw
// ResourceAttributes column. A flat `mapConcat(ResourceAttributes,
// detected_level_map)` would mean the parser-stage labels never
// landed on the row's Attributes — the regression this test pins.
func TestProjectSamples_ParserStageSurfacesExtractedLabels(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	for _, q := range []string{
		// Bare logfmt: extracts all `key=value` pairs.
		`{cluster="c1"} | logfmt`,
		// Bare JSON: extracts all top-level keys.
		`{cluster="c1"} | json`,
		// Typed logfmt: extracts named fields only.
		`{cluster="c1"} | logfmt level="lvl", app="application"`,
		// Typed JSON: extracts named JSON paths only.
		`{cluster="c1"} | json level="lvl", code="status.code"`,
		// Regexp parser with named captures.
		`{cluster="c1"} | regexp "(?P<method>\\w+) (?P<path>\\S+)"`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan := &chplan.Scan{Table: s.LogsTable}
			wrapped := l.ProjectSamples(plan, engine.Meta{
				IsMetric: false,
				Extra:    map[string]any{"expr": expr},
			})
			proj := wrapped.(*chplan.Project)
			attrsSlot := proj.Projections[1]

			// Outer wrap is withDetectedLevel's mapConcat. Drill into
			// arg[0] to find the parser-merge mapConcat. A leaf
			// ColumnRef there would be the regression: parser-stage
			// labels never landed on the row's projected Attributes.
			outer, ok := attrsSlot.Expr.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall", attrsSlot.Expr)
			}
			if outer.Name != "mapConcat" {
				t.Fatalf("outer FuncCall.Name: got %q, want %q", outer.Name, "mapConcat")
			}
			if len(outer.Args) != 2 {
				t.Fatalf("outer mapConcat args: got %d, want 2", len(outer.Args))
			}
			inner, ok := outer.Args[0].(*chplan.FuncCall)
			if !ok {
				t.Fatalf("outer arg[0]: got %T, want *chplan.FuncCall (parser-stage "+
					"merge mapConcat); a bare ColumnRef here means parser-extracted "+
					"keys are not landing on the row's Attributes — the precise "+
					"regression that collapsed reference Loki's hundreds of streams "+
					"per `| logfmt` / `| json` query into a handful on the "+
					"loki-compat differential", outer.Args[0])
			}
			if inner.Name != "mapConcat" {
				t.Errorf("inner parser-merge FuncCall.Name: got %q, want %q", inner.Name, "mapConcat")
			}
		})
	}
}

// TestProjectSamples_NoParserStage_KeepsBareResourceAttributes pins the
// negative path: when the query has no SQL-side parser stage, the
// projection MUST keep the bare ResourceAttributes column under the
// outer withDetectedLevel wrap. Regression coverage for the parser-stage
// branch over-firing on plain selectors / line filters / `| unpack` /
// `| pattern` (the latter two are Go-side post-fetch stages that the
// SQL projection should not try to surface).
func TestProjectSamples_NoParserStage_KeepsBareResourceAttributes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	for _, q := range []string{
		`{cluster="c1"}`,
		`{cluster="c1"} |= "error"`,
		`{cluster="c1"} | namespace="ns-0"`,
		`{cluster="c1"} | unpack`,
		`{cluster="c1"} | pattern "<method> <path>"`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan := &chplan.Scan{Table: s.LogsTable}
			wrapped := l.ProjectSamples(plan, engine.Meta{
				IsMetric: false,
				Extra:    map[string]any{"expr": expr},
			})
			proj := wrapped.(*chplan.Project)
			attrsSlot := proj.Projections[1]
			outer, ok := attrsSlot.Expr.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall "+
					"(detected_level wrap)", attrsSlot.Expr)
			}
			if outer.Name != "mapConcat" {
				t.Fatalf("outer FuncCall.Name: got %q, want %q", outer.Name, "mapConcat")
			}
			// arg[0] must be the bare ResourceAttributes ColumnRef — NOT
			// another mapConcat. A nested mapConcat would mean the
			// parser-stage surface is firing on queries that don't have
			// a SQL-side parser stage.
			ref, ok := outer.Args[0].(*chplan.ColumnRef)
			if !ok {
				t.Fatalf("outer arg[0]: got %T, want *chplan.ColumnRef "+
					"(the bare ResourceAttributes column when no SQL-side parser "+
					"stage is present); nesting another mapConcat here would mean "+
					"the parser-stage surface is over-firing on plain selectors "+
					"/ line filters / post-fetch parsers (`| unpack`, `| pattern`)",
					outer.Args[0])
			}
			if ref.Name != s.ResourceAttributesColumn {
				t.Errorf("outer arg[0] ColumnRef.Name: got %q, want %q",
					ref.Name, s.ResourceAttributesColumn)
			}
		})
	}
}

// TestProjectSamples_LogQueryWithDetectedLevelFilterTriggersWrap is a
// focused coverage point for the label-filter reference path — the
// `| detected_level="error"` form must trigger the wrap even though
// the matcher itself sits in a pipe stage rather than the stream
// selector. Same expectation as the explicit-matcher variant.
func TestProjectSamples_LogQueryWithDetectedLevelFilterTriggersWrap(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	for _, q := range []string{
		// label-filter form (pipe stage)
		`{job="api"} | detected_level="error"`,
		// short-alias label filter
		`{job="api"} | level=~"error|warn"`,
		// stream-selector form
		`{detected_level="error"}`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan := &chplan.Scan{Table: s.LogsTable}
			wrapped := l.ProjectSamples(plan, engine.Meta{
				IsMetric: false,
				Extra:    map[string]any{"expr": expr},
			})
			proj := wrapped.(*chplan.Project)
			attrsSlot := proj.Projections[1]
			fn, ok := attrsSlot.Expr.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("attributes slot expr: got %T, want *chplan.FuncCall (mapConcat) for query %q",
					attrsSlot.Expr, q)
			}
			if fn.Name != "mapConcat" {
				t.Errorf("attributes slot FuncCall.Name: got %q, want %q for query %q",
					fn.Name, "mapConcat", q)
			}
		})
	}
}
