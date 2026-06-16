package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestResourceLabelAllowed pins the allowlist gate that both the matcher
// (BRANCH 4) and the metadata listing share: an empty allowlist promotes
// every key; a non-empty allowlist admits a Prom label iff one of its
// dot<->underscore OTel candidates is allowlisted (matched against the
// ORIGINAL dotted key).
func TestResourceLabelAllowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		allow    []string
		promABel string
		want     bool
	}{
		{"empty_allowlist_promotes_all", nil, "k8s_namespace_name", true},
		{"allowlisted_dotted_key", []string{"k8s.namespace.name"}, "k8s_namespace_name", true},
		{"not_allowlisted", []string{"deployment.environment.name"}, "k8s_namespace_name", false},
		{"allowlist_underscored_form", []string{"k8s_namespace_name"}, "k8s_namespace_name", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelMetrics()
			s.PromResourceLabels = tc.allow
			if got := resourceLabelAllowed(s, tc.promABel); got != tc.want {
				t.Errorf("resourceLabelAllowed(%v, %q) = %v, want %v", tc.allow, tc.promABel, got, tc.want)
			}
		})
	}
}

// TestMatcherToExpr_AllowlistNarrowsResourceArm pins that a matcher on a
// label OUTSIDE the configured allowlist falls back to the
// Attributes-only lookup (no BRANCH-4 resource arm), while an allowlisted
// label keeps the two-map coalesce. This is the matcher-side enforcement
// of CERBERUS_PROM_RESOURCE_LABELS.
func TestMatcherToExpr_AllowlistNarrowsResourceArm(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.PromResourceLabels = []string{"k8s.namespace.name"} // allow ONLY namespace

	// Allowlisted label -> coalesce-over-two-maps (resource arm present).
	mAllowed, err := labels.NewMatcher(labels.MatchEqual, "k8s_namespace_name", "prod")
	if err != nil {
		t.Fatal(err)
	}
	allowedBin, ok := matcherToExpr(mAllowed, s).(*chplan.Binary)
	if !ok {
		t.Fatalf("allowed matcher: got %T, want *chplan.Binary", matcherToExpr(mAllowed, s))
	}
	if call, ok := allowedBin.Left.(*chplan.FuncCall); !ok || call.Name != "coalesce" {
		t.Errorf("allowed matcher LHS: got %v, want coalesce(...) with resource arm", allowedBin.Left)
	}

	// Non-allowlisted label -> bare Attributes map lookup (no resource arm).
	mDenied, err := labels.NewMatcher(labels.MatchEqual, "deployment_environment_name", "prod")
	if err != nil {
		t.Fatal(err)
	}
	deniedBin, ok := matcherToExpr(mDenied, s).(*chplan.Binary)
	if !ok {
		t.Fatalf("denied matcher: got %T, want *chplan.Binary", matcherToExpr(mDenied, s))
	}
	// deployment_environment_name has underscores so the bare lookup is the
	// candidate-chain `if(mapContains(...))`, NOT a coalesce-over-two-maps.
	if call, ok := deniedBin.Left.(*chplan.FuncCall); ok && call.Name == "coalesce" {
		t.Errorf("denied matcher LHS leaked the resource arm: %v", deniedBin.Left)
	}
}

// TestMergeResourceAttributesExpr_AllowlistFilter pins that a non-empty
// allowlist narrows the source ResourceAttributes map with a mapFilter IN
// the merge expression. With the default schema the source is ALWAYS a
// mapFilter because the dedicated-column exclusion (service.name →
// ServiceName) is applied unconditionally; only a schema that clears BOTH
// the allowlist and every dedicated column reads the bare column.
func TestMergeResourceAttributesExpr_AllowlistFilter(t *testing.T) {
	t.Parallel()

	// Default schema (ServiceNameColumn set, no allowlist): the source is a
	// mapFilter that excludes the dedicated keys — NOT a bare column ref.
	sAll := schema.DefaultOTelMetrics()
	src := resourceSourceMap(sAll)
	call, ok := src.(*chplan.FuncCall)
	if !ok || call.Name != "mapFilter" {
		t.Fatalf("default-schema source: got %v, want mapFilter(... NOT IN dedicated keys ...)", src)
	}

	// Non-empty allowlist: source stays a mapFilter (exclusion AND allowlist).
	sNarrow := schema.DefaultOTelMetrics()
	sNarrow.PromResourceLabels = []string{"k8s.namespace.name"}
	narrow := resourceSourceMap(sNarrow)
	ncall, ok := narrow.(*chplan.FuncCall)
	if !ok || ncall.Name != "mapFilter" {
		t.Fatalf("non-empty allowlist source: got %v, want mapFilter(...)", narrow)
	}

	// No dedicated column AND no allowlist: bare column ref (legacy path).
	sBare := schema.DefaultOTelMetrics()
	sBare.ServiceNameColumn = ""
	bare := resourceSourceMap(sBare)
	if ref, ok := bare.(*chplan.ColumnRef); !ok || ref.Name != sBare.ResourceAttributesColumn {
		t.Errorf("no-dedicated/no-allowlist source: got %v, want bare ResourceAttributes ColumnRef", bare)
	}
}

// TestDedicatedResourceLabelExcluded pins the central exclusion: a label
// backed by a dedicated top-level column (service.name → ServiceName) is
// never promoted via the resource arm — in either spelling — while a
// genuine resource key (k8s.namespace.name) still is. Clearing the
// dedicated column re-admits the key (the resource arm becomes its only
// path).
func TestDedicatedResourceLabelExcluded(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	for _, lbl := range []string{"service.name", "service_name"} {
		if !DedicatedResourceLabelExcluded(s, lbl) {
			t.Errorf("DedicatedResourceLabelExcluded(default, %q) = false, want true", lbl)
		}
		// The matcher/listing gate must also deny it.
		if resourceLabelAllowed(s, lbl) {
			t.Errorf("resourceLabelAllowed(default, %q) = true, want false (dedicated column owns it)", lbl)
		}
	}

	// Genuine resource key still promotes.
	if DedicatedResourceLabelExcluded(s, "k8s_namespace_name") {
		t.Error("k8s_namespace_name wrongly excluded — it has no dedicated column")
	}
	if !resourceLabelAllowed(s, "k8s_namespace_name") {
		t.Error("resourceLabelAllowed(default, k8s_namespace_name) = false, want true")
	}

	// Clearing the dedicated column re-admits service.name.
	sNoDedicated := schema.DefaultOTelMetrics()
	sNoDedicated.ServiceNameColumn = ""
	if DedicatedResourceLabelExcluded(sNoDedicated, "service.name") {
		t.Error("service.name excluded even with ServiceNameColumn cleared — should fall back to the resource arm")
	}
}

// TestMergeResourceAttributesExpr_NoColumnIsBareAttributes pins the
// custom-schema opt-out: clearing ResourceAttributesColumn collapses the
// merge to the bare Attributes ColumnRef so legacy emit is byte-stable.
func TestMergeResourceAttributesExpr_NoColumnIsBareAttributes(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.ResourceAttributesColumn = ""
	expr := mergeResourceAttributesExpr(s)
	ref, ok := expr.(*chplan.ColumnRef)
	if !ok || ref.Name != s.AttributesColumn {
		t.Errorf("opt-out merge: got %v, want bare Attributes ColumnRef", expr)
	}
}
