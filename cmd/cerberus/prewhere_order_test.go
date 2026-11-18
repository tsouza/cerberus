package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// Builds on either side of chopt.PrewhereReorderedSince.
var (
	whereOrderBuild = chopt.Version{Major: 24, Minor: 8, Patch: 14, Build: 39}
	reorderingBuild = chopt.Version{Major: 26, Minor: 8, Patch: 36, Build: 3}
)

// TestPrewhereInWhereOrderOnFleet pins the decision table: only a complete
// pass with every node on a build that keeps WHERE order sets it; a pass that
// read no version, missed a node, or reached a reordering node leaves it
// unset, which keeps the plans a reordering server reads best.
func TestPrewhereInWhereOrderOnFleet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fv   chclient.FleetVersions
		want bool
	}{
		{"every node keeps WHERE order", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, whereOrderBuild}, Complete: true}, true},
		{"one node reorders", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, reorderingBuild}, Complete: true}, false},
		{"every node reorders", chclient.FleetVersions{Versions: []chopt.Version{reorderingBuild}, Complete: true}, false},
		{"a node unreachable", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild}}, false},
		{"nothing reachable", chclient.FleetVersions{}, false},
		{"complete pass with no version", chclient.FleetVersions{Complete: true}, false},
	} {
		if got := prewhereInWhereOrderOnFleet(tc.fv); got != tc.want {
			t.Errorf("%s: prewhereInWhereOrderOnFleet = %v; want %v", tc.name, got, tc.want)
		}
	}
}
