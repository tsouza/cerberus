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

// classicWindowScanTable is the FROM clause of the scan every case below
// reads.
const classicWindowScanTable = "FROM `otel_metrics_histogram`"

// TestClassicBucketWindowGuard_EveryInstantWindowShapeReadsTheWindowOnce
// enumerates the instant classic-histogram shapes that reach the per-series
// window stage — every window function the idiom accepts, aggregated with
// and without a grouping, and the bare-selector form — and requires each
// emitted statement to carry the per-series check and to read the histogram
// table exactly once: no pre-count over a second copy of the window.
func TestClassicBucketWindowGuard_EveryInstantWindowShapeReadsTheWindowOnce(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		`histogram_quantile(0.5, sum by (le) (rate(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (increase(req_bucket[5m])))`,
		`histogram_quantile(0.5, sum by (le) (sum_over_time(req_bucket[5m])))`,
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
			if !strings.Contains(sqlStr, chplan.ClassicBucketWindowSeriesBudgetMessage) {
				t.Fatalf("emitted SQL carries no %q check\nSQL: %s", chplan.ClassicBucketWindowSeriesBudgetMessage, sqlStr)
			}
			if n := strings.Count(sqlStr, classicWindowScanTable); n != 1 {
				t.Fatalf("emitted SQL reads the histogram table %d times, want once\nSQL: %s", n, sqlStr)
			}
		})
	}
}

// TestClassicBucketWindowSeriesCeiling_DerivedFromCHQueryMaxMemory pins the
// per-series window ceiling to the per-query memory cap: half of a quarter
// of the cap over the fold's per-entry working set on two threads, the
// one-GiB value for an unset cap, and an explicit ceiling winning over the
// derivation.
func TestClassicBucketWindowSeriesCeiling_DerivedFromCHQueryMaxMemory(t *testing.T) {
	t.Parallel()
	// The value docs/operations.md and docs/configuration.md state for the
	// 1 GiB default cap.
	const shippedSeriesCeiling int64 = 152_520
	const halfGiB = config.DefaultCHQueryMaxMemory / 2
	at := func(capBytes int64) ResourceBounds {
		return ResourceBounds{CHQueryMaxMemory: capBytes}.withDefaults()
	}
	if got := at(config.DefaultCHQueryMaxMemory).ClassicBucketWindowSeriesMaxEntries; got != shippedSeriesCeiling {
		t.Fatalf("per-series ceiling at the default cap = %d, want %d", got, shippedSeriesCeiling)
	}
	for _, tc := range []struct {
		capBytes  int64
		wantEntry int64
	}{
		{halfGiB, 76_260},
		{2 * config.DefaultCHQueryMaxMemory, 305_040},
		{0, shippedSeriesCeiling},
	} {
		if got := at(tc.capBytes).ClassicBucketWindowSeriesMaxEntries; got != tc.wantEntry {
			t.Fatalf("per-series ceiling at a %d-byte cap = %d, want %d", tc.capBytes, got, tc.wantEntry)
		}
	}
	pinned := ResourceBounds{ClassicBucketWindowSeriesMaxEntries: 9, CHQueryMaxMemory: halfGiB}.withDefaults()
	if pinned.ClassicBucketWindowSeriesMaxEntries != 9 {
		t.Fatalf("explicit per-series ceiling = %d, want 9", pinned.ClassicBucketWindowSeriesMaxEntries)
	}
}
