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

// TestPromqlTopLevelKeysForOuterBy pins the normalisation of by-clause
// labels to the underscored Prom-canonical spelling so the outer
// aggregate's `Attributes['service_name']` lookup hits the synthesised
// map key regardless of which spelling the user wrote.
func TestPromqlTopLevelKeysForOuterBy(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	cases := []struct {
		name string
		in   []string
		want [][2]string
	}{
		{"empty", nil, nil},
		{"underscored", []string{"service_name"}, [][2]string{{"service_name", s.ServiceNameColumn}}},
		{"dotted", []string{"service.name"}, [][2]string{{"service_name", s.ServiceNameColumn}}},
		{"both_collapse", []string{"service_name", "service.name"}, [][2]string{{"service_name", s.ServiceNameColumn}}},
		{"unrelated_dropped", []string{"job", "service_name", "instance"}, [][2]string{{"service_name", s.ServiceNameColumn}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := promqlTopLevelKeysForOuterBy(tc.in, s)
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

// TestAugmentSelectorAttributes_NoResourceColumnIsPassThrough pins that
// the augmenting Project is suppressed when the schema clears
// ResourceAttributesColumn AND the outer by-clause references no
// top-level-routed label — the opt-out path stays byte-stable.
func TestAugmentSelectorAttributes_NoResourceColumnIsPassThrough(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ResourceAttributesColumn = "" // opt out of the resource merge
	input := &chplan.Scan{Table: s.GaugeTable}
	ctx := lowerCtx{}
	if got := augmentSelectorAttributes(input, ctx, s); got != chplan.Node(input) {
		t.Errorf("no-outer-by: expected pass-through, got %#v", got)
	}
	// `by(job)` references no top-level column → still pass-through.
	ctx = ctx.withOuterByLabels([]string{"job"})
	if got := augmentSelectorAttributes(input, ctx, s); got != chplan.Node(input) {
		t.Errorf("by(job): expected pass-through, got %#v", got)
	}
}

// TestAugmentSelectorAttributes_ResourceMergeWrapsProject pins that with
// the default schema (ResourceAttributesColumn set) the selector ALWAYS
// wraps a Project that rebinds Attributes to the resource-merge base
// `mapUpdate(sanitize(ResourceAttributes), Attributes)` — even with no
// outer-by label — so bare selectors surface resource labels.
func TestAugmentSelectorAttributes_ResourceMergeWrapsProject(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	input := &chplan.Scan{Table: s.GaugeTable}
	got := augmentSelectorAttributes(input, lowerCtx{}, s)
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("expected a Project wrap with the resource merge active, got %T", got)
	}
	var attrsExpr chplan.Expr
	for _, p := range proj.Projections {
		if p.Alias == s.AttributesColumn {
			attrsExpr = p.Expr
		}
	}
	call, ok := attrsExpr.(*chplan.FuncCall)
	if !ok || call.Name != "mapUpdate" {
		t.Fatalf("Attributes projection: got %v, want mapUpdate(sanitize(RA), Attributes)", attrsExpr)
	}
}

// TestAugmentSelectorAttributes_ServiceNameWrapsProject pins the
// augmenting Project shape when the outer by-clause references
// `service_name`. The Attributes slot becomes
// `mapConcat(Attributes, mapFilter(v != ”, map('service_name',
// toString(ServiceName))))` so the downstream LWR / RangeWindow's
// `GROUP BY Attributes` partitions over distinct ServiceName values.
func TestAugmentSelectorAttributes_ServiceNameWrapsProject(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	input := &chplan.Scan{Table: s.GaugeTable}
	ctx := lowerCtx{}.withOuterByLabels([]string{"service_name"})

	got := augmentSelectorAttributes(input, ctx, s)
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("got %T, want *chplan.Project", got)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("projections: got %d, want 4", len(proj.Projections))
	}
	attrsProj := proj.Projections[1]
	if attrsProj.Alias != s.AttributesColumn {
		t.Errorf("alias[1]: got %q, want %q", attrsProj.Alias, s.AttributesColumn)
	}
	mapConcat, ok := attrsProj.Expr.(*chplan.FuncCall)
	if !ok || mapConcat.Name != "mapConcat" {
		t.Fatalf("attrs expr: got %#v, want mapConcat(...)", attrsProj.Expr)
	}
}
