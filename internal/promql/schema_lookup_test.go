package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSchemaTopLevelColumn_ServiceName pins the matcher-routing
// invariant for task #232: both Prom-grammar (`service_name`) and
// OTel-canonical (`service.name`) spellings of the service-name label
// resolve to [schema.Metrics.ServiceNameColumn]. Other labels keep
// returning "" so the lowering falls back to the Attributes map.
func TestSchemaTopLevelColumn_ServiceName(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	cases := []struct {
		label string
		want  string
	}{
		{"service_name", s.ServiceNameColumn},
		{"service.name", s.ServiceNameColumn},
		{"job", ""},
		{"__name__", ""},
		{"", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			if got := schemaTopLevelColumn(s, tc.label); got != tc.want {
				t.Errorf("schemaTopLevelColumn(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

// TestSchemaTopLevelColumn_ClearedFieldOptsOut pins the custom-schema
// opt-out: a user who clears [schema.Metrics.ServiceNameColumn] takes
// the bare-map fallback because [schemaTopLevelColumn] reads the
// schema field rather than a static allow-list of column names.
func TestSchemaTopLevelColumn_ClearedFieldOptsOut(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ServiceNameColumn = ""
	if got := schemaTopLevelColumn(s, "service_name"); got != "" {
		t.Errorf("schemaTopLevelColumn with cleared ServiceNameColumn = %q, want \"\"", got)
	}
}

// TestMatcherToExpr_ServiceNameRoutesToCoalesce pins the
// matcher-lowering shape for `service_name` / `service.name` matchers:
// both spellings emit `coalesce(nullIf(ServiceName, ”),
// Attributes['service.name']-fallback-chain)` so producers that
// wrote either side (top-level column or map key) both match. Mirrors
// the LogQL fix in [internal/logql.matcherToExpr] (PR #669).
func TestMatcherToExpr_ServiceNameRoutesToCoalesce(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	for _, spelling := range []string{"service_name", "service.name"} {
		spelling := spelling
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			m, err := labels.NewMatcher(labels.MatchEqual, spelling, "cerberus")
			if err != nil {
				t.Fatalf("NewMatcher: %v", err)
			}
			expr := matcherToExpr(m, s)
			bin, ok := expr.(*chplan.Binary)
			if !ok {
				t.Fatalf("expr type: got %T, want *chplan.Binary", expr)
			}
			if bin.Op != chplan.OpEq {
				t.Errorf("Op: got %v, want OpEq", bin.Op)
			}
			coalesce, ok := bin.Left.(*chplan.FuncCall)
			if !ok || coalesce.Name != "coalesce" {
				t.Fatalf("lhs: got %#v, want coalesce(...)", bin.Left)
			}
			if len(coalesce.Args) != 2 {
				t.Fatalf("coalesce args: got %d, want 2", len(coalesce.Args))
			}
			nullIf, ok := coalesce.Args[0].(*chplan.FuncCall)
			if !ok || nullIf.Name != "nullIf" {
				t.Fatalf("coalesce arg0: got %#v, want nullIf(...)", coalesce.Args[0])
			}
			col, ok := nullIf.Args[0].(*chplan.ColumnRef)
			if !ok || col.Name != s.ServiceNameColumn {
				t.Errorf("nullIf arg0: got %#v, want ColumnRef(%q)", nullIf.Args[0], s.ServiceNameColumn)
			}
		})
	}
}

// TestMatcherToExpr_OtherLabelCoalescesResourceAttributes pins that a
// non-service, non-__name__ label resolves against BOTH the Attributes
// map AND the ResourceAttributes map (rc.5 BRANCH 4), Attributes winning:
// `coalesce(nullIf(Attributes[k], ”), nullIf(ResourceAttributes[k], ”),
// ”)`. The default schema names a ResourceAttributes column, so the
// resource arm is active.
func TestMatcherToExpr_OtherLabelCoalescesResourceAttributes(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	m, err := labels.NewMatcher(labels.MatchEqual, "job", "api")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	expr := matcherToExpr(m, s)
	bin, ok := expr.(*chplan.Binary)
	if !ok {
		t.Fatalf("expr type: got %T, want *chplan.Binary", expr)
	}
	// `job` with the resource arm active → coalesce-over-two-maps with an
	// empty-string floor (Attributes wins, absent-in-both → '').
	call, ok := bin.Left.(*chplan.FuncCall)
	if !ok || call.Name != "coalesce" {
		t.Fatalf("lhs: got %T (%v), want *chplan.FuncCall coalesce", bin.Left, bin.Left)
	}
	if len(call.Args) != 3 {
		t.Fatalf("coalesce arity: got %d, want 3 (Attributes, ResourceAttributes, '' floor)", len(call.Args))
	}
	// Arg 0 is the Attributes side, wrapped in nullIf; the inner lookup is
	// the bare MapAccess (job has no underscore).
	attrSide, ok := call.Args[0].(*chplan.FuncCall)
	if !ok || attrSide.Name != "nullIf" {
		t.Errorf("arg0: got %v, want nullIf(Attributes[...], '')", call.Args[0])
	} else if _, ok := attrSide.Args[0].(*chplan.MapAccess); !ok {
		t.Errorf("arg0 inner: got %T, want *chplan.MapAccess (Attributes['job'])", attrSide.Args[0])
	}
	// Arg 2 is the empty-string floor so negative matchers keep
	// absent-in-both rows.
	if lit, ok := call.Args[2].(*chplan.LitString); !ok || lit.V != "" {
		t.Errorf("arg2: got %v, want empty-string floor LitString{''}", call.Args[2])
	}
}

// TestMatcherToExpr_NoResourceColumnStaysAttributesMap pins that a custom
// schema that CLEARS ResourceAttributesColumn opts out of BRANCH 4 and
// keeps the legacy bare Attributes-map lookup — byte-stable for the
// opt-out path.
func TestMatcherToExpr_NoResourceColumnStaysAttributesMap(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ResourceAttributesColumn = ""
	m, err := labels.NewMatcher(labels.MatchEqual, "job", "api")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	expr := matcherToExpr(m, s)
	bin, ok := expr.(*chplan.Binary)
	if !ok {
		t.Fatalf("expr type: got %T, want *chplan.Binary", expr)
	}
	if _, ok := bin.Left.(*chplan.MapAccess); !ok {
		t.Errorf("lhs: got %T, want *chplan.MapAccess (no coalesce wrap when resource arm disabled)", bin.Left)
	}
}

// TestPromqlTopLevelKeys pins that the synthesised key set is derived
// from the SCHEMA, not from the query: the default schema always yields
// the `service_name` → ServiceName pair under its Prom-canonical
// underscored spelling, and clearing the column opts the schema out.
func TestPromqlTopLevelKeys(t *testing.T) {
	t.Parallel()

	def := schema.DefaultOTelMetrics()
	cleared := schema.DefaultOTelMetrics()
	cleared.ServiceNameColumn = ""

	cases := []struct {
		name string
		in   schema.Metrics
		want [][2]string
	}{
		{"default", def, [][2]string{{"service_name", def.ServiceNameColumn}}},
		{"column_cleared", cleared, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := promqlTopLevelKeys(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d]: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPromCanonicalTopLevelLabel pins that both spellings of the OTel key
// collapse to the underscored Prom-canonical form, so a `by(service.name)`
// aggregate's `Attributes['service_name']` lookup hits the synthesised key.
func TestPromCanonicalTopLevelLabel(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"service.name", "service_name"} {
		if got := promCanonicalTopLevelLabel(in); got != "service_name" {
			t.Errorf("%q: got %q, want %q", in, got, "service_name")
		}
	}
	if got := promCanonicalTopLevelLabel("job"); got != "job" {
		t.Errorf("unrelated label rewritten: got %q, want %q", got, "job")
	}
}

// canonicalAttributesInner asserts that expr is the `mapSort` wrapper
// [canonicalAttributesExpr] puts on every projected label map, and
// returns the wrapped expression. Every selector Project must carry it:
// it is the invariant that makes the Attributes Map usable as a
// series-identity key (see canonicalAttributesExpr).
func canonicalAttributesInner(t *testing.T, expr chplan.Expr) chplan.Expr {
	t.Helper()
	call, ok := expr.(*chplan.FuncCall)
	if !ok || call.Name != "mapSort" {
		t.Fatalf("attrs expr is not canonicalised: got %#v, want mapSort(...)", expr)
	}
	if len(call.Args) != 1 {
		t.Fatalf("mapSort args: got %d, want 1", len(call.Args))
	}
	return call.Args[0]
}

// TestAugmentSelectorAttributes_NoColumnsStillCanonicalises pins the
// schema that has NOTHING to merge — neither a ResourceAttributes column
// nor a dedicated top-level one. The Project used to be suppressed here
// as an identity map, which left the raw table's Attributes column
// flowing into every downstream series-identity GROUP BY uncanonicalised
// — the same silent series split the default schema suffered, reachable
// on any custom schema that opts out of the resource merge. The Project
// is now always emitted, carrying `mapSort(Attributes)`.
func TestAugmentSelectorAttributes_NoColumnsStillCanonicalises(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ResourceAttributesColumn = "" // opt out of the resource merge
	s.ServiceNameColumn = ""        // opt out of the dedicated overlay
	input := &chplan.Scan{Table: s.GaugeTable}
	got := augmentSelectorAttributes(input, lowerCtx{}, s)
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("got %T, want a *chplan.Project carrying the canonicalising wrap", got)
	}
	inner := canonicalAttributesInner(t, attributesProjection(t, proj, s))
	ref, ok := inner.(*chplan.ColumnRef)
	if !ok || ref.Name != s.AttributesColumn {
		t.Fatalf("mapSort argument: got %#v, want the bare %s ColumnRef", inner, s.AttributesColumn)
	}
}

// TestAugmentSelectorAttributes_DedicatedColumnSurvivesResourceOptOut
// pins that clearing ResourceAttributesColumn alone does NOT drop the
// dedicated column: the overlay is the only path left that can surface
// `service_name`, so it must still wrap a Project.
func TestAugmentSelectorAttributes_DedicatedColumnSurvivesResourceOptOut(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ResourceAttributesColumn = ""
	input := &chplan.Scan{Table: s.GaugeTable}
	got := augmentSelectorAttributes(input, lowerCtx{}, s)
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("expected a Project wrap with ServiceNameColumn set, got %T", got)
	}
	call, ok := canonicalAttributesInner(t, attributesProjection(t, proj, s)).(*chplan.FuncCall)
	if !ok || call.Name != "mapConcat" {
		t.Fatalf("Attributes projection: got %v, want mapSort(mapConcat(Attributes, <dedicated>))", call)
	}
}

// TestAugmentSelectorAttributes_BareSelectorCarriesServiceName is the
// regression pin for the Layer-14 MIG-07 finding: a BARE selector — no
// enclosing aggregation, no by-clause — must still project the dedicated
// ServiceName column as `service_name`. `service.name` is removed from
// the ResourceAttributes arm, so when this overlay was gated on an outer
// by-clause the label vanished from every plain `up` the collector wrote.
//
// The expected shape is
// `mapConcat(mapUpdate(sanitize(RA), sanitize(Attributes)),
//
//	mapFilter(v != ”, map('service_name', toString(ServiceName))))`.
func TestAugmentSelectorAttributes_BareSelectorCarriesServiceName(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	input := &chplan.Scan{Table: s.GaugeTable}

	got := augmentSelectorAttributes(input, lowerCtx{}, s)
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("got %T, want *chplan.Project", got)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("projections: got %d, want 4", len(proj.Projections))
	}
	mapConcat, ok := canonicalAttributesInner(t, attributesProjection(t, proj, s)).(*chplan.FuncCall)
	if !ok || mapConcat.Name != "mapConcat" {
		t.Fatalf("attrs expr: got %#v, want mapSort(mapConcat(...))", proj.Projections[1].Expr)
	}
	if len(mapConcat.Args) != 2 {
		t.Fatalf("mapConcat args: got %d, want 2", len(mapConcat.Args))
	}
	base, ok := mapConcat.Args[0].(*chplan.FuncCall)
	if !ok || base.Name != "mapUpdate" {
		t.Errorf("overlay base: got %v, want the mapUpdate resource merge", mapConcat.Args[0])
	}
	if !exprMentions(mapConcat.Args[1], s.ServiceNameColumn) {
		t.Errorf("overlay does not read %s: %#v", s.ServiceNameColumn, mapConcat.Args[1])
	}
	if !exprMentions(mapConcat.Args[1], "service_name") {
		t.Errorf("overlay does not synthesise the service_name key: %#v", mapConcat.Args[1])
	}
}

// TestAugmentSelectorAttributes_CatalogModeSkipsOverlay pins that catalog
// (metadata) mode keeps the bare Attributes ColumnRef: it resolves the
// dedicated columns at the leaf via [dedicatedLabelColumns], so building
// the overlay map here would be immediately subscripted away.
func TestAugmentSelectorAttributes_CatalogModeSkipsOverlay(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{catalog: &metadataCatalog{}}
	if got := selectorAttributesExpr(ctx, s); !isBareAttributesRef(got, s) {
		t.Errorf("catalog mode: got %#v, want the bare Attributes ColumnRef", got)
	}
}

// attributesProjection returns the Attributes-aliased projection expr.
func attributesProjection(t *testing.T, proj *chplan.Project, s schema.Metrics) chplan.Expr {
	t.Helper()
	for _, p := range proj.Projections {
		if p.Alias == s.AttributesColumn {
			return p.Expr
		}
	}
	t.Fatalf("no projection aliased %q", s.AttributesColumn)
	return nil
}

// exprMentions reports whether want appears as a column name or string
// literal anywhere in the expression tree — enough to assert that the
// overlay reads the dedicated column and names the synthesised key,
// without pinning the exact nesting the emitter is free to change.
func exprMentions(e chplan.Expr, want string) bool {
	switch v := e.(type) {
	case *chplan.ColumnRef:
		return v.Name == want
	case *chplan.LitString:
		return v.V == want
	case *chplan.InlineString:
		return v.V == want
	case *chplan.BareIdent:
		return v.Name == want
	case *chplan.FuncCall:
		for _, a := range v.Args {
			if exprMentions(a, want) {
				return true
			}
		}
	case *chplan.Lambda:
		return exprMentions(v.Body, want)
	case *chplan.Binary:
		return exprMentions(v.Left, want) || exprMentions(v.Right, want)
	}
	return false
}
