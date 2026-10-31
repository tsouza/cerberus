package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// SharedQuerySettingsForTest returns the per-query settings the dispatch
// seam's SettingsRules stamp on plan, for the external tests that lower real
// queries (the heads import engine, so only package engine_test can import
// them).
func SharedQuerySettingsForTest(r SettingsRules, plan chplan.Node) map[string]any {
	return chclient.QuerySettingsFromContext(r.apply(context.Background(), plan))
}
