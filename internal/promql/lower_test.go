package promql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
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
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// Fixed start/end used when a fixture's query contains `@ start()`
	// or `@ end()`. Picking small Unix-epoch values keeps the generated
	// SQL literals short and deterministic. Detected by string match;
	// fixtures without those modifiers use plain Lower (no range).
	const (
		fixtureStartUnix = int64(100)
		fixtureEndUnix   = int64(500)
	)
	start := time.Unix(fixtureStartUnix, 0).UTC()
	end := time.Unix(fixtureEndUnix, 0).UTC()

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
		// Fixtures that exercise `@ start()` / `@ end()` need the
		// time-aware lowering entrypoint; everything else uses plain
		// Lower so existing fixtures stay deterministic regardless of
		// the fixed range.
		var plan chplan.Node
		if strings.Contains(query, "@ start()") || strings.Contains(query, "@ end()") {
			plan, err = promql.LowerAt(context.Background(), expr, s, start, end)
		} else {
			plan, err = promql.Lower(context.Background(), expr, s)
		}
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
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
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "topk changes output shape",
			query:   `topk(5, up)`,
			wantErr: "changes output shape and lands with M1.7",
		},
		// Note: 'without' now lowers (M1.4); BinaryExpr vector+vector
		// rejection moved to binary_test.go; arithmetic ops are lowered
		// for the scalar-vector case (M1.2).
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			if _, err := promql.Lower(context.Background(), expr, s); err == nil {
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
