package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// classicWindowGuardEnd is the instant every case below lowers at.
var classicWindowGuardEnd = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// classicWindowGuardSQL lowers query at classicWindowGuardEnd and returns the
// emitted SQL.
func classicWindowGuardSQL(t *testing.T, query string) string {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), classicWindowGuardEnd, classicWindowGuardEnd)
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr
}

// TestClassicBucketWindowGuard_EveryInstantWindowShapeCarriesBothChecks
// enumerates the instant classic-histogram shapes that reach the per-series
// window stage — every window function the idiom accepts, aggregated with
// and without a grouping, and the bare-selector form — and requires both
// window checks in each emitted statement.
func TestClassicBucketWindowGuard_EveryInstantWindowShapeCarriesBothChecks(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		`histogram_quantile(0.5, sum by (le) (rate(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (increase(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (sum_over_time(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (avg_over_time(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (max_over_time(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (min_over_time(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (last_over_time(req_bucket[5m])))`,
		`histogram_quantile(0.5, avg_over_time(req_bucket[5m]))`,
		`histogram_quantile(0.5, max_over_time(req_bucket[5m]))`,
		`histogram_quantile(0.5, min_over_time(req_bucket[5m]))`,
		`histogram_quantile(0.5, last_over_time(req_bucket[5m]))`,
		`histogram_quantile(0.5, sum by (le, job) (rate(req_bucket[5m])))`,
		`histogram_quantile(0.5, max by (le) (rate(req_bucket[5m])))`,
		`histogram_quantile(0.5, rate(req_bucket[5m]))`,
		`histogram_quantile(0.5, sum by (le) (req_bucket))`,
		`histogram_quantile(0.5, sum by (le) (rate(req_bucket[5m] @ 1767268800)))`,
		`histogram_quantile(0.5, sum without (job) (rate(req_bucket[5m] offset 1h)))`,
		`histogram_quantile(0.5, sum by (le) (rate(req_bucket{le=~"0.5|1|\\+Inf"}[5m])))`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sqlStr := classicWindowGuardSQL(t, query)
			for _, msg := range []string{chplan.ClassicBucketWindowBudgetMessage, chplan.ClassicBucketWindowSeriesBudgetMessage} {
				if !strings.Contains(sqlStr, msg) {
					t.Fatalf("emitted SQL carries no %q check\nSQL: %s", msg, sqlStr)
				}
			}
		})
	}
}

// TestClassicBucketWindowGuard_PreCountReadsTheScanOnce pins the shape of
// the query-wide check: one scalar subquery over a copy of the stage's
// filtered scan, counting entries, rows and series — not a window function
// over the stage's input, which would hold every scanned row until it had
// read them all.
func TestClassicBucketWindowGuard_PreCountReadsTheScanOnce(t *testing.T) {
	t.Parallel()
	sqlStr := classicWindowGuardSQL(t, `histogram_quantile(0.5, sum by (le) (rate(req_bucket[5m])))`)
	before, _, ok := strings.Cut(sqlStr, chplan.ClassicBucketWindowBudgetMessage)
	if !ok {
		t.Fatalf("no window check in\n%s", sqlStr)
	}
	guard := before[strings.LastIndex(before, "throwIf("):]
	for _, want := range []string{
		"sum((length(`ExplicitBounds`) + ?))",
		"count()",
		"uniqCombined64(`Attributes`, `ResourceAttributes`, `ServiceName`)",
		"FROM `otel_metrics_histogram`",
	} {
		if !strings.Contains(guard, want) {
			t.Fatalf("pre-count lacks %q\nguard: %s", want, guard)
		}
	}
	if strings.Contains(guard, " OVER ") {
		t.Fatalf("pre-count uses a window function\nguard: %s", guard)
	}
}

// TestClassicBucketWindowCeilings_DerivedFromCHQueryMaxMemory pins both
// window ceilings to the per-query memory cap: three-quarters of the cap for
// the query-wide estimate, half the remaining quarter over the fold's
// per-entry working set on two threads for the per-series ceiling, the
// one-GiB values for an unset cap, and an explicit ceiling winning over the
// derivation.
func TestClassicBucketWindowCeilings_DerivedFromCHQueryMaxMemory(t *testing.T) {
	t.Parallel()
	// The values docs/operations.md and docs/configuration.md state for the
	// 1 GiB default cap.
	const (
		shippedCostCeiling   int64 = 805_306_368
		shippedSeriesCeiling int64 = 152_520
	)
	const halfGiB = config.DefaultCHQueryMaxMemory / 2
	at := func(capBytes int64) ResourceBounds {
		return ResourceBounds{CHQueryMaxMemory: capBytes}.withDefaults()
	}
	def := at(config.DefaultCHQueryMaxMemory)
	if def.ClassicBucketWindowMaxCostUnits != shippedCostCeiling {
		t.Fatalf("cost ceiling at the default cap = %d, want %d", def.ClassicBucketWindowMaxCostUnits, shippedCostCeiling)
	}
	if def.ClassicBucketWindowSeriesMaxEntries != shippedSeriesCeiling {
		t.Fatalf("per-series ceiling at the default cap = %d, want %d", def.ClassicBucketWindowSeriesMaxEntries, shippedSeriesCeiling)
	}
	for _, tc := range []struct {
		capBytes            int64
		wantCost, wantEntry int64
	}{
		{halfGiB, 402_653_184, 76_260},
		{2 * config.DefaultCHQueryMaxMemory, 1_610_612_736, 305_040},
		{0, shippedCostCeiling, shippedSeriesCeiling},
	} {
		got := at(tc.capBytes)
		if got.ClassicBucketWindowMaxCostUnits != tc.wantCost || got.ClassicBucketWindowSeriesMaxEntries != tc.wantEntry {
			t.Fatalf("ceilings at a %d-byte cap = (%d, %d), want (%d, %d)", tc.capBytes,
				got.ClassicBucketWindowMaxCostUnits, got.ClassicBucketWindowSeriesMaxEntries, tc.wantCost, tc.wantEntry)
		}
	}
	pinned := ResourceBounds{ClassicBucketWindowMaxCostUnits: 7, ClassicBucketWindowSeriesMaxEntries: 9, CHQueryMaxMemory: halfGiB}.withDefaults()
	if pinned.ClassicBucketWindowMaxCostUnits != 7 || pinned.ClassicBucketWindowSeriesMaxEntries != 9 {
		t.Fatalf("explicit ceilings = (%d, %d), want (7, 9)", pinned.ClassicBucketWindowMaxCostUnits, pinned.ClassicBucketWindowSeriesMaxEntries)
	}
}
