package traceql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "traceql")

// TestLower walks every *.txtar fixture under test/spec/traceql/, parses
// the TraceQL in `query.traceql`, lowers it, emits SQL, and compares
// the result to the recorded `sql` + `args` sections.
func TestLower(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.traceql")
		if !ok {
			t.Fatalf("fixture %s missing 'query.traceql' section", c.Name)
		}
		query = strings.TrimSpace(query)

		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"sql":  sqlStr,
			"args": formatArgs(args),
		})
	})
}

func formatArgs(args []any) string {
	if len(args) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, a := range args {
		fmt.Fprintf(&b, "[%d] %T = %#v\n", i, a, a)
	}
	return b.String()
}
