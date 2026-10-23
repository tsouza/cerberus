package choptwire_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestSetServerStrategies_PrewhereByServerFollowsTheResolvedSet pins that the
// heads leave the PREWHERE split to the server exactly when the resolved
// optimizations include move_to_prewhere: auto on a server inside its auto
// range, and not on one outside it.
func TestSetServerStrategies_PrewhereByServerFollowsTheResolvedSet(t *testing.T) {
	for _, tc := range []struct {
		server chopt.Version
		want   bool
	}{
		{chopt.Version{Major: 24, Minor: 8}, false},
		{chopt.Version{Major: 25, Minor: 9}, true},
		{chopt.Version{Major: 26, Minor: 6}, false},
	} {
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: "auto"}, tc.server)
		if err != nil {
			t.Fatalf("chopt.Resolve on %v: %v", tc.server, err)
		}
		cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
		choptwire.SetServerStrategies(cell, set)
		got := cell.Load()
		if got.Metrics.PrewhereByServer != tc.want || got.Logs.PrewhereByServer != tc.want || got.Traces.PrewhereByServer != tc.want {
			t.Errorf("server %v: loaded strategies %+v; want PrewhereByServer=%v on every signal", tc.server, got, tc.want)
		}
	}
}

// TestSetServerStrategies_CanonicalMapTypeCastFollowsTheResolvedSet pins that
// the heads cast the canonical series key exactly when the resolved
// optimizations include canonical_map_type_cast: auto below 25.1, whose
// mapSort returns a type a spilled read-back rejects, and not from 25.1.
func TestSetServerStrategies_CanonicalMapTypeCastFollowsTheResolvedSet(t *testing.T) {
	for _, tc := range []struct {
		server chopt.Version
		want   bool
	}{
		{chopt.Version{Major: 24, Minor: 8}, true},
		{chopt.Version{Major: 24, Minor: 12}, true},
		{chopt.Version{Major: 25, Minor: 1}, false},
		{chopt.Version{Major: 26, Minor: 6}, false},
	} {
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: "auto"}, tc.server)
		if err != nil {
			t.Fatalf("chopt.Resolve on %v: %v", tc.server, err)
		}
		cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
		choptwire.SetServerStrategies(cell, set)
		got := cell.Load()
		if got.Metrics.CanonicalMapTypeCast != tc.want || got.Logs.CanonicalMapTypeCast != tc.want || got.Traces.CanonicalMapTypeCast != tc.want {
			t.Errorf("server %v: loaded strategies %+v; want CanonicalMapTypeCast=%v on every signal", tc.server, got, tc.want)
		}
	}
}
