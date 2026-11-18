package main

import (
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// prewhereInWhereOrderOnFleet decides whether every server the heads read
// runs a multi-step PREWHERE in the order of the WHERE conjuncts
// (chopt.EnabledSet.PrewhereInWhereOrder), given what the fleet probe saw. fv
// is the remembered fleet (rememberingFleetProber). Only a complete pass in
// which every node runs such a build sets it: a query can land on any node,
// and a node that reorders the conditions reads no less for the order the
// plan writes them in. A pass that read no version, or missed a node, leaves
// it unset, which keeps the plans a reordering server is read with.
func prewhereInWhereOrderOnFleet(fv chclient.FleetVersions) bool {
	if !fv.Complete || len(fv.Versions) == 0 {
		return false
	}
	for _, v := range fv.Versions {
		if !chopt.PrewhereKeepsWhereOrder(v) {
			return false
		}
	}
	return true
}
