package logql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "logql")

// TestLower walks every *.txtar fixture under test/spec/logql/, parses
// the LogQL in `query.logql`, lowers it, emits SQL, and compares the
// result to the recorded `sql` + `args` sections.
func TestLower(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.logql")
		if !ok {
			t.Fatalf("fixture %s missing 'query.logql' section", c.Name)
		}
		query = strings.TrimSpace(query)

		expr, err := syntax.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := logql.Lower(context.Background(), expr, s)
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
