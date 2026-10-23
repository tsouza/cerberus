package main

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// analyzerMandatoryOnFleet decides whether queries must no longer disable the
// analyzer (chopt.EnabledSet.AnalyzerMandatory), given what the fleet probe
// saw and the decision in force. It is conservative across nodes in the
// opposite direction to feature resolution: a reached node on a build where
// the analyzer is mandatory refuses enable_analyzer=0, and a query can land on
// it however old the others are, so one such node is enough. A complete pass
// with every node on an older build clears it; a pass that could not reach
// every node and saw no mandatory-analyzer build leaves the decision in force,
// because the node it missed may be the upgraded one.
func analyzerMandatoryOnFleet(fv chclient.FleetVersions, current bool) bool {
	if slices.ContainsFunc(fv.Versions, chopt.AnalyzerMandatory) {
		return true
	}
	if !fv.Complete {
		return current
	}
	return false
}
