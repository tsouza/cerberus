package chopttest

import (
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/promql"
)

// This file carries no build tag on purpose: BuildRangeLowerers depends only
// on chopt and promql, and the untagged unit lane's solver-decision ratchet
// (test/perf/solver_decision_ratchet_test.go) consumes it. The rest of this
// package stays behind the integration tag because it needs a live
// ClickHouse.

// BuildRangeLowerers builds the FULL promql.RangeLowerers dispatch table from
// set. It is the SAME function cmd/cerberus's boot path calls — both delegate
// to internal/choptwire.Build — so a real-ClickHouse activation test wired
// through here is exercising the production table, not a copy of it.
//
// It used to be a reviewed duplicate of cmd/cerberus's own unexported
// nativeRangeLowerers, on the stated grounds that an unexported `package main`
// function cannot be imported. That was true of the function and false of the
// formula: it depends only on chopt and promql and so always belonged in an
// importable package. Every real-CH activation lane therefore validated the
// copy while the binary booted the original (cerberus issue #3186).
func BuildRangeLowerers(set chopt.EnabledSet) promql.RangeLowerers {
	return choptwire.RangeLowerers(set)
}
