package tempo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBuildAttributeValuesSQL_MaterializedColumnRouting pins cerberus
// issue #2776's tag-values routing contract: a single-scope (resource.x /
// span.x) lookup whose key is materialized reads the narrow column
// directly — no arrayJoin, no mapContains, no map subscript — while an
// unconfigured key keeps today's map-backed shape unchanged. The
// auto-scope (`.x`) form's OWN materialized routing (cerberus issue
// #2870, which unions a per-side materialized/map-backed read rather than
// short-circuiting the whole query to one column) is pinned separately in
// TestBuildAttributeValuesSQL_AutoScopeMaterializedColumnRouting
// (search_tag_values_autoscope_materialized_test.go) — the all-or-nothing
// "materialized ⇒ no arrayJoin/mapContains anywhere" assertion below
// doesn't fit its union shape, which can legitimately keep mapContains on
// whichever side has no materialized entry for the key.
func TestBuildAttributeValuesSQL_MaterializedColumnRouting(t *testing.T) {
	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"}
	s.MaterializedResourceAttributeColumns = map[string]string{"k8s.namespace.name": "__cerberus_materialized_k8s.namespace.name"}

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()

	cases := []struct {
		name             string
		key              string
		scope            tempo.AttrMapScopeForTest
		wantMaterialized bool
	}{
		{"span_materialized", "http.status_code", tempo.AttrMapScopeSpanForTest, true},
		{"resource_materialized", "k8s.namespace.name", tempo.AttrMapScopeResourceForTest, true},
		{"span_unconfigured_key_stays_on_map", "rpc.method", tempo.AttrMapScopeSpanForTest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlStr, _ := tempo.BuildAttributeValuesSQLForTest(s, tc.key, tc.scope, nil, start, end, nil)
			isMaterialized := strings.Contains(sqlStr, "__cerberus_materialized")
			if isMaterialized != tc.wantMaterialized {
				t.Errorf("materialized routing = %v, want %v; SQL: %s", isMaterialized, tc.wantMaterialized, sqlStr)
			}
			if tc.wantMaterialized {
				for _, notWant := range []string{"arrayJoin", "mapContains"} {
					if strings.Contains(sqlStr, notWant) {
						t.Errorf("materialized-column SQL unexpectedly contains %q: %s", notWant, sqlStr)
					}
				}
			}
		})
	}
}

// TestBuildAttributeValuesSQL_UnmaterializedSchemaUnchanged confirms
// schema.DefaultOTelTraces()'s nil registries produce byte-identical SQL
// to before this feature existed — the opt-in gate at the tag-values layer.
func TestBuildAttributeValuesSQL_UnmaterializedSchemaUnchanged(t *testing.T) {
	withRegistry := schema.DefaultOTelTraces()
	withRegistry.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"}
	without := schema.DefaultOTelTraces()

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()

	// A DIFFERENT key than the one configured in withRegistry must render
	// identically regardless of whether the registry exists at all.
	gotWith, _ := tempo.BuildAttributeValuesSQLForTest(withRegistry, "rpc.method", tempo.AttrMapScopeSpanForTest, nil, start, end, nil)
	gotWithout, _ := tempo.BuildAttributeValuesSQLForTest(without, "rpc.method", tempo.AttrMapScopeSpanForTest, nil, start, end, nil)
	if gotWith != gotWithout {
		t.Errorf("unconfigured key's SQL differs based on registry presence:\nwith registry:    %s\nwithout registry: %s", gotWith, gotWithout)
	}
}
