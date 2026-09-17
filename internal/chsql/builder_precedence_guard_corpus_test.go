package chsql_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
	"github.com/tsouza/cerberus/test/spec"
)

// precedenceCorpusFloor is the fewest plans the sweep must have rendered
// under the guard for its green to mean anything: the chsql emit suite plus
// the three heads' corpora hold well over a thousand fixtures, so a sweep
// that rendered fewer than this lost most of them to a harness change.
const precedenceCorpusFloor = 1000

// harnessRangeStart / harnessRangeEnd / harnessInstantEval mirror the
// deterministic windows the heads' own TestLower harnesses lower with, so
// every fixture lowers here exactly as it does there.
var (
	harnessRangeStart  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	harnessRangeEnd    = harnessRangeStart.Add(5 * time.Minute)
	harnessInstantEval = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
)

// TestPrecedenceGuard_EmitSuites renders every plan the chsql emit suite
// registers and every fixture of the PromQL, LogQL and TraceQL corpora
// with the render-time precedence guard installed, and fails on any
// operand ClickHouse would re-associate. The guard is what makes the
// class checkable at all: a Frag built at one site and handed through a
// parameter into an operator at another carries its precedence mistake
// past both the caller and any reader of the rendered SQL.
func TestPrecedenceGuard_EmitSuites(t *testing.T) {
	report := chsql.InstallPrecedenceGuardForTest()
	rendered := 0
	emit := func(t *testing.T, plan chplan.Node) {
		t.Helper()
		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		rendered++
	}

	for name, plan := range plans {
		t.Run("chsql/"+name, func(t *testing.T) { emit(t, plan) })
	}

	metrics := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	spec.Walk(t, filepath.Join("..", "..", "test", "spec", "promql"), func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.promql")
		if !ok {
			return
		}
		query = strings.TrimSpace(query)
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		var plan chplan.Node
		switch {
		case hasSection(c, "metadata_label_values"):
			label, _ := c.Section("metadata_label_values")
			plan, err = promql.LowerMetadataLabelValues(context.Background(), expr, metrics, harnessRangeStart, harnessRangeEnd, strings.TrimSpace(label))
		case hasSection(c, "metadata_label_names"):
			plan, err = promql.LowerMetadataLabelNames(context.Background(), expr, metrics, harnessRangeStart, harnessRangeEnd)
		case strings.Contains(query, "@ start()") || strings.Contains(query, "@ end()"):
			plan, err = promql.LowerAt(context.Background(), expr, metrics, harnessRangeStart, harnessRangeEnd)
		case hasSection(c, "range_step"):
			rs, _ := c.Section("range_step")
			step, perr := time.ParseDuration(strings.TrimSpace(rs))
			if perr != nil {
				t.Fatalf("range_step: %v", perr)
			}
			plan, err = promql.LowerAtRange(context.Background(), expr, metrics, harnessRangeStart, harnessRangeEnd, step)
		default:
			plan, err = promql.LowerAt(context.Background(), expr, metrics, harnessInstantEval, harnessInstantEval)
		}
		if err != nil {
			t.Fatalf("lower %q: %v", query, err)
		}
		emit(t, plan)
	})

	logs := schema.DefaultOTelLogs()
	spec.Walk(t, filepath.Join("..", "..", "test", "spec", "logql"), func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.logql")
		if !ok {
			return
		}
		expr, err := logql.ParseExprPermissive(strings.TrimSpace(query))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		start := sectionTime(t, c, "start")
		end := sectionTime(t, c, "end")
		step := sectionDuration(t, c, "step")
		var plan chplan.Node
		switch {
		case start.IsZero() && end.IsZero():
			plan, err = logql.Lower(context.Background(), expr, logs)
		case step > 0:
			plan, err = logql.LowerAtRange(context.Background(), expr, logs, start, end, step)
		default:
			plan, err = logql.LowerAt(context.Background(), expr, logs, start, end)
		}
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		emit(t, plan)
	})

	traces := schema.DefaultOTelTraces()
	spec.Walk(t, filepath.Join("..", "..", "test", "spec", "traceql"), func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.traceql")
		if !ok {
			return
		}
		expr, err := ast.Parse(strings.TrimSpace(query))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ctx := context.Background()
		if v, ok := c.Section("search_limit"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("search_limit: %v", err)
			}
			ctx = traceql.WithSearchTraceLimit(ctx, n)
		}
		if v, ok := c.Section("search_window"); ok {
			fields := strings.Fields(v)
			if len(fields) != 2 {
				t.Fatalf("search_window wants two bounds, got %q", v)
			}
			startSec, err1 := strconv.ParseInt(fields[0], 10, 64)
			endSec, err2 := strconv.ParseInt(fields[1], 10, 64)
			if err1 != nil || err2 != nil {
				t.Fatalf("search_window: %q", v)
			}
			ctx = traceql.WithSearchWindow(ctx, time.Unix(startSec, 0).UTC(), time.Unix(endSec, 0).UTC())
		}
		plan, err := traceql.Lower(ctx, expr, traces)
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		emit(t, plan)
	})

	violations := report()
	if rendered < precedenceCorpusFloor {
		t.Fatalf("the sweep rendered %d plans, fewer than the %d it must cover to be evidence", rendered, precedenceCorpusFloor)
	}
	if len(violations) > 0 {
		t.Fatalf("%d operand(s) ClickHouse would re-associate:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func hasSection(c *spec.Case, name string) bool {
	_, ok := c.Section(name)
	return ok
}

func sectionTime(t *testing.T, c *spec.Case, name string) time.Time {
	t.Helper()
	v, ok := c.Section(name)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return ts
}

func sectionDuration(t *testing.T, c *spec.Case, name string) time.Duration {
	t.Helper()
	v, ok := c.Section(name)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}
