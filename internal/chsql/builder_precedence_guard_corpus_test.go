package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestPrecedenceGuard_EmitSuite renders every plan the chsql emit suite
// registers with the render-time precedence guard installed and fails on
// any operand ClickHouse would re-associate. The guard is what makes the
// class checkable at all: a Frag built at one site and handed through a
// parameter into an operator at another carries its precedence mistake
// past both the caller and any reader of the rendered SQL.
//
// The three heads' fixture corpora are swept by their own TestLower
// harnesses (internal/{promql,logql,traceql}/lower_test.go), which install
// the same guard around a walk that also holds every rendered statement to
// its golden. Rendering those corpora here as well would execute most of
// the emitter under a test that asserts nothing about the SQL — coverage
// gremlins would then count as attempted and a truncating mutant would
// survive under — so this sweep stays with the plans this package owns.
func TestPrecedenceGuard_EmitSuite(t *testing.T) {
	report := chsql.InstallPrecedenceGuard()
	rendered := 0
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			rendered++
		})
	}
	if rendered != len(plans) {
		t.Fatalf("the sweep rendered %d of the %d registered plans", rendered, len(plans))
	}
	if violations := report(); len(violations) > 0 {
		t.Fatalf("%d operand(s) ClickHouse would re-associate:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
