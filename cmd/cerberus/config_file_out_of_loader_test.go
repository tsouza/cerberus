package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestConfigFile_OutOfLoaderCeilingReachesTheLowering pins the whole path a
// cerberus.yaml value takes to a setting the typed registry does not own:
// the file, through config.FromEnv's Settings lookup, through
// resolveBoundOverrides, into promql's lowering, out as the ceiling literal in
// the emitted guard. Each hop is unit-tested on its own; this is the proof the
// hops are actually joined, because a file key that every hop accepts and none
// applies loads without error and changes nothing.
func TestConfigFile_OutOfLoaderCeilingReachesTheLowering(t *testing.T) {
	// The ceiling is chosen so it cannot be mistaken for the derived default,
	// which is in the millions.
	const ceiling int64 = 5
	t.Setenv(promql.EnvExpHistogramWindowMaxCostUnits, "")
	if err := os.Unsetenv(promql.EnvExpHistogramWindowMaxCostUnits); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	inCerberusYAML(t, promql.EnvExpHistogramWindowMaxCostUnits+": "+strconv.FormatInt(ceiling, 10)+"\n")

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	_, promBounds, err := resolveBoundOverrides(cfg)
	if err != nil {
		t.Fatalf("resolveBoundOverrides: %v", err)
	}

	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(`rate(latency_exp_hist[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		end.Add(-5*time.Minute), end, time.Minute, promql.LowerOpts{ResourceBounds: promBounds})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts: %v", err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The guard renders as `throwIf(<cost> > ?, '<message>')`: the ceiling is
	// the bound parameter immediately before the inline message, so it is the
	// last placeholder in the SQL prefix that ends at the message.
	before, _, ok := strings.Cut(sqlStr, chplan.ExpHistogramWindowSampleBudgetMessage)
	if !ok {
		t.Fatalf("emitted SQL carries no samples-per-window guard at all\nSQL: %s", sqlStr)
	}
	ceilingArg := strings.Count(before, "?") - 1
	if ceilingArg < 0 || ceilingArg >= len(args) {
		t.Fatalf("no bound parameter precedes the guard message (placeholder index %d of %d args)", ceilingArg, len(args))
	}
	if got := args[ceilingArg]; got != ceiling {
		t.Errorf("emitted guard compares against %v (%T), want the file's ceiling %d", got, got, ceiling)
	}
}
