package logql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "logql")

// TestLower walks every *.txtar fixture under test/spec/logql/, parses
// the LogQL in `query.logql`, lowers it, emits SQL, and compares the
// result to the recorded `sql` + `args` sections.
//
// Fixtures may optionally declare `start:` / `end:` / `step:` sections.
// `start:` + `end:` (both RFC3339Nano) thread a [start, end] window
// through [logql.LowerAt], so the emitted SQL carries a Timestamp
// BETWEEN predicate above every Scan(LogsTable). Adding a `step:`
// duration (e.g. `1m`) lifts the lowering to [logql.LowerAtRange] so
// range-aggregation lowerings switch to the matrix RangeWindow shape
// (one row per anchor across [start, end] spaced by step). Fixtures
// without any of these sections lower via [logql.Lower] (no time
// window) so the existing fixture corpus remains stable.
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

		start, end, step, err := readWindowSections(c)
		if err != nil {
			t.Fatalf("window sections: %v", err)
		}

		var plan chplan.Node
		switch {
		case start.IsZero() && end.IsZero():
			plan, err = logql.Lower(context.Background(), expr, s)
		case step > 0:
			plan, err = logql.LowerAtRange(context.Background(), expr, s, start, end, step)
		default:
			plan, err = logql.LowerAt(context.Background(), expr, s, start, end)
		}
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

// readWindowSections pulls optional `start:` / `end:` (RFC3339Nano) and
// `step:` (Go duration) sections from c. Missing or empty sections
// return zero values so the caller falls back to the no-window Lower
// path (or the no-step LowerAt path when only start/end are set).
func readWindowSections(c *spec.Case) (time.Time, time.Time, time.Duration, error) {
	var (
		start, end time.Time
		step       time.Duration
	)
	if v, ok := c.Section("start"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return time.Time{}, time.Time{}, 0, fmt.Errorf("start: %w", err)
			}
			start = t
		}
	}
	if v, ok := c.Section("end"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return time.Time{}, time.Time{}, 0, fmt.Errorf("end: %w", err)
			}
			end = t
		}
	}
	if v, ok := c.Section("step"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return time.Time{}, time.Time{}, 0, fmt.Errorf("step: %w", err)
			}
			step = d
		}
	}
	return start, end, step, nil
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
