package logql_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

func mustProjectSamples(t testing.TB, lang *logql.Lang, plan chplan.Node, meta engine.Meta) chplan.Node {
	t.Helper()
	projected, err := lang.ProjectSamples(plan, meta)
	if err != nil {
		t.Fatalf("ProjectSamples: %v", err)
	}
	return projected
}

// mustLowerLog lowers the log query expr over s, the plan a log query's
// ProjectSamples receives.
func mustLowerLog(t testing.TB, expr syntax.Expr, s schema.Logs) chplan.Node {
	t.Helper()
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%s): %v", expr, err)
	}
	return plan
}
