package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// expHistWindowGuardEnd is the eval anchor every case below lowers at.
var expHistWindowGuardEnd = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// expHistWindowGuardSQL lowers query in one grid mode and returns the
// emitted SQL.
func expHistWindowGuardSQL(t *testing.T, query string, rangeMode bool) string {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	s := schema.DefaultOTelMetrics()
	var plan chplan.Node
	if rangeMode {
		plan, err = LowerAtRange(context.Background(), expr, s,
			expHistWindowGuardEnd.Add(-5*time.Minute), expHistWindowGuardEnd, time.Minute)
	} else {
		plan, err = LowerAt(context.Background(), expr, s, expHistWindowGuardEnd, expHistWindowGuardEnd)
	}
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr
}

// TestExpHistogramWindowGuard_EveryWindowFoldShapeCarriesIt is the
// anti-drift mechanism cerberus issue #3252's guard depends on, and it is
// deliberately keyed on QUERY SHAPES rather than on call sites.
//
// The guard has to ride directly on the reduction whose groupArray
// aliases it reads, and there are seven such reductions across the FOLD
// family's grid modes, the over_time range mode, and the fused
// `histogram_quantile(q, <fold>)` path. A test that asserted "these seven
// functions call the helper" would pass forever while an eighth reduction
// was added beside them. This one fails the moment a real query stops
// being covered.
//
// Both grid modes are run for every shape because they reduce through
// different nodes — a chplan.RangeBucketFanout in range mode, a plain
// chplan.Aggregate in instant mode — and the samples-per-window axis is
// the same in both: one instant anchor over a 5m range holds exactly as
// many samples as one of the range grid's anchors does.
func TestExpHistogramWindowGuard_EveryWindowFoldShapeCarriesIt(t *testing.T) {
	t.Parallel()

	// Every window-folding exp-histogram shape cerberus answers: the seven
	// FOLD names over a bare selector, the same under an aggregation, the
	// subquery form, and the fused quantile form the issue measured.
	for _, query := range []string{
		`rate(latency_exp_hist[5m])`,
		`increase(latency_exp_hist[5m])`,
		`delta(latency_exp_hist[5m])`,
		`irate(latency_exp_hist[5m])`,
		`idelta(latency_exp_hist[5m])`,
		`sum_over_time(latency_exp_hist[5m])`,
		`avg_over_time(latency_exp_hist[5m])`,
		`sum(rate(latency_exp_hist[5m]))`,
		`avg(sum_over_time(latency_exp_hist[5m]))`,
		`rate((latency_exp_hist)[5m:1m])`,
		`sum_over_time((latency_exp_hist)[5m:1m])`,
		`histogram_quantile(0.95, sum(rate(latency_exp_hist[5m])))`,
		`histogram_quantile(0.95, rate(latency_exp_hist[5m]))`,
	} {
		for _, mode := range []struct {
			name      string
			rangeMode bool
		}{{"instant", false}, {"range", true}} {
			t.Run(query+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				sqlStr := expHistWindowGuardSQL(t, query, mode.rangeMode)
				if !strings.Contains(sqlStr, chplan.ExpHistogramWindowSampleBudgetMessage) {
					t.Errorf("%s (%s): emitted SQL carries no samples-per-window pre-rejection — "+
						"this shape builds a per-group window groupArray and nothing bounds how many samples land in it",
						query, mode.name)
				}
			})
		}
	}
}

// TestExpHistogramWindowGuard_NonWindowShapesDoNotCarryIt is the
// discriminating control. The guard reads aliases only an exp-histogram
// WINDOW reduction publishes, so a shape without one must not carry it —
// and a guard bolted onto every reduction indiscriminately would both
// cost real work on cheap queries and fail to emit (the aliases would not
// resolve).
//
// The classic bucket ladder is the load-bearing member of this list: it
// also reduces through a chplan.RangeBucketFanout with groupArrays, and
// it is exactly what a plan-shape predicate keyed on "a fan-out with
// arrays" would over-match — the same over-match #3251's own two-conjunct
// predicate had to exclude by measurement.
func TestExpHistogramWindowGuard_NonWindowShapesDoNotCarryIt(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		`latency_exp_hist`,
		`sum(latency_exp_hist)`,
		`histogram_quantile(0.95, latency_exp_hist)`,
		`last_over_time(latency_exp_hist[5m])`,
		`count_over_time(latency_exp_hist[5m])`,
		`histogram_quantile(0.95, sum by (le) (rate(latency_bucket[5m])))`,
		`rate(plain_counter[5m])`,
	} {
		for _, mode := range []struct {
			name      string
			rangeMode bool
		}{{"instant", false}, {"range", true}} {
			t.Run(query+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				sqlStr := expHistWindowGuardSQL(t, query, mode.rangeMode)
				if strings.Contains(sqlStr, chplan.ExpHistogramWindowSampleBudgetMessage) {
					t.Errorf("%s (%s): carries the samples-per-window pre-rejection, but publishes no "+
						"exp-histogram window groupArray for it to measure", query, mode.name)
				}
			})
		}
	}
}

// TestExpHistogramWindowGuard_CostExpressionShape pins the arithmetic the
// guard actually emits, so a rewrite that keeps the message but loses a
// term fails here rather than silently widening the bound by orders of
// magnitude.
//
// The two quantities are asserted by the aliases they read rather than by
// re-deriving the expression: `length` of the timestamp groupArray is S
// (NOT uniqExact — see [expHistogramWindowSamplesExpr]), and an arrayMax
// of per-element lengths over BOTH bucket groupArrays is W.
func TestExpHistogramWindowGuard_CostExpressionShape(t *testing.T) {
	t.Parallel()

	sqlStr := expHistWindowGuardSQL(t, `rate(latency_exp_hist[5m])`, true)
	for _, want := range []string{
		"throwIf(",
		chplan.ExpHistogramWindowSampleBudgetMessage,
		"length(`" + hqWindowTsListAlias + "`)",
		"arrayMax(arrayMap(",
		hqAggPosBucketsArrayAlias,
		hqAggNegBucketsArrayAlias,
	} {
		if !strings.Contains(sqlStr, want) {
			t.Errorf("emitted guard is missing %q\nSQL: %s", want, sqlStr)
		}
	}
	// uniqExact still appears — the fan-out's own MinSamples HAVING uses
	// it — but never as the guard's own sample count, which must be the
	// array length. Asserting the length form above is what pins that;
	// this asserts the two are not confused by checking the guard's
	// message and the length expression appear in one statement.
	if strings.Count(sqlStr, "length(`"+hqWindowTsListAlias+"`)") < 2 {
		t.Errorf("the cost expression names S twice (S * W * (S + W)); got %d occurrence(s)\nSQL: %s",
			strings.Count(sqlStr, "length(`"+hqWindowTsListAlias+"`)"), sqlStr)
	}
}

// TestExpHistogramWindowCostUnitsForMemory pins the cap derivation.
//
// The expectations are LITERAL numbers rather than expressions over the
// constants under test: computing them from expHistogramWindowCostUnitsPerGiB
// would pass under any value that constant happened to hold, including one
// so large the guard never fires — which is exactly the failure mode a
// derivation test exists to catch. The values below are the ones the
// measurement in exp_histogram_window_sample_bound.go's own doc justifies.
func TestExpHistogramWindowCostUnitsForMemory(t *testing.T) {
	t.Parallel()

	const gib int64 = 1 << 30
	for _, tc := range []struct {
		name string
		cap  int64
		want int64
		why  string
	}{
		{
			"product default 1 GiB", gib, 5_000_000,
			"the calibrated per-GiB rate, applied once",
		},
		{
			"8 GiB", 8 * gib, 40_000_000,
			"linear in the cap: the cost units are a proxy for bytes",
		},
		{
			"1.5 GiB", gib + gib/2, 7_500_000,
			"the remainder is proportional, not rounded down to 1 GiB",
		},
		{
			"256 MiB", 256 << 20, 1_500_000,
			"below the floor: a small cap narrows the guard, it does not close it",
		},
		{
			"unset", 0, 5_000_000,
			"no cap stamped at all — the same bound a 1 GiB deployment gets",
		},
		{
			"negative", -1, 5_000_000,
			"treated as unset rather than as a zero budget that rejects everything",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExpHistogramWindowCostUnitsForMemory(tc.cap); got != tc.want {
				t.Errorf("ExpHistogramWindowCostUnitsForMemory(%d) = %d, want %d (%s)",
					tc.cap, got, tc.want, tc.why)
			}
		})
	}
}

// TestExpHistogramWindowBound_AdmitsTheOrdinaryPanelAndRefusesTheDenseSeries
// is the calibration's own falsifiability check: a ceiling is only a
// safety rail if it separates the two populations the measurement
// separated. Asserted as arithmetic over the resolved ceiling rather than
// through a query, because the axis is a property of the DATA, which no
// lowering test can seed.
//
// The two points are the ones the issue and the measurement name: an
// ordinary OTel histogram scraped at 15s contributes 20 samples to a 5m
// window, and the 20 Hz series that motivated #3252 contributes 6000. The
// widths are the measured stored widths of the same two metrics.
func TestExpHistogramWindowBound_AdmitsTheOrdinaryPanelAndRefusesTheDenseSeries(t *testing.T) {
	t.Parallel()

	const gib int64 = 1 << 30
	units := ExpHistogramWindowCostUnitsForMemory(gib)
	cost := func(samples, width int64) int64 { return samples * width * (samples + width) }

	for _, tc := range []struct {
		name           string
		samples, width int64
		wantRejected   bool
	}{
		{"15s scrape, 5m window, 8 buckets", 20, 8, false},
		{"1s scrape, 5m window, 8 buckets", 300, 8, false},
		{"wide-bucket metric, 30 samples, 152 buckets", 30, 152, false},
		{"20 Hz series, 5m window, 8 buckets", 6000, 8, true},
		{"20 Hz series, 1m window, 8 buckets", 1200, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cost(tc.samples, tc.width) > units; got != tc.wantRejected {
				t.Errorf("S=%d W=%d: cost %d against ceiling %d gives rejected=%v, want %v",
					tc.samples, tc.width, cost(tc.samples, tc.width), units, got, tc.wantRejected)
			}
		})
	}
}
