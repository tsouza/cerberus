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

// TestSetServerStrategies_PrewhereInWhereOrderFollowsTheSet pins that every
// head orders its filters for a server that keeps WHERE order exactly when
// the set says every server does, and that Resolve alone never says so.
func TestSetServerStrategies_PrewhereInWhereOrderFollowsTheSet(t *testing.T) {
	set, _, err := chopt.Resolve(chopt.Config{Optimizations: "auto"}, chopt.Version{Major: 24, Minor: 8})
	if err != nil {
		t.Fatalf("chopt.Resolve: %v", err)
	}
	for _, want := range []bool{false, true} {
		if want {
			set = set.WithPrewhereInWhereOrder(true)
		}
		cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
		choptwire.SetServerStrategies(cell, set)
		got := cell.Load()
		if got.Metrics.PrewhereInWhereOrder != want || got.Logs.PrewhereInWhereOrder != want || got.Traces.PrewhereInWhereOrder != want {
			t.Errorf("set PrewhereInWhereOrder=%v: loaded strategies %+v; want it on every signal", set.PrewhereInWhereOrder(), got)
		}
	}
}

// TestSetServerStrategies_SearchDrainsFollowTheResolvedSet pins that a
// bounded search tests its chosen traces in the scan's PREWHERE as a scalar
// exactly when the resolved optimizations include search_drain_scalar_traces
// (auto on 26.2 and 26.3, where the server skips granules by the trace id
// skip index for the scalar and not for a GLOBAL IN set), and bounds a
// search that ranks the rows it drains by its chosen traces' earliest and
// latest timestamp exactly when they include search_drain_trace_bounds (auto
// on the same lines).
func TestSetServerStrategies_SearchDrainsFollowTheResolvedSet(t *testing.T) {
	for _, tc := range []struct {
		server                    chopt.Version
		scalarTraces, traceBounds bool
	}{
		{chopt.Version{Major: 24, Minor: 8}, false, false},
		{chopt.Version{Major: 26, Minor: 1}, false, false},
		{chopt.Version{Major: 26, Minor: 2}, true, true},
		{chopt.Version{Major: 26, Minor: 3}, true, true},
		{chopt.Version{Major: 26, Minor: 4}, false, false},
		{chopt.Version{Major: 26, Minor: 8}, false, false},
	} {
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: "auto"}, tc.server)
		if err != nil {
			t.Fatalf("chopt.Resolve on %v: %v", tc.server, err)
		}
		cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
		choptwire.SetServerStrategies(cell, set)
		got := cell.Load()
		for _, signal := range []chsql.SchemaStrategies{got.Metrics, got.Logs, got.Traces} {
			if signal.SearchDrainScalarTraces != tc.scalarTraces || signal.SearchDrainTraceBounds != tc.traceBounds {
				t.Errorf("server %v: loaded strategies %+v; want SearchDrainScalarTraces=%v and SearchDrainTraceBounds=%v on every signal",
					tc.server, got, tc.scalarTraces, tc.traceBounds)
			}
		}
	}

	// Each strategy follows its own feature: an explicit listing of one
	// leaves the other off.
	for _, tc := range []struct {
		feature                   string
		scalarTraces, traceBounds bool
	}{
		{chopt.FeatureSearchDrainScalarTraces, true, false},
		{chopt.FeatureSearchDrainTraceBounds, false, true},
	} {
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: tc.feature}, chopt.Version{Major: 26, Minor: 8})
		if err != nil {
			t.Fatalf("chopt.Resolve %s: %v", tc.feature, err)
		}
		cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
		choptwire.SetServerStrategies(cell, set)
		if got := cell.Load().Traces; got.SearchDrainScalarTraces != tc.scalarTraces || got.SearchDrainTraceBounds != tc.traceBounds {
			t.Errorf("only %s: traces strategies %+v; want SearchDrainScalarTraces=%v and SearchDrainTraceBounds=%v", tc.feature, got, tc.scalarTraces, tc.traceBounds)
		}
	}
}
