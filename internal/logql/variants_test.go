package logql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerMultiVariant pins the LogQL `variants(...) of (...)` lowering.
//
// Arms that read the SAME log stream FUSE into one plan: a single Project
// over a RangeWindow carrying one chplan.RangeWindowVariant per arm, which
// reads the table once and stamps each row's arm into the `__variant__`
// column (issue #1501). Arms that read DIFFERENT streams keep the per-arm
// shape — each lowered independently, re-shaped into the canonical Sample
// contract, tagged with a literal `__variant__="<index>"` folded into its
// Attributes map, and concatenated with a UnionAll. Either way the labelling
// mirrors reference Loki's constants.VariantLabel (one tag per arm index).
func TestLowerMultiVariant(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()

	tests := []struct {
		name      string
		query     string
		wantArms  int
		wantTags  []string // `__variant__` value expected per arm, in order
		wantFused bool
	}{
		{
			// A lone arm has nothing to share a pass with.
			name:     "single variant",
			query:    `variants(count_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantArms: 1,
			wantTags: []string{"0"},
		},
		{
			name:      "two variants count + bytes",
			query:     `variants(count_over_time({app="foo"}[5m]), bytes_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantArms:  2,
			wantTags:  []string{"0", "1"},
			wantFused: true,
		},
		{
			name:      "grouped variants",
			query:     `variants(sum by (app) (count_over_time({app="foo"}[5m])), sum by (app) (bytes_over_time({app="foo"}[5m]))) of ({app="foo"}[5m])`,
			wantArms:  2,
			wantTags:  []string{"0", "1"},
			wantFused: true,
		},
		{
			// Both reducers read the same unwrapped value. They must share
			// one value slot rather than falling back to two table scans.
			name:      "shared value expression",
			query:     `variants(max_over_time({app="foo"} | unwrap latency [5m]), min_over_time({app="foo"} | unwrap latency [5m])) of ({app="foo"}[5m])`,
			wantArms:  2,
			wantTags:  []string{"0", "1"},
			wantFused: true,
		},
		{
			// The `of (...)` arm is a hint, not a constraint: the parser
			// accepts arms whose selectors differ from it and from each
			// other, and those genuinely read two streams.
			name:     "divergent selectors keep the per-arm shape",
			query:    `variants(count_over_time({app="foo"}[5m]), count_over_time({app="bar"}[5m])) of ({app="foo"}[5m])`,
			wantArms: 2,
			wantTags: []string{"0", "1"},
		},
		{
			// One arm aggregates and the other does not, so no single
			// pipeline can serve both.
			name:     "mixed pipelines keep the per-arm shape",
			query:    `variants(sum by (app) (count_over_time({app="foo"}[5m])), count_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantArms: 2,
			wantTags: []string{"0", "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr, err := logql.ParseExprPermissive(tt.query)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v", tt.query, err)
			}
			plan, err := logql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tt.query, err)
			}

			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// The point of fusing: one read of the log table however many
			// arms the query carries. The per-arm shape reads it once each.
			wantScans := tt.wantArms
			if tt.wantFused {
				wantScans = 1
			}
			if got := strings.Count(sqlStr, "`otel_logs`"); got != wantScans {
				t.Errorf("log table read %d times; want %d", got, wantScans)
			}
			for _, tag := range tt.wantTags {
				if !argsContain(args, "__variant__") || !argsContain(args, tag) {
					t.Errorf("args missing __variant__ tag %q: %v", tag, args)
				}
			}

			if tt.wantFused {
				proj, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("top node = %T; want *chplan.Project over a fused window", plan)
				}
				// The fused plan stamps the arm from a COLUMN, not a literal:
				// one row per (series, anchor, arm) comes out of one pass.
				if !attrsCarriesVariantColumn(projectionExpr(t, proj, "Attributes")) {
					t.Error("fused Attributes expr does not fold the __variant__ column")
				}
				if !strings.Contains(sqlStr, "ARRAY JOIN") {
					t.Error("fused SQL carries no ARRAY JOIN unpivot")
				}
				return
			}

			u, ok := plan.(*chplan.UnionAll)
			if !ok {
				t.Fatalf("top node = %T; want *chplan.UnionAll", plan)
			}
			if len(u.Inputs) != tt.wantArms {
				t.Fatalf("UnionAll arms = %d; want %d", len(u.Inputs), tt.wantArms)
			}
			// Each arm must be a top-level Project aliasing the canonical
			// Sample columns, with its own literal `__variant__` tag folded
			// into Attributes.
			for i, arm := range u.Inputs {
				proj, ok := arm.(*chplan.Project)
				if !ok {
					t.Fatalf("arm %d = %T; want *chplan.Project", i, arm)
				}
				if !attrsCarriesVariantTag(projectionExpr(t, proj, "Attributes"), tt.wantTags[i]) {
					t.Errorf("arm %d Attributes expr does not fold __variant__=%q", i, tt.wantTags[i])
				}
			}
			if tt.wantArms > 1 && !strings.Contains(sqlStr, "UNION ALL") {
				t.Errorf("multi-arm SQL missing UNION ALL: %s", sqlStr)
			}
		})
	}
}

// projectionExpr returns the expression p projects under alias, failing the
// test when there is none.
func projectionExpr(t *testing.T, p *chplan.Project, alias string) chplan.Expr {
	t.Helper()
	for _, proj := range p.Projections {
		if proj.Alias == alias {
			return proj.Expr
		}
	}
	t.Fatalf("Project has no %q projection", alias)
	return nil
}

// attrsCarriesVariantColumn reports whether expr is the
// `mapConcat(<attrs>, map("__variant__", __variant__))` shape the FUSED
// lowering builds — the arm identity read from a column rather than baked in
// as a literal.
func attrsCarriesVariantColumn(expr chplan.Expr) bool {
	inner, ok := variantMapArg(expr)
	if !ok {
		return false
	}
	col, ok := inner.Args[1].(*chplan.ColumnRef)
	return ok && col.Name == "__variant__"
}

// attrsCarriesVariantTag reports whether expr is the
// `mapConcat(<attrs>, map("__variant__", "<tag>"))` shape the PER-ARM
// variant lowering builds.
func attrsCarriesVariantTag(expr chplan.Expr, tag string) bool {
	inner, ok := variantMapArg(expr)
	if !ok {
		return false
	}
	val, ok := inner.Args[1].(*chplan.LitString)
	return ok && val.V == tag
}

// variantMapArg peels `mapConcat(<attrs>, map("__variant__", <x>))` down to
// the inner two-argument `map(...)` call, which both variant shapes build and
// differ only in the second argument of.
func variantMapArg(expr chplan.Expr) (*chplan.FuncCall, bool) {
	fc, ok := expr.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnMapMerge || len(fc.Args) != 2 {
		return nil, false
	}
	inner, ok := fc.Args[1].(*chplan.FuncCall)
	if !ok || inner.Fn != chplan.FnMap || len(inner.Args) != 2 {
		return nil, false
	}
	key, ok := inner.Args[0].(*chplan.LitString)
	if !ok || key.V != "__variant__" {
		return nil, false
	}
	return inner, true
}

func argsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestMultiVariantIsMetricQuery pins that a `variants(...) of (...)`
// expression routes through the metric (matrix) response shape rather
// than the log-stream wrap.
func TestMultiVariantIsMetricQuery(t *testing.T) {
	t.Parallel()
	expr, err := logql.ParseExprPermissive(
		`variants(count_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !logql.IsMetricQuery(expr) {
		t.Fatal("IsMetricQuery(variants(...)) = false; want true")
	}
}

// TestProjectSamplesForwardsVariantPlan pins that the wire-path Sample
// reshape forwards a multi-variant plan unchanged — both the FUSED shape and
// the per-arm UnionAll, since both already carry the canonical Sample
// columns. Wrapping either in the generic metric reshape would re-reference
// the `ResourceAttributes` column the variant Project has already consumed
// into `Attributes`, so the passthrough is load-bearing.
func TestProjectSamplesForwardsVariantPlan(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	cases := map[string]struct {
		query    string
		wantSQL  string
		wantScan int
	}{
		"fused": {
			query:    `variants(count_over_time({app="foo"}[5m]), bytes_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantSQL:  "ARRAY JOIN",
			wantScan: 1,
		},
		"per-arm union": {
			query:    `variants(count_over_time({app="foo"}[5m]), count_over_time({app="bar"}[5m])) of ({app="foo"}[5m])`,
			wantSQL:  "UNION ALL",
			wantScan: 2,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expr, err := logql.ParseExprPermissive(tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := logql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("lower: %v", err)
			}

			wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})
			if wrapped != plan {
				t.Fatalf("ProjectSamples wrapped the variant plan (%T); want it forwarded unchanged", wrapped)
			}

			sqlStr, _, err := chsql.Emit(context.Background(), wrapped)
			if err != nil {
				t.Fatalf("Emit(forwarded plan): %v", err)
			}
			if !strings.Contains(sqlStr, tc.wantSQL) {
				t.Errorf("forwarded SQL missing %q", tc.wantSQL)
			}
			if got := strings.Count(sqlStr, "`otel_logs`"); got != tc.wantScan {
				t.Errorf("log table read %d times; want %d", got, tc.wantScan)
			}
		})
	}
}

// TestLowerMultiVariantMatrixGrouped pins the intersection the fixtures miss:
// a RANGE query (matrix window, one row per step anchor) whose arms ALSO
// carry a vector-aggregation pipeline. That is the shape Grafana's Logs
// Drilldown breakdown panels generate, and it exercises the matrix emitter
// and the wrap threading together — variants_range_count_bytes covers matrix
// with bare arms, variants_grouped covers grouped arms on an instant window,
// and neither covers both at once.
func TestLowerMultiVariantMatrixGrouped(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)

	const query = `variants(sum by (app) (count_over_time({app="foo"}[5m])), ` +
		`sum by (app) (bytes_over_time({app="foo"}[5m]))) of ({app="foo"}[5m])`

	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := logql.LowerAtRange(context.Background(), expr, s, start, end, 30*time.Second)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := strings.Count(sqlText, "`otel_logs`"); got != 1 {
		t.Errorf("log table read %d times; the fused shape reads it once", got)
	}
	if !strings.Contains(sqlText, "ARRAY JOIN") {
		t.Error("fused SQL carries no ARRAY JOIN unpivot")
	}
	// The matrix shape must survive fusion: without a per-anchor grid the
	// range query collapses to a single point per series.
	if !strings.Contains(sqlText, "anchor_ts") {
		t.Error("fused matrix SQL carries no per-anchor grid")
	}
	// Both arms still reach the output, each under its own value column.
	for _, col := range []string{"Value_0", "Value_1"} {
		if !strings.Contains(sqlText, col) {
			t.Errorf("arm value column %q missing from SQL", col)
		}
	}
	for _, tag := range []string{"0", "1"} {
		if !argsContain(args, tag) {
			t.Errorf("args missing __variant__ tag %q", tag)
		}
	}
}
