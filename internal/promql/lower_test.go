package promql_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "promql")

// TestLower walks every *.txtar fixture under test/spec/promql/, parses the
// PromQL in `query.promql`, lowers it to a chplan, emits SQL, and asserts
// the `sql` + `args` sections match what's recorded.
//
// To add a new case: create test/spec/promql/<name>.txtar with a
// `-- query.promql --` section, then run `just update-golden`.
func TestLower(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.promql")
		if !ok {
			t.Fatalf("fixture %s missing 'query.promql' section", c.Name)
		}
		query = strings.TrimSpace(query)

		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.Lower(expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		sql, args, err := chsql.Emit(plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"sql":  sql,
			"args": formatArgs(args),
		})
	})
}

// TestLower_errors covers the unsupported-PromQL paths so regressions in
// the error message remain visible.
func TestLower_errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "without aggregation rejected",
			query:   `sum without (instance) (up)`,
			wantErr: "'without' aggregation is not yet supported",
		},
		{
			name:    "binary op rejected",
			query:   `up + up`,
			wantErr: "unsupported expression",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			if _, err := promql.Lower(expr, s); err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
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
