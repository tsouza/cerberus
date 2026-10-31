package main

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// shapeDefectsOnFleet decides which shape defects (chopt.ShapeDefects) the
// engine must gate queries against, given what the fleet probe saw and the
// set in force. It is conservative across nodes, like the analyzer decision:
// a query of the defect's shape can land on any reached node, so one node on
// an affected build is enough to carry the defect. A complete pass with every
// node outside its ranges clears it; a pass that could not reach every node
// and saw no affected build leaves the decision in force, because the node it
// missed may be the affected one.
func shapeDefectsOnFleet(fv chclient.FleetVersions, current chopt.EnabledSet) []string {
	var ids []string
	for _, d := range chopt.ShapeDefects() {
		affected := slices.ContainsFunc(fv.Versions, func(v chopt.Version) bool {
			return chopt.KnownShapeDefect(d.ID, v)
		})
		if affected || (!fv.Complete && current.HasShapeDefect(d.ID)) {
			ids = append(ids, d.ID)
		}
	}
	return ids
}
