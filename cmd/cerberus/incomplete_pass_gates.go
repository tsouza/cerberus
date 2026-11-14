package main

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// gatesInForce returns the defect-gate decisions a fleet probe pass is judged
// against: the shape defects (carried on the returned set) and the
// condition-cache override (the returned bool) that shapeDefectsOnFleet and
// conditionCacheOverride keep when the pass could not reach every node.
// inForce is the resolution in force (nil at boot), and conditionCacheOff the
// override the client carries now.
//
// When inForce describes the fleet, those are its own decisions. At boot
// nothing is in force yet, and a floor fallback records that no node answered,
// so neither says anything about a node the pass missed — which may run any
// build, including one inside a wrong-result range no reached node carries.
// Then every gate is in force on top of whatever is: each shape defect, and the
// condition-cache override. A gate stays off when the lowest build the pass
// reached predates its setting (chopt.GateSettings), because every reached node
// on that build would reject each query the gate stamps; a pass that reached no
// node adds no gate. The analyzer decision has no such stand-in: a node that
// refuses enable_analyzer=0 rejects the query rather than answering it wrong,
// and the rejection asks for an early re-probe.
//
// A complete pass never reads what this returns: both decisions then follow the
// nodes it reached alone, which is how a later complete pass clears the gates
// an incomplete one started on.
func gatesInForce(fv chclient.FleetVersions, inForce *chOptResolution, conditionCacheOff bool) (chopt.EnabledSet, bool) {
	var current chopt.EnabledSet
	if inForce != nil {
		current = inForce.Set
	}
	lowest, reached := fv.Lowest()
	if inForce.describesFleet() || !reached {
		return current, conditionCacheOff
	}
	ids := current.ShapeDefectIDs()
	for _, d := range chopt.ShapeDefects() {
		if !slices.Contains(ids, d.ID) && gateSettingAccepted(lowest, d.Setting) {
			ids = append(ids, d.ID)
		}
	}
	return current.WithShapeDefects(ids), conditionCacheOff || gateSettingAccepted(lowest, chopt.SettingUseQueryConditionCache)
}

// gateSettingAccepted reports whether a node on build v accepts gate setting
// name: whether v is at or above the setting's first accepting release.
func gateSettingAccepted(v chopt.Version, name string) bool {
	g, ok := chopt.GateSettingByName(name)
	return ok && v.AtLeast(g.Since)
}
