package tempo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBuildAttributeValuesSQL_AutoScopeMaterializedColumnRouting pins
// cerberus issue #2870's tag-values routing contract: the auto-scope
// (`.x` leading-dot, or the bare-key V1 fallback) form unions each side's
// best available read — a materialized column's narrow, arrayJoin-free
// read where the key is materialized in that map, a map subscript
// (mapContains-gated) where it isn't — and drops the map-only
// arrayJoin-over-both-maps shape entirely once EITHER side is
// materialized. A key materialized in neither map keeps today's single
// arrayJoin shape unchanged.
func TestBuildAttributeValuesSQL_AutoScopeMaterializedColumnRouting(t *testing.T) {
	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = map[string]string{
		"http.status_code": "__cerberus_materialized_http.status_code",
		"both.key":         "__cerberus_materialized_span_both.key",
	}
	s.MaterializedResourceAttributeColumns = map[string]string{
		"k8s.namespace.name": "__cerberus_materialized_k8s.namespace.name",
		"both.key":           "__cerberus_materialized_resource_both.key",
	}

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()

	cases := []struct {
		name string
		key  string
		// wantMaterializedMarkers are __cerberus_materialized_* substrings
		// the rendered SQL must contain — one per materialized side.
		wantMaterializedMarkers []string
		// wantUnionAll is true whenever at least one side is materialized:
		// the query switches from the single arrayJoin shape to a UNION
		// ALL of per-side arms.
		wantUnionAll bool
		// wantMapBacked is true whenever at least one side still needs the
		// map subscript / mapContains fallback (unconfigured side, or
		// neither side materialized).
		wantMapBacked bool
	}{
		{
			name:                    "materialized_in_both",
			key:                     "both.key",
			wantMaterializedMarkers: []string{"__cerberus_materialized_span_both.key", "__cerberus_materialized_resource_both.key"},
			wantUnionAll:            true,
			wantMapBacked:           false,
		},
		{
			name:                    "materialized_in_span_only",
			key:                     "http.status_code",
			wantMaterializedMarkers: []string{"__cerberus_materialized_http.status_code"},
			wantUnionAll:            true,
			wantMapBacked:           true,
		},
		{
			name:                    "materialized_in_resource_only",
			key:                     "k8s.namespace.name",
			wantMaterializedMarkers: []string{"__cerberus_materialized_k8s.namespace.name"},
			wantUnionAll:            true,
			wantMapBacked:           true,
		},
		{
			name:                    "materialized_in_neither",
			key:                     "rpc.method",
			wantMaterializedMarkers: nil,
			wantUnionAll:            false,
			wantMapBacked:           true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlStr, _ := tempo.BuildAttributeValuesSQLForTest(s, tc.key, tempo.AttrMapScopeAnyForTest, nil, start, end, nil)

			for _, marker := range tc.wantMaterializedMarkers {
				if !strings.Contains(sqlStr, marker) {
					t.Errorf("SQL missing expected materialized column %q: %s", marker, sqlStr)
				}
			}
			gotUnionAll := strings.Contains(sqlStr, "UNION ALL")
			if gotUnionAll != tc.wantUnionAll {
				t.Errorf("UNION ALL present = %v, want %v; SQL: %s", gotUnionAll, tc.wantUnionAll, sqlStr)
			}
			gotMapBacked := strings.Contains(sqlStr, "mapContains") || strings.Contains(sqlStr, "arrayJoin")
			if gotMapBacked != tc.wantMapBacked {
				t.Errorf("map-backed arm present = %v, want %v; SQL: %s", gotMapBacked, tc.wantMapBacked, sqlStr)
			}
			if tc.wantUnionAll {
				// Once routed to the union shape, the arrayJoin-over-array
				// idiom the map-only path uses must be entirely gone —
				// each arm is either a plain column read or a map
				// subscript, never an arrayJoin fan-out.
				if strings.Contains(sqlStr, "arrayJoin") {
					t.Errorf("union-routed SQL unexpectedly contains arrayJoin: %s", sqlStr)
				}
			} else {
				if !strings.Contains(sqlStr, "arrayJoin") {
					t.Errorf("materialized-in-neither SQL should keep the unchanged arrayJoin shape: %s", sqlStr)
				}
			}
		})
	}
}

// TestBuildAttributeValuesSQL_AutoScopeMaterializedUnchangedForConfiguredOtherKey
// confirms that a key NOT present in either registry renders identically
// whether or not the schema carries materialized registries for OTHER
// keys — mirroring
// TestBuildAttributeValuesSQL_UnmaterializedSchemaUnchanged's single-scope
// coverage for the auto-scope form.
func TestBuildAttributeValuesSQL_AutoScopeMaterializedUnchangedForConfiguredOtherKey(t *testing.T) {
	withRegistry := schema.DefaultOTelTraces()
	withRegistry.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"}
	without := schema.DefaultOTelTraces()

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()

	gotWith, _ := tempo.BuildAttributeValuesSQLForTest(withRegistry, "rpc.method", tempo.AttrMapScopeAnyForTest, nil, start, end, nil)
	gotWithout, _ := tempo.BuildAttributeValuesSQLForTest(without, "rpc.method", tempo.AttrMapScopeAnyForTest, nil, start, end, nil)
	if gotWith != gotWithout {
		t.Errorf("unconfigured key's auto-scope SQL differs based on registry presence:\nwith registry:    %s\nwithout registry: %s", gotWith, gotWithout)
	}
}
