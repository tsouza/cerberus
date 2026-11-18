package main

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// prewhereInWhereOrderOnFleet decides whether every server the heads read
// runs a multi-step PREWHERE in the order of the WHERE conjuncts
// (chopt.EnabledSet.PrewhereInWhereOrder), given what the fleet probe saw and
// the decision in force. fv is the remembered fleet (rememberingFleetProber).
// A reached node on a build that reorders the conditions clears it, because a
// query can land on that node. A complete pass that read every node on a
// build that keeps WHERE order sets it. A pass that missed a node and saw no
// reordering build leaves the decision in force, as analyzerMandatoryOnFleet
// does; at boot nothing is in force, so the plans start in the arrangement a
// reordering server reads best.
func prewhereInWhereOrderOnFleet(fv chclient.FleetVersions, current bool) bool {
	if slices.ContainsFunc(fv.Versions, func(v chopt.Version) bool { return !chopt.PrewhereKeepsWhereOrder(v) }) {
		return false
	}
	if !fv.Complete || len(fv.Versions) == 0 {
		return current
	}
	return true
}
