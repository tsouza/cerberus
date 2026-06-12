// Static compute-fan-out tripwire (Phase-1 perf assessment, Component D).
//
// Every perf bug the assessment catalogued was a COMPUTE FAN-OUT — a
// step-grid CROSS JOIN, an arrayJoin explosion feeding a JOIN, or an
// unbounded WITH RECURSIVE closure. internal/perf/fanout.Lint flags those
// shapes statically (zero data). This meta-test wires the linter into the
// always-on (non-chDB) check gate: it lowers every test/spec/** fixture,
// runs the linter on the lowered plan + emitted SQL, and fails if any
// unbounded fan-out shape survives.
//
// It MUST stay green on main — #804 / #805 (range_lwr / histogram
// step-grid) and #808 / #809 (recursive depth caps) fixed every known
// shape. A future PR that reintroduces an unbounded fan-out will trip
// here at review time, before it can ship.
//
// Guards: range_lwr / histogram step-grid blowup (#804 / #805),
// nested-set / structural recursion without a depth cap (#808 / #809),
// and the latent arrayJoin-into-JOIN / per-row correlated-subquery
// classes the assessment flagged.
package regression

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/perf/fanout"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
)

// specDir resolves test/spec/<head> relative to this test file's package
// dir (test/regression).
func specDir(head string) string {
	return filepath.Join("..", "spec", head)
}

// TestFanoutLint_LoweredPlans re-lowers every PromQL / LogQL / TraceQL
// fixture and asserts the lowered plan + emitted SQL carry no unbounded
// compute-fan-out shape. This is the IR-level half of the tripwire (it
// reaches chplan.CrossJoin / fan-out-into-JOIN / correlated-subquery
// shapes that are only visible on the plan tree).
func TestFanoutLint_LoweredPlans(t *testing.T) {
	t.Parallel()

	for _, head := range []string{"promql", "logql", "traceql"} {
		head := head
		t.Run(head, func(t *testing.T) {
			t.Parallel()
			lowerHead(t, head, func(t *testing.T, name string, plan chplan.Node, sql string) {
				if vs := fanout.Lint(plan, sql); len(vs) > 0 {
					reportViolations(t, head, name, vs)
				}
			})
		})
	}
}

// TestFanoutLint_EmittedSQL scans the committed `-- sql --` section of
// EVERY fixture (including the test/spec/chsql/ fixtures that no head
// lowers) for SQL-only fan-out shapes — today, an uncapped WITH
// RECURSIVE. This is the SQL-level half of the tripwire and guarantees
// the recursive-cap rule covers the structural-join emitter's own
// golden fixtures, not just the re-lowerable heads.
func TestFanoutLint_EmittedSQL(t *testing.T) {
	t.Parallel()

	for _, head := range []string{"promql", "logql", "traceql", "chsql"} {
		head := head
		t.Run(head, func(t *testing.T) {
			t.Parallel()
			dir := specDir(head)
			matches, err := filepath.Glob(filepath.Join(dir, "*.txtar"))
			if err != nil {
				t.Fatalf("glob %s: %v", dir, err)
			}
			if len(matches) == 0 {
				t.Fatalf("no fixtures under %s", dir)
			}
			sort.Strings(matches)
			for _, m := range matches {
				c, err := spec.Load(m)
				if err != nil {
					t.Fatalf("load %s: %v", m, err)
				}
				sql, ok := c.Section("sql")
				if !ok {
					continue
				}
				// nil plan: only the SQL-keyed rules fire here.
				if vs := fanout.Lint(nil, sql); len(vs) > 0 {
					reportViolations(t, head, c.Name, vs)
				}
			}
		})
	}
}

// reportViolations fails the test with a loud, actionable message. A
// violation is a genuine unbounded-fan-out finding — never suppress or
// allow-list it; fix the lowering / emitter at the source.
func reportViolations(t *testing.T, head, name string, vs []fanout.Violation) {
	t.Helper()
	var b strings.Builder
	for _, v := range vs {
		b.WriteString("\n  ")
		b.WriteString(v.String())
	}
	t.Errorf("unbounded compute fan-out in %s fixture %q:%s\n"+
		"This is a real perf regression — a future query of this shape blows up intermediate cardinality.\n"+
		"Fix the lowering/emitter at the source (collapse the fan-out / cap the recursion); do NOT allow-list it.",
		head, name, b.String())
}

// ---------------------------------------------------------------------
// per-head re-lowering (mirrors internal/{promql,logql,traceql}/lower_test)
// ---------------------------------------------------------------------

func lowerHead(t *testing.T, head string, fn func(t *testing.T, name string, plan chplan.Node, sql string)) {
	t.Helper()
	dir := specDir(head)
	matches, err := filepath.Glob(filepath.Join(dir, "*.txtar"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no fixtures under %s", dir)
	}
	sort.Strings(matches)

	ctx := context.Background()
	for _, m := range matches {
		c, err := spec.Load(m)
		if err != nil {
			t.Fatalf("load %s: %v", m, err)
		}
		plan, ok := lowerFixture(t, head, c)
		if !ok {
			continue
		}
		sql, _, err := chsql.Emit(ctx, plan)
		if err != nil {
			t.Fatalf("[%s] emit %s: %v", head, c.Name, err)
		}
		fn(t, c.Name, plan, sql)
	}
}

// lowerFixture lowers a single fixture for its head, returning (plan,
// true) or (nil, false) when the fixture is not head-lowerable (e.g. the
// PromQL exemplars_ fixtures, which the chsql exemplars harness owns).
func lowerFixture(t *testing.T, head string, c *spec.Case) (chplan.Node, bool) {
	t.Helper()
	ctx := context.Background()
	switch head {
	case "promql":
		return lowerPromQL(t, ctx, c)
	case "logql":
		return lowerLogQL(t, ctx, c)
	case "traceql":
		return lowerTraceQL(t, ctx, c)
	}
	return nil, false
}

func lowerPromQL(t *testing.T, ctx context.Context, c *spec.Case) (chplan.Node, bool) {
	t.Helper()
	// exemplars_ fixtures do not flow through promql.Lower (owned by the
	// chsql exemplars harness) — skip, mirroring internal/promql lower_test.
	if strings.HasPrefix(c.Name, "exemplars_") {
		return nil, false
	}
	query, ok := c.Section("query.promql")
	if !ok {
		t.Fatalf("promql fixture %s missing query.promql", c.Name)
	}
	query = strings.TrimSpace(query)

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("promql ParseExpr(%q): %v", query, err)
	}

	// Same deterministic anchors the promql lower_test harness uses.
	start := time.Unix(100, 0).UTC()
	end := time.Unix(500, 0).UTC()
	instantEval := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)

	var plan chplan.Node
	switch {
	case strings.Contains(query, "@ start()") || strings.Contains(query, "@ end()"):
		plan, err = promql.LowerAt(ctx, expr, s, start, end)
	default:
		if rs, ok := c.Section("range_step"); ok {
			stepDur, perr := time.ParseDuration(strings.TrimSpace(rs))
			if perr != nil {
				t.Fatalf("promql fixture %s: parse range_step %q: %v", c.Name, rs, perr)
			}
			plan, err = promql.LowerAtRange(ctx, expr, s, rangeStart, rangeEnd, stepDur)
		} else {
			plan, err = promql.LowerAt(ctx, expr, s, instantEval, instantEval)
		}
	}
	if err != nil {
		t.Fatalf("promql Lower(%q): %v", query, err)
	}
	return plan, true
}

func lowerLogQL(t *testing.T, ctx context.Context, c *spec.Case) (chplan.Node, bool) {
	t.Helper()
	query, ok := c.Section("query.logql")
	if !ok {
		t.Fatalf("logql fixture %s missing query.logql", c.Name)
	}
	query = strings.TrimSpace(query)

	s := schema.DefaultOTelLogs()
	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("logql ParseExprPermissive(%q): %v", query, err)
	}

	start := parseTimeSection(t, c, "start")
	end := parseTimeSection(t, c, "end")
	step := parseDurationSection(t, c, "step")

	var plan chplan.Node
	switch {
	case start.IsZero() && end.IsZero():
		plan, err = logql.Lower(ctx, expr, s)
	case step > 0:
		plan, err = logql.LowerAtRange(ctx, expr, s, start, end, step)
	default:
		plan, err = logql.LowerAt(ctx, expr, s, start, end)
	}
	if err != nil {
		t.Fatalf("logql Lower(%q): %v", query, err)
	}
	return plan, true
}

func lowerTraceQL(t *testing.T, ctx context.Context, c *spec.Case) (chplan.Node, bool) {
	t.Helper()
	query, ok := c.Section("query.traceql")
	if !ok {
		t.Fatalf("traceql fixture %s missing query.traceql", c.Name)
	}
	query = strings.TrimSpace(query)

	s := schema.DefaultOTelTraces()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("traceql Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("traceql Lower(%q): %v", query, err)
	}
	return plan, true
}

func parseTimeSection(t *testing.T, c *spec.Case, name string) time.Time {
	t.Helper()
	v, ok := c.Section(name)
	if !ok {
		return time.Time{}
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t.Fatalf("fixture %s: parse %s %q: %v", c.Name, name, v, err)
	}
	return ts
}

func parseDurationSection(t *testing.T, c *spec.Case, name string) time.Duration {
	t.Helper()
	v, ok := c.Section(name)
	if !ok {
		return 0
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("fixture %s: parse %s %q: %v", c.Name, name, v, err)
	}
	return d
}
