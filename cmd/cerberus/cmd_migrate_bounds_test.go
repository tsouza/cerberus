package main

import (
	"context"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
)

// TestDryRunExplainer_ExpHistogramBudgetFollowsTheConfiguredMemoryCap pins
// that the offline preview lowers with the SAME resource bounds the server
// runs under. The exponential-histogram window budget is derived from
// CERBERUS_CH_QUERY_MAX_MEMORY (promql.ExpHistogramWindowCostUnitsForMemory)
// at the lowering seam; a preview engine that left ResourceBounds at its
// zero value bound every deployment's SQL to the 1 GiB-derived default,
// while `migrate explain` claims byte-identical SQL to the server's. The
// budget is a bound argument of the emitted SQL, so the assertion reads
// the dry run's Args.
func TestDryRunExplainer_ExpHistogramBudgetFollowsTheConfiguredMemoryCap(t *testing.T) {
	const fourGiB = int64(4) << 30
	cfg := cfgWithBudget()
	cfg.ClickHouse.MaxQueryMemoryBytes = fourGiB
	ex, err := newDryRunExplainer(cfg)
	if err != nil {
		t.Fatalf("newDryRunExplainer: %v", err)
	}

	const query = "sum(rate(latency_exp_hist[5m]))"
	dr, err := ex.eng.DryRunSQL(context.Background(), ex.promRange, query)
	if err != nil {
		t.Fatalf("dry run %q: %v", query, err)
	}

	want := promql.ExpHistogramWindowCostUnitsForMemory(fourGiB)
	def := promql.ExpHistogramWindowCostUnitsForMemory(0)
	if want == def {
		t.Fatalf("test premise: a 4 GiB cap derives the same budget (%d) as no cap; the assertion below could not discriminate", want)
	}
	if !slices.Contains(dr.Args, any(want)) {
		t.Errorf("the preview's bound args %v carry no exp-histogram window budget of %d (the 4 GiB derivation); "+
			"the offline lowering does not run under the configured memory cap", dr.Args, want)
	}
	if slices.Contains(dr.Args, any(def)) {
		t.Errorf("the preview's bound args carry the no-cap default budget %d beside a configured 4 GiB cap", def)
	}
}
