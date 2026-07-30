package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The deferred-label-shaping hoist moves the OTel -> Prometheus attribute
// reshape ABOVE a native grid aggregate, which is only value-preserving when
// the Project it peels is exactly a label-shaping one over the row shape the
// native emitter reads. Every condition [hoistableShapingProject] checks guards
// a WRONG ANSWER, not a missed optimization, so the rejections below matter as
// much as the acceptance: a classifier that says yes too often ships a query
// that groups on the wrong series identity or selects a column that is not
// there.

// hoistTestScan is the raw relation the shaping Project sits over — the
// Filter-over-Scan shape a matchers-filtered selector lowers to.
func hoistTestScan() chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Database: "otel", Table: "otel_metrics_sum"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
}

// shapedAttributes is the shaping tower the lowering actually emits for the
// Attributes map — a non-identity expression over the raw column.
func shapedAttributes(s schema.Metrics) chplan.Expr {
	return canonicalAttributesExpr(&chplan.ColumnRef{Name: s.AttributesColumn})
}

// canonicalShapingProject builds the four-alias selector Project
// [augmentSelectorAttributes] emits: MetricName / Attributes / Timestamp /
// Value, with only Attributes shaped.
func canonicalShapingProject(s schema.Metrics, in chplan.Node) *chplan.Project {
	return &chplan.Project{
		Input: in,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: shapedAttributes(s), Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

func columnNames(t *testing.T, exprs []chplan.Expr) []string {
	t.Helper()
	out := make([]string, 0, len(exprs))
	for _, e := range exprs {
		ref, ok := e.(*chplan.ColumnRef)
		if !ok {
			t.Fatalf("group key %#v is not a *chplan.ColumnRef", e)
		}
		out = append(out, ref.Name)
	}
	return out
}

// TestHoistableShapingProject_DefersShapedAttributes pins the production shape:
// a single shaped Attributes group key becomes one Recollapse projection aliased
// to the OUTPUT column name, the aggregate re-groups on the RAW column, and the
// relation handed back is the one below the Project.
//
// The alias is the load-bearing assertion. The emitter projects the shaped key
// under its own synthetic intermediate and renames it to this alias only at the
// outer level; carrying the output name any lower would let ClickHouse re-bind
// the group key to the shaped column, which returns the right answer at the old
// cost and fails no test.
func TestHoistableShapingProject_DefersShapedAttributes(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	raw := hoistTestScan()
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}

	in, recollapse, rawGroupBy, ok := hoistableShapingProject(
		canonicalShapingProject(s, raw), groupBy, s.TimestampColumn, s.ValueColumn,
	)
	if !ok {
		t.Fatal("hoistableShapingProject refused the canonical selector Project")
	}
	if !in.Equal(raw) {
		t.Errorf("peeled input = %#v; want the relation below the Project", in)
	}
	if len(recollapse) != 1 {
		t.Fatalf("recollapse = %d projections; want 1", len(recollapse))
	}
	if recollapse[0].Alias != s.AttributesColumn {
		t.Errorf("recollapse alias = %q; want the output column name %q", recollapse[0].Alias, s.AttributesColumn)
	}
	if !recollapse[0].Expr.Equal(shapedAttributes(s)) {
		t.Errorf("recollapse expr = %#v; want the peeled shaping tower", recollapse[0].Expr)
	}
	if got := columnNames(t, rawGroupBy); len(got) != 1 || got[0] != s.AttributesColumn {
		t.Errorf("rawGroupBy = %v; want the raw [%s] the shaping reads", got, s.AttributesColumn)
	}
}

// TestHoistableShapingProject_CarriesPassThroughKeys pins the mixed case: a
// group key the Project passes through untouched stays a series identity key on
// the aggregate, and the raw columns the shaping reads are appended to it.
func TestHoistableShapingProject_CarriesPassThroughKeys(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	groupBy := []chplan.Expr{
		&chplan.ColumnRef{Name: s.MetricNameColumn},
		&chplan.ColumnRef{Name: s.AttributesColumn},
	}

	_, recollapse, rawGroupBy, ok := hoistableShapingProject(
		canonicalShapingProject(s, hoistTestScan()), groupBy, s.TimestampColumn, s.ValueColumn,
	)
	if !ok {
		t.Fatal("hoistableShapingProject refused a pass-through + shaped key pair")
	}
	if len(recollapse) != 1 || recollapse[0].Alias != s.AttributesColumn {
		t.Fatalf("recollapse = %+v; want only the shaped Attributes key", recollapse)
	}
	want := []string{s.MetricNameColumn, s.AttributesColumn}
	got := columnNames(t, rawGroupBy)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("rawGroupBy = %v; want %v (pass-through first, then the shaping inputs)", got, want)
	}
}

func TestHoistableShapingProject_Rejections(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	raw := hoistTestScan()
	attributesKey := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}

	// histogramCompanion is the trap this classifier exists to avoid. It emits
	// the SAME four aliases over the SAME shaping tower and differs only in
	// deriving Value from the bucket Count — hoisting it would strip both the
	// cast and the rename, so the aggregate would read a Value column the raw
	// relation does not have.
	histogramCompanion := canonicalShapingProject(s, raw)
	histogramCompanion.Projections[3] = chplan.Projection{
		Expr:  &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Count"}}},
		Alias: s.ValueColumn,
	}

	withReplacements := canonicalShapingProject(s, raw)
	withReplacements.Replacements = []chplan.Projection{
		{Expr: shapedAttributes(s), Alias: s.AttributesColumn},
	}

	emptyAlias := canonicalShapingProject(s, raw)
	emptyAlias.Projections[0].Alias = ""

	duplicateAlias := canonicalShapingProject(s, raw)
	duplicateAlias.Projections[0].Alias = s.AttributesColumn

	// nameOverlay reads MetricName while shaping Attributes (the `__name__`
	// overlay shape), so MetricName becomes a shaping INPUT. The emitter never
	// surfaces a shaping input above the merge level, so keeping MetricName as
	// an output identity key at the same time would silently drop it.
	nameOverlay := canonicalShapingProject(s, raw)
	nameOverlay.Projections[1].Expr = &chplan.FuncCall{
		Name: chplan.CanonicalMapFunc,
		Args: []chplan.Expr{&chplan.FuncCall{
			Name: "mapUpdate",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.AttributesColumn},
				&chplan.ColumnRef{Name: s.MetricNameColumn},
			},
		}},
	}

	constantShaping := canonicalShapingProject(s, raw)
	constantShaping.Projections[1].Expr = &chplan.FuncCall{Name: "map", Args: nil}

	allIdentity := canonicalShapingProject(s, raw)
	allIdentity.Projections[1].Expr = &chplan.ColumnRef{Name: s.AttributesColumn}

	cases := []struct {
		name    string
		in      chplan.Node
		groupBy []chplan.Expr
		// timestampCol overrides the schema timestamp column for the one case
		// that needs a column the Project never names; empty means the schema's.
		timestampCol string
	}{
		{
			name:    "not a Project",
			in:      raw,
			groupBy: attributesKey,
		},
		{
			name:    "no projections",
			in:      &chplan.Project{Input: raw},
			groupBy: attributesKey,
		},
		{
			name:    "carries Replacements",
			in:      withReplacements,
			groupBy: attributesKey,
		},
		{
			name:    "empty projection alias",
			in:      emptyAlias,
			groupBy: attributesKey,
		},
		{
			name:    "duplicate projection alias",
			in:      duplicateAlias,
			groupBy: attributesKey,
		},
		{
			name:    "histogram companion derives Value",
			in:      histogramCompanion,
			groupBy: attributesKey,
		},
		{
			// A timestamp column the Project never names cannot survive the
			// peel: the aggregate would read an identifier that is not there.
			name:         "timestamp is not projected",
			in:           canonicalShapingProject(s, raw),
			groupBy:      attributesKey,
			timestampCol: "UnprojectedTimeUnix",
		},
		{
			name:    "computed group key",
			in:      canonicalShapingProject(s, raw),
			groupBy: []chplan.Expr{shapedAttributes(s)},
		},
		{
			name:    "group key the Project does not define",
			in:      canonicalShapingProject(s, raw),
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}},
		},
		{
			name:    "duplicate group key",
			in:      canonicalShapingProject(s, raw),
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}, &chplan.ColumnRef{Name: s.AttributesColumn}},
		},
		{
			name:    "nothing is shaped",
			in:      allIdentity,
			groupBy: attributesKey,
		},
		{
			name:    "shaping reads no column",
			in:      constantShaping,
			groupBy: attributesKey,
		},
		{
			name:    "pass-through key is also a shaping input",
			in:      nameOverlay,
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.MetricNameColumn}, &chplan.ColumnRef{Name: s.AttributesColumn}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timestampCol := tc.timestampCol
			if timestampCol == "" {
				timestampCol = s.TimestampColumn
			}
			if _, _, _, ok := hoistableShapingProject(tc.in, tc.groupBy, timestampCol, s.ValueColumn); ok {
				t.Error("hoistableShapingProject accepted a shape the deferred emit cannot render correctly")
			}
		})
	}
}

// TestHoistShaping_RejectsPeeledInputTheEmitterCannotRead pins the coupling
// guard: the classifier is deliberately laxer about the Project than
// [isNativeRateInput] is, so hoistShaping re-checks the relation LEFT BEHIND.
// A Project over a Limit peels cleanly and still leaves a relation the native
// emitter cannot read, which would be a production 502 rather than a slow query.
func TestHoistShaping_RejectsPeeledInputTheEmitterCannotRead(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	const scanRowCap = 1000
	rw := &chplan.RangeWindow{
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		Input:           canonicalShapingProject(s, &chplan.Limit{Input: hoistTestScan(), Count: scanRowCap}),
	}
	if _, _, _, ok := hoistShaping(rw, s); ok {
		t.Error("hoistShaping accepted a peel that leaves a relation the native emitter cannot read")
	}

	rw.Input = canonicalShapingProject(s, hoistTestScan())
	if _, _, _, ok := hoistShaping(rw, s); !ok {
		t.Error("hoistShaping refused a peel that leaves the Filter/Scan the native emitter reads")
	}
}
