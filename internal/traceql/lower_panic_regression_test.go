package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLower_ParseableInputsNeverPanic pins inputs that the reference
// TraceQL parser accepts but that once crashed cerberus's lowering.
// Each entry must lower to either a plan or an error — a panic fails
// the test via the runtime.
//
// `{}|0>0` is the one bug the retired weekly fuzz lane found in its
// lifetime (#324: non-aggregate scalar-filter LHS hit an unchecked
// type assertion). The lane is gone; the regression pin stays. Add
// future parses-but-crashed inputs here.
func TestLower_ParseableInputsNeverPanic(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	inputs := []string{
		`{}|0>0`, // #324
	}
	for _, q := range inputs {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(q)
			if err != nil {
				t.Fatalf("input no longer parses upstream: %v", err)
			}
			_, _ = traceql.Lower(context.Background(), expr, s)
		})
	}
}
