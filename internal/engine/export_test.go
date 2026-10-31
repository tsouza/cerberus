package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// SharedQuerySettingsForTest returns the per-query settings the dispatch
// seam stamps on plan: applySharedQuerySettings over one inspection of plan,
// with r's flags and testQueryMemoryCap, so the external tests that lower
// real queries (the heads import engine, so only package engine_test can
// import them) see every rule the seam composes, not only SettingsRules'
// flag-driven ones.
func SharedQuerySettingsForTest(r SettingsRules, plan chplan.Node) map[string]any {
	f := inspectPlanShape(plan, r.Traces.TraceIDColumn)
	return chclient.QuerySettingsFromContext(applySharedQuerySettings(context.Background(), f, testQueryMemoryCap, r))
}
