package chsql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// queryExemplarsFixtureDir holds the Layer-2a TXTAR snapshot(s) for
// the Prom /api/v1/query_exemplars emitter. The fixture lives under
// test/spec/promql/exemplars_*.txtar — the same dir the promql lower
// fixtures live in — because PR B's handler treats it as a promql-head
// endpoint. The promql/ runner (internal/promql/lower_test.go) tests
// `query.promql`-bearing fixtures; this harness pairs with the
// exemplar fixtures by filename prefix so the two layers coexist on a
// shared directory.
var queryExemplarsFixtureDir = filepath.Join("..", "..", "test", "spec", "promql")

// queryExemplarsCases maps fixture base name → the emitter input
// shape. Mirrors the pattern in emit_test.go's plans map — adding a
// fixture is:
//  1. Create the .txtar with at least an empty `-- sql --` section.
//  2. Register an entry here.
//  3. Run `GOLDEN_UPDATE=1 just test-spec` to materialise the SQL.
//  4. Review the diff and commit both files.
//
// The handler-side scenarios (multi-matcher predicate, large window,
// summary short-circuit) live in PR B's handler tests; this layer-2a
// harness asserts only the SQL shape the emitter produces.
var queryExemplarsCases = map[string]struct {
	table     string
	predicate chsql.Frag
	start     time.Time
	end       time.Time
	schema    schema.Metrics
}{
	"exemplars_basic": {
		table: schema.DefaultOTelMetrics().SumTable,
		predicate: chsql.Eq(
			chsql.Col(schema.DefaultOTelMetrics().MetricNameColumn),
			chsql.Lit("http_request_duration_seconds"),
		),
		start:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		end:    time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
		schema: schema.DefaultOTelMetrics(),
	},
}

// TestEmitQueryExemplars_Fixtures runs the registered cases against
// the TXTAR snapshots. Walks every `exemplars_*.txtar` under
// test/spec/promql/ and asserts the emitted SQL + args match.
//
// The walker filters by filename prefix (not by section presence)
// because cerberus's CI rejects t.Skip in test code (the forbid-skip
// GA gate). The promql/ directory hosts both the lower-pipeline
// fixtures (consumed by internal/promql/lower_test.go) and the
// exemplars layer-2a fixtures (consumed here); the prefix-filter
// keeps each harness scoped without spurious case discovery.
func TestEmitQueryExemplars_Fixtures(t *testing.T) {
	t.Parallel()

	matches, err := filepath.Glob(filepath.Join(queryExemplarsFixtureDir, "exemplars_*.txtar"))
	if err != nil {
		t.Fatalf("glob %s: %v", queryExemplarsFixtureDir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no exemplars_*.txtar fixtures in %s", queryExemplarsFixtureDir)
	}
	sort.Strings(matches)
	for _, m := range matches {
		c, err := spec.Load(m)
		if err != nil {
			t.Fatalf("load %s: %v", m, err)
		}
		t.Run(c.Name, func(t *testing.T) {
			tc, ok := queryExemplarsCases[c.Name]
			if !ok {
				t.Fatalf("no case registered for fixture %s; add it to queryExemplarsCases in query_exemplars_spec_test.go", c.Name)
			}

			sql, args, err := chsql.EmitQueryExemplars(
				context.Background(),
				tc.table,
				tc.predicate,
				tc.start,
				tc.end,
				tc.schema,
			)
			if err != nil {
				t.Fatalf("EmitQueryExemplars: %v", err)
			}

			spec.Match(t, c, map[string]string{
				"sql":  sql,
				"args": formatQueryExemplarsArgs(args),
			})
		})
	}
}

// formatQueryExemplarsArgs mirrors the formatArgs helper in
// emit_test.go. Duplicated rather than exported so the exemplars suite
// stays free to change its formatting independently of the main emit
// suite.
func formatQueryExemplarsArgs(args []any) string {
	if len(args) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, a := range args {
		fmt.Fprintf(&b, "[%d] %T = %#v\n", i, a, a)
	}
	return b.String()
}
