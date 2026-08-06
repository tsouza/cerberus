package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// nameBearingProject is the leaf shape every subquery spine bottoms out
// in: the selector Project that exposes a real per-series MetricName
// column. It is what makes the spine carryable at all.
func nameBearingProject(s schema.Metrics) *chplan.Project {
	return &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		},
	}
}

// droppedNameProject is what projectValueOverInner leaves behind for a
// derived sample: the empty literal, which internal/api/format's
// WithMetricName renders as "no __name__ at all". It is upstream's
// inputDropName=true, materialised as a column value.
func droppedNameProject(s schema.Metrics) *chplan.Project {
	return &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		},
	}
}

// attrOnlySubqueryWindow is the RangeWindow shape the subquery lowering
// builds at every level: grouped on the Attributes map alone. The
// windowed-array emitter projects exactly the grouping keys, so this is
// precisely why a per-series name could not reach an outer reducer.
func attrOnlySubqueryWindow(fn string, input chplan.Node, s schema.Metrics) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func:    fn,
		Input:   input,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
}

// assertNameGroupKey asserts rw groups on Attributes AND MetricName, in
// that order — the original key must survive alongside the new one, or
// series that differ only by label collapse.
func assertNameGroupKey(t *testing.T, rw *chplan.RangeWindow, s schema.Metrics) {
	t.Helper()
	if len(rw.GroupBy) != 2 {
		t.Fatalf("grouping key: got %d keys, want 2 (%s + %s)",
			len(rw.GroupBy), s.AttributesColumn, s.MetricNameColumn)
	}
	if !isIdentityColumnRef(rw.GroupBy[0], s.AttributesColumn) {
		t.Fatalf("original key clobbered: got %#v, want ColumnRef(%q)", rw.GroupBy[0], s.AttributesColumn)
	}
	if !isIdentityColumnRef(rw.GroupBy[1], s.MetricNameColumn) {
		t.Fatalf("appended key: got %#v, want ColumnRef(%q)", rw.GroupBy[1], s.MetricNameColumn)
	}
}

// assertAttrOnlyGroupKey asserts rw was left exactly as the subquery
// lowering built it — the negative half of every rejection case. A
// widened key on a rejected spine would be a half-applied mutation.
func assertAttrOnlyGroupKey(t *testing.T, rw *chplan.RangeWindow, s schema.Metrics) {
	t.Helper()
	if len(rw.GroupBy) != 1 {
		t.Fatalf("grouping key widened on a rejected spine: got %d keys, want 1", len(rw.GroupBy))
	}
	if !isIdentityColumnRef(rw.GroupBy[0], s.AttributesColumn) {
		t.Fatalf("grouping key: got %#v, want ColumnRef(%q)", rw.GroupBy[0], s.AttributesColumn)
	}
}

// TestSubqueryPreservedNameExpr_IdentityInnerCarriesColumn is the
// `last_over_time({__name__=~"a|b"}[10m:1m])` shape from #1778: the
// subquery's Identity window (Func == "") is a pure resampler, so the
// name below it reaches the outer reducer once the key is widened.
func TestSubqueryPreservedNameExpr_IdentityInnerCarriesColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", nameBearingProject(s), s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_PreservingInnerCarriesColumn is #1794's
// nested-Call shape: `last_over_time(last_over_time(x[5m])[10m:1m])`.
// Both windows preserve, so BOTH must gain the key — the outer reducer
// can only read a name the inner window still projects.
func TestSubqueryPreservedNameExpr_PreservingInnerCarriesColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	leafWindow := attrOnlySubqueryWindow("last_over_time", nameBearingProject(s), s)
	identity := attrOnlySubqueryWindow("", leafWindow, s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
	assertNameGroupKey(t, leafWindow, s)
}

// TestSubqueryPreservedNameExpr_DroppingInnerRejected pins upstream's
// `inputDropName`: `last_over_time(rate(x[5m])[10m:1m])` reports no
// `__name__`, because rate already deleted it. A preserving outer fn
// must not resurrect it.
func TestSubqueryPreservedNameExpr_DroppingInnerRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	leafWindow := attrOnlySubqueryWindow("rate", nameBearingProject(s), s)
	identity := attrOnlySubqueryWindow("", leafWindow, s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over a rate inner: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
	assertAttrOnlyGroupKey(t, leafWindow, s)
}

// TestSubqueryPreservedNameExpr_DroppingOuterRejected is the top-level
// gate: `max_over_time(x[10m:1m])` drops the name whatever the spine
// carries, so nothing is widened and the plan is left narrower.
func TestSubqueryPreservedNameExpr_DroppingOuterRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", nameBearingProject(s), s)

	if got := subqueryPreservedNameExpr(identity, "max_over_time", s); got != nil {
		t.Fatalf("name expr for a dropping outer fn: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_EmptyLiteralNameRejected: a spine whose
// MetricName is the empty literal has ALREADY dropped the name
// (projectValueOverInner's derived-sample marker). Carrying it would
// widen the GROUP BY by a constant and change nothing observable.
func TestSubqueryPreservedNameExpr_EmptyLiteralNameRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", droppedNameProject(s), s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over an already-dropped name: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_NonEmptyLiteralNameAccepted: the
// histogram companion arm projects a genuine constant name
// (`concat(MetricName, '_sum')` folded, or a pinned `'<base>_count'`).
// That IS a name, so the spine carries it.
func TestSubqueryPreservedNameExpr_NonEmptyLiteralNameAccepted(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	companion := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_histogram"},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: "http_requests_count"}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		},
	}
	identity := attrOnlySubqueryWindow("", companion, s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_FilterIsTransparent: the modifier
// time-bound Filter reshapes no columns, so it must not terminate the
// walk in either direction.
func TestSubqueryPreservedNameExpr_FilterIsTransparent(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	leafWindow := attrOnlySubqueryWindow("last_over_time", nameBearingProject(s), s)
	filtered := &chplan.Filter{Input: leafWindow, Predicate: &chplan.LitString{V: "1"}}
	identity := attrOnlySubqueryWindow("", filtered, s)

	got := subqueryPreservedNameExpr(identity, "first_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
	assertNameGroupKey(t, leafWindow, s)
}

// TestSubqueryPreservedNameExpr_UnionAllAllArmsCarry: a regex
// `__name__` matcher fans the scan out across every metrics table, so
// the spine's leaf is a UnionAll rather than a single Project. Every arm
// carries a name, so the union does.
func TestSubqueryPreservedNameExpr_UnionAllAllArmsCarry(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	union := &chplan.UnionAll{Inputs: []chplan.Node{
		nameBearingProject(s),
		&chplan.Filter{Input: nameBearingProject(s), Predicate: &chplan.LitString{V: "1"}},
	}}
	identity := attrOnlySubqueryWindow("", union, s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_UnionAllArmWindowsWidened pins the arm
// that a split predicate/mutator pair would silently miss: a RangeWindow
// living INSIDE a union arm (directly, and behind a Filter). The
// acceptance walk reaches those windows, so the widening must too —
// internal/chsql/range_window.go projects exactly the grouping keys, so
// an arm left on `GROUP BY Attributes` alone contributes a relation with
// no MetricName while the caller's wrap emits `SELECT MetricName` over
// the union: ClickHouse answers `Unknown identifier`, a 502, not a wrong
// number. Accepting an arm and not widening it must be unrepresentable.
func TestSubqueryPreservedNameExpr_UnionAllArmWindowsWidened(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	bareArmWindow := attrOnlySubqueryWindow("last_over_time", nameBearingProject(s), s)
	filteredArmWindow := attrOnlySubqueryWindow("first_over_time", nameBearingProject(s), s)
	union := &chplan.UnionAll{Inputs: []chplan.Node{
		bareArmWindow,
		&chplan.Filter{Input: filteredArmWindow, Predicate: &chplan.LitString{V: "1"}},
	}}
	identity := attrOnlySubqueryWindow("", union, s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
	assertNameGroupKey(t, bareArmWindow, s)
	assertNameGroupKey(t, filteredArmWindow, s)
}

// TestSubqueryPreservedNameExpr_UnionAllArmWindowDropsRejected is the
// negative half of the arm walk: one arm reduces with a name-dropping
// function, so the union cannot deliver a name and NOTHING may be
// widened — not the sibling arm's window, not the outer identity. A
// half-applied mutation is the failure this pairs against.
func TestSubqueryPreservedNameExpr_UnionAllArmWindowDropsRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	carryingArmWindow := attrOnlySubqueryWindow("last_over_time", nameBearingProject(s), s)
	droppingArmWindow := attrOnlySubqueryWindow("rate", nameBearingProject(s), s)
	union := &chplan.UnionAll{Inputs: []chplan.Node{carryingArmWindow, droppingArmWindow}}
	identity := attrOnlySubqueryWindow("", union, s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over a union with a rate arm: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
	assertAttrOnlyGroupKey(t, carryingArmWindow, s)
	assertAttrOnlyGroupKey(t, droppingArmWindow, s)
}

// TestSubqueryPreservedNameExpr_UnionAllOneArmDropsRejected: a single
// name-dropping arm makes the union's MetricName column only partly
// populated, which would report `__name__` on some series and not
// others. Reject the whole union.
func TestSubqueryPreservedNameExpr_UnionAllOneArmDropsRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	union := &chplan.UnionAll{Inputs: []chplan.Node{
		nameBearingProject(s),
		droppedNameProject(s),
	}}
	identity := attrOnlySubqueryWindow("", union, s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over a partly-named union: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_EmptyUnionAllRejected: an armless union
// exposes no columns at all, so "every arm carries a name" must not be
// vacuously true.
func TestSubqueryPreservedNameExpr_EmptyUnionAllRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", &chplan.UnionAll{}, s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over an armless union: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_UnknownNodeRejected: an Aggregate (or
// any other reshaping node) has no MetricName slot — PromQL drops the
// name across an aggregation anyway — so the default arm must reject
// rather than walk blindly into it.
//
// A bare Scan is deliberately NOT the exemplar here: MetricName is one of
// the metrics table's own columns, so the walk recognises it and carries
// the name (see nodeCarriesMetricName's Scan arm, which is what lets a
// native-eligible selector — Filter over Scan, no shaping Project — keep
// its name). Aggregate is the node that genuinely exposes no name slot.
func TestSubqueryPreservedNameExpr_UnknownNodeRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	agg := &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases: []string{s.AttributesColumn},
		AggFuncs: []chplan.AggFunc{{
			Name:  "sum",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			Alias: s.ValueColumn,
		}},
	}
	identity := attrOnlySubqueryWindow("", agg, s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr over an Aggregate: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_BareScanCarriesName is the positive twin
// of the case above, and the reason the Scan arm exists at all: a
// `{__name__=~"a|b"}` selector that resolves to ONE table lowers to
// Filter-over-Scan with no shaping Project, the shape the native ts_grid
// path requires. MetricName is a real column of that relation, so the name
// must ride the window's grouping key rather than be synthesised away.
func TestSubqueryPreservedNameExpr_BareScanCarriesName(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: s.MetricNameColumn},
			Right: &chplan.LitString{V: "cpu_temp"},
		},
	}, s)

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)
	ref, ok := got.(*chplan.ColumnRef)
	if !ok || ref.Name != s.MetricNameColumn {
		t.Fatalf("name expr over Filter-over-Scan: got %#v, want a %s ColumnRef",
			got, s.MetricNameColumn)
	}
}

// TestSubqueryPreservedNameExpr_SchemaWithoutMetricNameColumn: no
// per-series name exists to carry, so the ColumnRef would reference a
// column that is not there. Same branch preservedNameExpr takes at the
// leaf.
func TestSubqueryPreservedNameExpr_SchemaWithoutMetricNameColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = ""
	identity := attrOnlySubqueryWindow("", nameBearingProject(s), s)

	if got := subqueryPreservedNameExpr(identity, "last_over_time", s); got != nil {
		t.Fatalf("name expr for a nameless schema: got %#v, want nil", got)
	}
	assertAttrOnlyGroupKey(t, identity, s)
}

// TestSubqueryPreservedNameExpr_DoesNotDuplicateExistingKey pins
// idempotence across the spine walk: a window that already groups on
// MetricName must not gain a second copy, which would be a redundant
// GROUP BY key and a churned golden.
func TestSubqueryPreservedNameExpr_DoesNotDuplicateExistingKey(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	identity := attrOnlySubqueryWindow("", nameBearingProject(s), s)
	identity.GroupBy = append(identity.GroupBy, &chplan.ColumnRef{Name: s.MetricNameColumn})

	got := subqueryPreservedNameExpr(identity, "last_over_time", s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	assertNameGroupKey(t, identity, s)
}

// TestProjectionOutputName pins the alias-then-bare-ColumnRef rule: a
// computed expression with no alias names no output column, so it must
// never be mistaken for a MetricName projection.
func TestProjectionOutputName(t *testing.T) {
	t.Parallel()
	cases := []struct {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := projectionOutputName(tc.proj); got != tc.want {
				t.Fatalf("projectionOutputName: got %q, want %q", got, tc.want)
			}
		})
	}
}
