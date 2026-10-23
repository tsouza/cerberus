package main

import (
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// legacyAnalyzerAllowed decides whether queries may still disable the
// analyzer (chopt.EnabledSet.LegacyAnalyzer), given what the fleet probe saw
// and the decision in force. It is conservative across nodes in the opposite
// direction to feature resolution: a reached node on a build where the
// analyzer is mandatory refuses enable_analyzer=0, and a query can land on it
// however old the others are, so one such node withholds the stamp. It is
// granted only when every known node answered from an older build; a pass
// that could not reach every node and saw no mandatory-analyzer build leaves
// the decision unchanged.
func legacyAnalyzerAllowed(fv chclient.FleetVersions, current bool) bool {
	for _, v := range fv.Versions {
		if chopt.AnalyzerMandatory(v) {
			return false
		}
	}
	if !fv.Complete {
		return current
	}
	return true
}
