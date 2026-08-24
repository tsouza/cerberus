package promql

import (
	"context"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// deltaPrefixEnabledSchema returns schema.DefaultOTelMetrics() with the
// DELTA-prefix aggregate table fields populated exactly the way production
// resolves them (CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true), via the real
// schema.DefaultOTelMetricsFrom env-resolution path rather than hand-set
// literals, so this test tracks the real defaults instead of duplicating
// them.
func deltaPrefixEnabledSchema() schema.Metrics {
	return schema.DefaultOTelMetricsFrom(func(key string) string {
		if key == schema.EnvMetricsDeltaPrefixEnabled {
			return "true"
		}
		return ""
	})
}

// findRangeWindow returns the first *chplan.RangeWindow reachable from n in
// pre-order, or nil. Every fixture in this file lowers to exactly one.
func findRangeWindow(n chplan.Node) *chplan.RangeWindow {
	var found *chplan.RangeWindow
	chplan.Walk(n, func(node chplan.Node) bool {
		if found != nil {
			return false
		}
		if rw, ok := node.(*chplan.RangeWindow); ok {
			found = rw
			return false
		}
		return true
	})
	return found
}

// deltaPrefixProjectionExpr returns the Expr projected under alias in p, or
// nil when no projection carries that alias.
func deltaPrefixProjectionExpr(p *chplan.Project, alias string) chplan.Expr {
	for _, proj := range p.Projections {
		if proj.Alias == alias {
			return proj.Expr
		}
	}
	return nil
}

// TestDeltaPrefixAggregateEligibleFunc pins the narrower-than-
// counterTemporalityRangeFn eligibility set: only rate/increase route
// through deltaFirstValFrag's extrapolationKind.isCounter() branch in
// chsql, so only those two get chplan.RangeWindow.DeltaPrefixAggregateInput
// populated — irate (temporality-eligible but not counter-extrapolated) and
// every plain *_over_time function stay excluded.
func TestDeltaPrefixAggregateEligibleFunc(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"rate": true, "increase": true,
		"irate": false, "delta": false, "sum_over_time": false, "avg_over_time": false,
	}
	for fn, want := range cases {
		if got := deltaPrefixAggregateEligibleFunc(fn); got != want {
			t.Errorf("deltaPrefixAggregateEligibleFunc(%q) = %v, want %v", fn, got, want)
		}
	}
}

// TestDeltaPrefixAggregateArm_AttributesExprMatchesInput is Task 6's
// structural plan-equality guard (cerberus issue #2389, design comment
// "§4.2 revision", task 6 item 4): chplan.RangeWindow.DeltaPrefixAggregateInput's
// Attributes projection Expr must be structurally Equal to r.Input's own
// Attributes projection Expr, because both are built by the IDENTICAL
// selectorAttributesExpr(ctx, s) call — augmentSelectorAttributes for Input,
// augmentDeltaPrefixAggregateAttributes for DeltaPrefixAggregateInput.
//
// This is the cheap, always-on guard against the exact series-identity-
// alignment landmine the issue's Task 1 chDB spike found (a raw-tuple → 7
// distinct read-time series collapse via Map key order / sanitisation /
// dedicated-column overwrite): if the two expressions ever diverge, chsql's
// deltaPrefixAggregateSource's name-based GroupBy join silently stops
// matching and the fast path degrades to reporting zero prior accumulation
// for every series — wrong, and silent, not a query error.
func TestDeltaPrefixAggregateArm_AttributesExprMatchesInput(t *testing.T) {
	t.Parallel()
	s := deltaPrefixEnabledSchema()
	if s.DeltaPrefixTable == "" {
		t.Fatal("test fixture broken: deltaPrefixEnabledSchema didn't populate DeltaPrefixTable")
	}

	for _, funcName := range []string{"rate", "increase"} {
		t.Run(funcName, func(t *testing.T) {
			t.Parallel()
			expr := mustParse(t, funcName+`(http_requests_total{env="prod"}[5m])`)
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			rw := findRangeWindow(plan)
			if rw == nil {
				t.Fatalf("no RangeWindow in lowered plan %#v", plan)
			}
			if rw.DeltaPrefixAggregateInput == nil {
				t.Fatal("DeltaPrefixAggregateInput is nil — eligibility gating dropped it for a rate()/increase() call over a schema naming a DeltaPrefixTable")
			}

			inputProject, ok := rw.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("rw.Input is %T, want *chplan.Project (the default schema always sets "+
					"ResourceAttributesColumn/ServiceNameColumn, so augmentSelectorAttributes never "+
					"takes its bare-passthrough skip)", rw.Input)
			}
			aggProject, ok := rw.DeltaPrefixAggregateInput.(*chplan.Project)
			if !ok {
				t.Fatalf("rw.DeltaPrefixAggregateInput is %T, want *chplan.Project", rw.DeltaPrefixAggregateInput)
			}

			wantExpr := deltaPrefixProjectionExpr(inputProject, s.AttributesColumn)
			gotExpr := deltaPrefixProjectionExpr(aggProject, s.AttributesColumn)
			if wantExpr == nil {
				t.Fatal("rw.Input's Project carries no Attributes projection")
			}
			if gotExpr == nil {
				t.Fatal("rw.DeltaPrefixAggregateInput's Project carries no Attributes projection")
			}
			if !gotExpr.Equal(wantExpr) {
				t.Errorf("DeltaPrefixAggregateInput's Attributes expr is NOT Equal to Input's:\n  input: %#v\n  agg:   %#v",
					wantExpr, gotExpr)
			}
			if !wantExpr.Equal(gotExpr) {
				t.Error("Expr.Equal is not symmetric for the Attributes projection")
			}
		})
	}
}

// TestDeltaPrefixAggregateArm_ScanFilteredByMetricNameOnly pins §3's exact
// filtering contract: the aggregate arm's Scan is gated on the SAME
// MetricName matcher the primary selector arm resolves, and nothing else —
// the query's other label matchers (env=/region=~ here) must never reach
// this scan, because the aggregate table's GROUP BY join narrows on the
// shaped Attributes key at chsql emit time, not on a second copy of the
// label predicate.
func TestDeltaPrefixAggregateArm_ScanFilteredByMetricNameOnly(t *testing.T) {
	t.Parallel()
	s := deltaPrefixEnabledSchema()
	expr := mustParse(t, `rate(http_requests_total{env="prod",region=~"eu.*"}[5m])`)
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	rw := findRangeWindow(plan)
	if rw == nil || rw.DeltaPrefixAggregateInput == nil {
		t.Fatal("DeltaPrefixAggregateInput missing")
	}
	aggProject, ok := rw.DeltaPrefixAggregateInput.(*chplan.Project)
	if !ok {
		t.Fatalf("DeltaPrefixAggregateInput is %T, want *chplan.Project", rw.DeltaPrefixAggregateInput)
	}
	filter, ok := aggProject.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("aggProject.Input is %T, want *chplan.Filter (the metric-name matcher predicate)", aggProject.Input)
	}
	scan, ok := filter.Input.(*chplan.Scan)
	if !ok {
		t.Fatalf("filter.Input is %T, want *chplan.Scan", filter.Input)
	}
	if scan.Table != s.DeltaPrefixTable {
		t.Errorf("Scan.Table = %q, want %q", scan.Table, s.DeltaPrefixTable)
	}

	wantMatchers := []*labels.Matcher{{Type: labels.MatchEqual, Name: model.MetricNameLabel, Value: "http_requests_total"}}
	wantPred := buildPredicate(wantMatchers, s)
	if !filter.Predicate.Equal(wantPred) {
		t.Errorf("Filter.Predicate = %#v, want the metric-name-only predicate %#v "+
			"(the env=/region=~ matchers must NOT reach this scan)", filter.Predicate, wantPred)
	}
}

// TestDeltaPrefixAggregateArm_AbsentWithoutSchemaTable pins the primary
// backward-compat gate: a schema that hasn't opted into the DELTA-prefix
// table (schema.Metrics.DeltaPrefixTable == "", the default absent
// CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED) never gets the field populated, so
// lowering is byte-identical to before this mechanism existed.
func TestDeltaPrefixAggregateArm_AbsentWithoutSchemaTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	if s.DeltaPrefixTable != "" {
		t.Fatal("fixture assumption broken: DefaultOTelMetrics() no longer defaults DeltaPrefixTable to empty")
	}
	expr := mustParse(t, `rate(http_requests_total[5m])`)
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	rw := findRangeWindow(plan)
	if rw == nil {
		t.Fatal("no RangeWindow in lowered plan")
	}
	if rw.DeltaPrefixAggregateInput != nil {
		t.Error("DeltaPrefixAggregateInput populated despite an empty schema.Metrics.DeltaPrefixTable")
	}
}

// TestDeltaPrefixAggregateArm_AbsentForIneligibleFunc pins the
// deltaPrefixAggregateEligibleFunc gate end to end through Lower: even on a
// schema that names a DeltaPrefixTable, a range function outside {rate,
// increase} never gets the field populated.
func TestDeltaPrefixAggregateArm_AbsentForIneligibleFunc(t *testing.T) {
	t.Parallel()
	s := deltaPrefixEnabledSchema()
	for _, funcName := range []string{"irate", "delta", "sum_over_time"} {
		t.Run(funcName, func(t *testing.T) {
			t.Parallel()
			expr := mustParse(t, funcName+`(http_requests_total[5m])`)
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			rw := findRangeWindow(plan)
			if rw == nil {
				t.Fatal("no RangeWindow in lowered plan")
			}
			if rw.DeltaPrefixAggregateInput != nil {
				t.Errorf("%s: DeltaPrefixAggregateInput populated — deltaPrefixAggregateEligibleFunc should exclude it", funcName)
			}
		})
	}
}
