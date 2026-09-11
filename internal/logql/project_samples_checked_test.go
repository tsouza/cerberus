package logql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
)

func mustProjectSamples(t testing.TB, lang *logql.Lang, plan chplan.Node, meta engine.Meta) chplan.Node {
	t.Helper()
	projected, err := lang.ProjectSamples(plan, meta)
	if err != nil {
		t.Fatalf("ProjectSamples: %v", err)
	}
	return projected
}
