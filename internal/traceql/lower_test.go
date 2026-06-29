package traceql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

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
		// Optional `search_limit:` section threads /api/search's response
		// trace limit into lowering (via the same ctx key the handler
		// uses), so a fixture can pin the bounded nested-set numbering
		// shape (#103). Absent ⇒ unbounded, today's behaviour.
		ctx := context.Background()
		if v, ok := c.Section("search_limit"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("fixture %s: bad search_limit %q: %v", c.Name, v, err)
			}
			ctx = traceql.WithSearchTraceLimit(ctx, n)
		}
		// Optional `search_window:` section threads /api/search's request
		// [start, end] window (two whitespace-separated Unix-second bounds)
		// into lowering, so a fixture can pin the windowed compound/structural
		// search shape (#1109 GAP-3). Absent ⇒ no window, today's behaviour.
		if v, ok := c.Section("search_window"); ok {
			fields := strings.Fields(v)
			if len(fields) != 2 {
				t.Fatalf("fixture %s: search_window wants two Unix-second bounds, got %q", c.Name, v)
			}
			startSec, err1 := strconv.ParseInt(fields[0], 10, 64)
			endSec, err2 := strconv.ParseInt(fields[1], 10, 64)
			if err1 != nil || err2 != nil {
				t.Fatalf("fixture %s: bad search_window %q", c.Name, v)
			}
			ctx = traceql.WithSearchWindow(ctx, time.Unix(startSec, 0).UTC(), time.Unix(endSec, 0).UTC())
		}
		plan, err := traceql.Lower(ctx, expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"sql":    sqlStr,
			"args":   formatArgs(args),
			"chplan": spec.PrintChplan(plan),
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
