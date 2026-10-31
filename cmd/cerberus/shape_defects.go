package main

import (
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// shapeDefectsOnFleet decides how the engine handles each shape defect
// (chopt.ShapeDefects), given what the fleet probe saw and the set in force
// (chopt.FleetDefectGate): gated while a reached node carries it and every
// reached node accepts the gate's setting, refused while a reached node
// carries it and another is too old to accept the setting.
func shapeDefectsOnFleet(fv chclient.FleetVersions, current chopt.EnabledSet) map[string]chopt.DefectGate {
	gates := map[string]chopt.DefectGate{}
	for _, d := range chopt.ShapeDefects() {
		affected := func(v chopt.Version) bool { return chopt.KnownShapeDefect(d.ID, v) }
		if g := chopt.FleetDefectGate(fv.Versions, fv.Complete, affected, d.SettingSince, current.ShapeDefectGate(d.ID)); g != chopt.DefectAbsent {
			gates[d.ID] = g
		}
	}
	return gates
}
