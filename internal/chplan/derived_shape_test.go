package chplan_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// testSampleColumns is the canonical four-column naming used throughout
// this file — the OTel-CH defaults the PromQL head runs with.
var testSampleColumns = chplan.SampleColumns{
	MetricName: "MetricName",
	Attributes: "Attributes",
	Timestamp:  "TimeUnix",
	Value:      "Value",
}

// derivedShapeVerdicts is the expected chplan.IsDerivedShape answer for a
// plan ROOTED at each Node kind, using the populated instances in
// allNodeKinds (whose Filter / Project instances sit over a plain Scan).
//
// `true` means the node's SELECT exposes only (group keys…, timestamp,
// value): MetricName does not exist in that scope, so a consumer must
// synthesise it as the empty string. Referencing it instead is a
// ClickHouse code-47 `Unknown expression identifier 'MetricName'` — a 502
// on a live query_range, which is how the two-copy version of this
// classifier announced its drift.
//
// TestIsDerivedShape_CoversEveryNodeKind derives the key set from the
// package's planNode() implementers, so a new Node kind cannot be added
// without deciding, here, which shape it emits.
var derivedShapeVerdicts = map[string]bool{
	// Reducing and metrics-pipeline nodes: derived.
	"Aggregate":                true,
	"MetricsAggregate":         true,
	"MetricsHistogramOverTime": true,
	"RangeWindow":              true,
	"RangeWindowNative":        true,

	// Everything else keeps the canonical columns in scope (or is
	// re-canonicalised by its own emit, as a nested VectorSetOp is).
	"AbsentOverTime":          false,
	"CrossJoin":               false,
	"Filter":                  false,
	"HistogramQuantile":       false,
	"HistogramQuantileNative": false,
	"InfoJoin":                false,
	"Limit":                   false,
	"MetricsCompare":          false,
	"MetricsSecondStage":      false,
	"NaryVectorSetOp":         false,
	"NestedSetAnnotate":       false,
	"OneRow":                  false,
	"OrderBy":                 false,
	"Project":                 false,
	"RangeBucketFanout":       false,
	"RangeLWR":                false,
	"RangeWindowResample":     false,
	"Scan":                    false,
	"SearchTraceLimit":        false,
	"SetOperation":            false,
	"StepGrid":                false,
	"StructuralJoin":          false,
	"TopK":                    false,
	"UnionAll":                false,
	"VectorJoin":              false,
	"VectorSetOp":             false,
}

// TestIsDerivedShape_CoversEveryNodeKind is the class-level ratchet behind
// the one classifier. chplan.IsDerivedShape is consumed by BOTH the HTTP
// layer's canonical-sample projection and the chsql emitter's per-arm
// VectorSetOp canonicalisation; when those were two hand-written switches
// they silently drifted apart (the emitter grew MetricsAggregate and
// MetricsHistogramOverTime arms, the HTTP copy never did) while a
// docstring on each asserted they could not.
//
// One function cannot drift from itself, and this test closes the
// remaining hole: a Node kind added without a shape verdict fails here,
// naming the kind, rather than defaulting to "canonical" in silence.
func TestIsDerivedShape_CoversEveryNodeKind(t *testing.T) {
	t.Parallel()

	assertCoversEveryNodeKind(t, derivedShapeVerdicts, "derivedShapeVerdicts",
		"decide whether its SELECT exposes MetricName and record the verdict")
}

// TestIsDerivedShape_ClassifiesEveryNodeKind runs the classifier over one
// populated instance of every Node kind and compares against the recorded
// verdict, naming the kind on a mismatch. Deleting an arm from the switch
// therefore reds this test with the exact type that stopped being
// classified as derived.
func TestIsDerivedShape_ClassifiesEveryNodeKind(t *testing.T) {
	t.Parallel()

	for _, node := range allNodeKinds() {
		kind := reflect.TypeOf(node).Elem().Name()
		want, recorded := derivedShapeVerdicts[kind]
		if !recorded {
			// Reported by TestIsDerivedShape_CoversEveryNodeKind; skipping the
			// comparison here keeps this failure from duplicating that one.
			continue
		}
		if got := chplan.IsDerivedShape(node, testSampleColumns); got != want {
			t.Errorf("IsDerivedShape(*%s) = %v, want %v — a plan rooted at %s %s %s in scope, "+
				"and both the HTTP sample projection and the chsql VectorSetOp arm emit "+
				"read this one answer",
				kind, got, want, kind,
				map[bool]string{true: "does NOT have", false: "has"}[want],
				testSampleColumns.MetricName)
		}
	}
}

// TestIsDerivedShape_Transparency pins the two recursive rules, which are
// where the classifier's answer stops being a property of the root alone:
// a Filter never changes the column set, and a Project keeps a derived
// inner derived unless it republishes ALL FOUR canonical columns.
func TestIsDerivedShape_Transparency(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	window := &chplan.RangeWindow{
		Input: scan, Func: "rate", Range: 5 * time.Minute, Step: time.Minute,
		Start: time.Unix(1000, 0).UTC(), End: time.Unix(4600, 0).UTC(),
	}
	col := func(name string) chplan.Projection {
		return chplan.Projection{Expr: &chplan.ColumnRef{Name: name}}
	}
	canonical := []chplan.Projection{
		col(testSampleColumns.MetricName),
		col(testSampleColumns.Attributes),
		col(testSampleColumns.Timestamp),
		col(testSampleColumns.Value),
	}
	// The `projectValueOverInner` shape: replaces Value, widens nothing.
	valueRewrite := []chplan.Projection{
		col(testSampleColumns.Attributes),
		{Expr: &chplan.FuncCall{Name: "abs", Args: []chplan.Expr{&chplan.ColumnRef{Name: testSampleColumns.Value}}}, Alias: testSampleColumns.Value},
	}

	for _, tc := range []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"Filter over a derived inner stays derived", &chplan.Filter{Input: window}, true},
		{"Filter over a canonical inner stays canonical", &chplan.Filter{Input: scan}, false},
		{
			"value-rewrite Project over a derived inner stays derived",
			&chplan.Project{Input: window, Projections: valueRewrite},
			true,
		},
		{
			"canonical Project over a derived inner is canonical",
			&chplan.Project{Input: window, Projections: canonical},
			false,
		},
		{
			"Project missing one canonical column stays derived",
			&chplan.Project{Input: window, Projections: canonical[:3]},
			true,
		},
		{
			"Filter above a value-rewrite Project above a derived inner stays derived",
			&chplan.Filter{Input: &chplan.Project{Input: window, Projections: valueRewrite}},
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chplan.IsDerivedShape(tc.plan, testSampleColumns); got != tc.want {
				t.Errorf("IsDerivedShape = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProjectExposesCanonical_RespectsConfiguredNames pins that the
// classifier reads the column names it is HANDED rather than the OTel-CH
// defaults: the schema override config can rename all four, and a
// classifier that hard-coded them would call every plan derived on a
// renamed deployment.
func TestProjectExposesCanonical_RespectsConfiguredNames(t *testing.T) {
	t.Parallel()

	renamed := chplan.SampleColumns{
		MetricName: "metric", Attributes: "labels", Timestamp: "ts", Value: "v",
	}
	p := &chplan.Project{
		Input: &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "metric"}},
			{Expr: &chplan.ColumnRef{Name: "labels"}},
			{Expr: &chplan.ColumnRef{Name: "ts"}},
			{Expr: &chplan.ColumnRef{Name: "v"}},
		},
	}
	if !chplan.ProjectExposesCanonical(p, renamed) {
		t.Error("ProjectExposesCanonical = false for a Project naming all four configured columns")
	}
	if chplan.ProjectExposesCanonical(p, testSampleColumns) {
		t.Error("ProjectExposesCanonical = true against a naming the Project does not use")
	}
}

// TestProjectionOutputName pins the alias-then-bare-ColumnRef rule: a
// computed expression with no alias names no output column, so it must
// never be mistaken for a MetricName projection.
func TestProjectionOutputName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		proj chplan.Projection
		want string
	}{
		{"alias wins", chplan.Projection{Expr: &chplan.LitString{V: "x"}, Alias: "MetricName"}, "MetricName"},
		{
			"alias wins over column",
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "MetricName"},
			"MetricName",
		},
		{"bare column passes through", chplan.Projection{Expr: &chplan.ColumnRef{Name: "MetricName"}}, "MetricName"},
		{"computed and unaliased names nothing", chplan.Projection{Expr: &chplan.LitString{V: "x"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chplan.ProjectionOutputName(tc.proj); got != tc.want {
				t.Fatalf("ProjectionOutputName: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProjectionOutputsColumn pins the alias-wins rule that decides whether
// a Project still carries the canonical timestamp column. An alias RENAMES
// the output, so an aliased projection outputs its alias and nothing else —
// the expression underneath is no longer visible to anything above. Getting
// this wrong flips a vector join between the canonical (per-row TimeUnix)
// and derived (one row per series) shapes, which changes the join key and
// therefore the answer.
func TestProjectionOutputsColumn(t *testing.T) {
	t.Parallel()

	const col = "TimeUnix"
	for _, tc := range []struct {
		name string
		proj chplan.Projection
		want bool
	}{
		{
			name: "alias names the column",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: col},
			want: true,
		},
		{
			name: "alias renames the column away",
			// The expression IS the column, but the alias republishes it under
			// another name, so col is no longer an output of this projection.
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: "anchor_ts"},
		},
		{
			name: "bare column ref",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: col}},
			want: true,
		},
		{
			name: "bare column ref to another column",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
		{
			name: "unaliased non-column expression",
			proj: chplan.Projection{Expr: &chplan.FuncCall{Name: "now64"}},
		},
	} {
		if got := chplan.ProjectionOutputsColumn(tc.proj, col); got != tc.want {
			t.Errorf("%s: ProjectionOutputsColumn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProjectionOutputsColumn_EmptyColumn pins that nothing "outputs" the
// empty column name. A computed projection with no alias names no output,
// and a caller that lost track of its column name must not be told that
// projection matches.
func TestProjectionOutputsColumn_EmptyColumn(t *testing.T) {
	t.Parallel()

	if chplan.ProjectionOutputsColumn(chplan.Projection{Expr: &chplan.FuncCall{Name: "now64"}}, "") {
		t.Error("ProjectionOutputsColumn(computed projection, \"\") = true, want false")
	}
}
