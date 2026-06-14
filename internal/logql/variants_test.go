package logql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerMultiVariant pins the LogQL `variants(...) of (...)` lowering:
// each variant arm lowers independently, gets re-shaped into the
// canonical Sample contract, and is tagged with a synthetic
// `__variant__="<index>"` label folded into its Attributes map. The arms
// are concatenated with a UnionAll. Mirrors reference Loki's
// constants.VariantLabel labelling (one arm per index).
func TestLowerMultiVariant(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()

	tests := []struct {
		name     string
		query    string
		wantArms int
		wantTags []string // `__variant__` value expected per arm, in order
	}{
		{
			name:     "single variant",
			query:    `variants(count_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantArms: 1,
			wantTags: []string{"0"},
		},
		{
			name:     "two variants count + bytes",
			query:    `variants(count_over_time({app="foo"}[5m]), bytes_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
			wantArms: 2,
			wantTags: []string{"0", "1"},
		},
		{
			name:     "grouped variants",
			query:    `variants(sum by (app) (count_over_time({app="foo"}[5m])), sum by (app) (bytes_over_time({app="foo"}[5m]))) of ({app="foo"}[5m])`,
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

			u, ok := plan.(*chplan.UnionAll)
			if !ok {
				t.Fatalf("top node = %T; want *chplan.UnionAll", plan)
			}
			if len(u.Inputs) != tt.wantArms {
				t.Fatalf("UnionAll arms = %d; want %d", len(u.Inputs), tt.wantArms)
			}

			// Each arm must be a top-level Project aliasing the canonical
			// Sample columns, with the `__variant__` tag folded into
			// Attributes.
			for i, arm := range u.Inputs {
				proj, ok := arm.(*chplan.Project)
				if !ok {
					t.Fatalf("arm %d = %T; want *chplan.Project", i, arm)
				}
				var sawAttrs bool
				for _, p := range proj.Projections {
					if p.Alias == "Attributes" {
						sawAttrs = true
						if !attrsCarriesVariantTag(p.Expr, tt.wantTags[i]) {
							t.Errorf("arm %d Attributes expr does not fold __variant__=%q: %#v",
								i, tt.wantTags[i], p.Expr)
						}
					}
				}
				if !sawAttrs {
					t.Errorf("arm %d Project has no Attributes projection", i)
				}
			}

			// The emitted SQL must be a single self-contained statement
			// (UNION ALL across arms) carrying the variant tags as bound
			// string args.
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if tt.wantArms > 1 && !strings.Contains(sqlStr, "UNION ALL") {
				t.Errorf("multi-arm SQL missing UNION ALL: %s", sqlStr)
			}
			for _, tag := range tt.wantTags {
				if !argsContain(args, "__variant__") || !argsContain(args, tag) {
					t.Errorf("args missing __variant__ tag %q: %v", tag, args)
				}
			}
		})
	}
}

// attrsCarriesVariantTag reports whether expr is the
// `mapConcat(<attrs>, map("__variant__", "<tag>"))` shape the variant
// lowering builds.
func attrsCarriesVariantTag(expr chplan.Expr, tag string) bool {
	fc, ok := expr.(*chplan.FuncCall)
	if !ok || fc.Name != "mapConcat" || len(fc.Args) != 2 {
		return false
	}
	inner, ok := fc.Args[1].(*chplan.FuncCall)
	if !ok || inner.Name != "map" || len(inner.Args) != 2 {
		return false
	}
	key, ok := inner.Args[0].(*chplan.LitString)
	if !ok || key.V != "__variant__" {
		return false
	}
	val, ok := inner.Args[1].(*chplan.LitString)
	return ok && val.V == tag
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

// TestProjectSamplesForwardsVariantUnion pins that the wire-path
// Sample reshape forwards a multi-variant UnionAll unchanged (its arms
// are already canonical Sample shape), and the forwarded plan emits
// valid SQL. Wrapping the union in the generic metric reshape would
// re-reference the `ResourceAttributes` column the per-arm Project has
// already consumed into `Attributes`, so the passthrough is load-bearing.
func TestProjectSamplesForwardsVariantUnion(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	l := &logql.Lang{Schema: s}

	expr, err := logql.ParseExprPermissive(
		`variants(count_over_time({app="foo"}[5m]), bytes_over_time({app="foo"}[5m])) of ({app="foo"}[5m])`,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})
	if wrapped != plan {
		t.Fatalf("ProjectSamples wrapped the variant union (%T); want the same *chplan.UnionAll forwarded unchanged", wrapped)
	}

	// The forwarded plan must emit valid SQL carrying both variant tags.
	sqlStr, _, err := chsql.Emit(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Emit(forwarded union): %v", err)
	}
	if !strings.Contains(sqlStr, "UNION ALL") {
		t.Errorf("forwarded union SQL missing UNION ALL: %s", sqlStr)
	}
}
