package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestLower_ExpHistogram_MixedOrBesideHistogramVector(t *testing.T) {
	t.Parallel()
	queries := []string{
		"(latency_exp_hist or num_cpus) + other_exp_hist",
		"other_exp_hist + (latency_exp_hist or num_cpus)",
		"label_replace(latency_exp_hist or num_cpus, \"l\", \"$1\", \"series\", \"(.*)\") * 2",
		"label_join(latency_exp_hist or num_cpus, \"l\", \"-\", \"series\") * 2",
	}
	at := time.Unix(1700000000, 0)
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if got := chplan.RowShapeOf(plan); got != chplan.MixedRowShape {
				t.Fatalf("LowerAt(%q): root publishes %s, want %s", query, got, chplan.MixedRowShape)
			}
		})
	}
}
