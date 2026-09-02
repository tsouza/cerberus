//go:build chdb

// chDB-backed proof that the duplicate-ROW contract reaches the five range
// functions outside the `*_over_time` family — cerberus issue #2927.
//
// # The contract, unchanged
//
// A window's sample multiset is its DISTINCT (timestamp, value) rows. PR #2928
// (cerberus issue #2914) established it for `*_over_time`; this file executes
// the SAME rule for the range functions that were left counting a duplicated
// row twice: irate, idelta, deriv, predict_linear and
// double_exponential_smoothing — plus changes and resets, which the rule now
// declares for family consistency and which are proven here to be immune to
// it rather than assumed to be.
//
// The identity is Prometheus's own: tsdb.memSeries.appendable compares an
// incoming sample against the stored one by VALUE BITS, so a bit-identical
// re-append is a silent no-op and a conflicting one is rejected. An
// identical-row duplicate is therefore storage duplication Prometheus absorbs
// and cerberus must absorb too. The CONFLICTING-value duplicate is untouched
// by this file and stays exactly where cerberus issue #2905 left it — see
// duplicate_timestamp_seed_chdb_test.go, which owns that shape.
//
// # Why every case computes its expectation with a Go reducer
//
// Each case names the reducer that defines its answer over a sample multiset,
// and the runner applies that reducer to BOTH candidate windows: the
// contract's (the duplicate collapsed) and the both-rows one. It then asserts
// the executed answer is the first and is NOT the second, and cross-checks the
// case's own `immune` declaration against whether those two arithmetics
// actually coincide. A case cannot therefore pass by asserting the number the
// emitter happens to produce, and an immunity claim cannot drift away from the
// arithmetic that justifies it.
//
// # Why the alternate lowerings run too
//
// One rule, every lowering of the same query. Each function's alternates run
// the identical query through the lagInFrame adjacency shape and the native
// timeSeries*ToGrid shape where it has one, asserting the emitted SQL actually
// DIFFERS (so a declined strategy cannot make the comparison an answer against
// itself) and that the answer is the same contract answer.
package promql_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
)

// dupRowMetric is this file's own metric name, distinct from every other name
// sharing the package's single chDB session (fixture_chdb_test.go).
const dupRowMetric = "duplicate_sample_row_range_family_test_metric"

// dupRowSeed is cerberus issue #2927's own seed: one series, three logical
// samples, and the final one stored TWICE with an identical value.
//
// The duplicate sits at the window's trailing edge on purpose. That is the
// position where the trailing-pair functions (irate / idelta) degenerate into
// comparing the duplicate against itself, and the position of maximum leverage
// on the least-squares fit (deriv / predict_linear), so one seed exercises
// every failure mode the issue measured.
//
// The values are NOT collinear: 1 -> 10 -> 5 puts the duplicated point off the
// line through the other two, which is what makes the regression pair diverge
// at all. A collinear duplicate re-weights the fit without moving it, which is
// why a casual reproduction of this bug shows nothing.
const dupRowSeed = dupTSMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('` + dupRowMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),
    ('` + dupRowMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 10.0),
    ('` + dupRowMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 5.0),
    ('` + dupRowMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 5.0);
`

// dupRowPoint is one sample of the window, positioned by its offset from the
// evaluation anchor in seconds (negative going back in time) — the same x-axis
// chsql's windowPairsSLRFrag builds with `dateDiff('second', anchor, ts)`, so
// the regression reducers below are computed over the emitter's own axis
// rather than a re-derivation of it.
type dupRowPoint struct {
	offsetSeconds float64
	value         float64
}

// The two candidate sample multisets for the final anchor's
// (00:00:00, 00:05:00] window, in ascending timestamp order:
//
//	dupRowContractWindow — the duplicate counted ONCE, which is also exactly
//	  what Prometheus's own head storage holds for this seed.
//	dupRowBothRowsWindow — both stored ROWS, the window cerberus built for
//	  these five functions before this change.
var (
	dupRowContractWindow = []dupRowPoint{{-240, 1}, {-120, 10}, {0, 5}}
	dupRowBothRowsWindow = []dupRowPoint{{-240, 1}, {-120, 10}, {0, 5}, {0, 5}}
)

// dupRowPredictHorizon is the `t` seconds argument predict_linear projects
// forward. Whole-second and non-negative so the NATIVE
// timeSeriesPredictLinearToGrid arm is eligible too (its predict_offset is a
// constant parametric arg — see promql.nativePredictLinearHorizonEligible);
// a fractional or negative horizon would silently keep every lowering on the
// fan-out and turn the cross-lowering comparison into a comparison of an
// answer with itself.
const dupRowPredictHorizon = 60.0

// dupRowSmoothingFactor / dupRowTrendFactor are
// double_exponential_smoothing's (sf, tf). Both 0.5, inside the open (0, 1)
// domain Prometheus requires.
const (
	dupRowSmoothingFactor = 0.5
	dupRowTrendFactor     = 0.5
)

// dupRowIdelta is idelta's reducer: the plain difference of the last two
// samples, with no counter-reset repair (Prometheus's funcIdelta, and
// chsql's emitRangeWindowIDelta, both skip it).
func dupRowIdelta(pts []dupRowPoint) float64 {
	if len(pts) < 2 {
		return math.NaN()
	}
	return pts[len(pts)-1].value - pts[len(pts)-2].value
}

// dupRowIrate is irate's reducer: the trailing pair's counter-reset-repaired
// increase over the pair's interval in seconds. The repair (`curr < prev` =>
// the raw curr) is Prometheus's funcIrate and chsql's CounterOrDeltaPairDelta
// CUMULATIVE branch; this seed's 10 -> 5 pair exercises it.
//
// A zero-length interval yields NaN, which is precisely the defect: when the
// trailing pair is a duplicated row against itself the interval is zero and
// the whole SERIES disappears from the answer rather than merely answering a
// wrong number.
func dupRowIrate(pts []dupRowPoint) float64 {
	if len(pts) < 2 {
		return math.NaN()
	}
	prev, curr := pts[len(pts)-2], pts[len(pts)-1]
	interval := curr.offsetSeconds - prev.offsetSeconds
	if interval <= 0 {
		return math.NaN()
	}
	delta := curr.value - prev.value
	if curr.value < prev.value {
		delta = curr.value
	}
	return delta / interval
}

// dupRowSimpleLinearRegression is ClickHouse's simpleLinearRegression and
// Prometheus's linearRegression: the ordinary least-squares (slope, intercept)
// of the window against the anchor-relative seconds axis. The intercept is the
// fitted value AT the anchor, which is what makes predict_linear's answer
// `intercept + slope*t`.
func dupRowSimpleLinearRegression(pts []dupRowPoint) (slope, intercept float64) {
	n := float64(len(pts))
	var sumX, sumY, sumXY, sumX2 float64
	for _, p := range pts {
		sumX += p.offsetSeconds
		sumY += p.value
		sumXY += p.offsetSeconds * p.value
		sumX2 += p.offsetSeconds * p.offsetSeconds
	}
	slope = (sumXY - sumX*sumY/n) / (sumX2 - sumX*sumX/n)
	return slope, sumY/n - slope*sumX/n
}

// dupRowDeriv is deriv's reducer: the least-squares slope, per second.
func dupRowDeriv(pts []dupRowPoint) float64 {
	if len(pts) < 2 {
		return math.NaN()
	}
	slope, _ := dupRowSimpleLinearRegression(pts)
	return slope
}

// dupRowPredictLinear is predict_linear's reducer: the same fit evaluated
// dupRowPredictHorizon seconds past the anchor.
func dupRowPredictLinear(pts []dupRowPoint) float64 {
	if len(pts) < 2 {
		return math.NaN()
	}
	slope, intercept := dupRowSimpleLinearRegression(pts)
	return intercept + slope*dupRowPredictHorizon
}

// dupRowDoubleExponentialSmoothing is
// double_exponential_smoothing's reducer, transcribing Prometheus's
// funcDoubleExponentialSmoothing: the level is seeded with the SECOND sample
// and the trend with the first difference, then the recurrence runs from the
// third sample on. chsql's holtWintersValueFrag folds the identical
// recurrence over window_vals[3:] from the same seed.
func dupRowDoubleExponentialSmoothing(pts []dupRowPoint) float64 {
	if len(pts) < 2 {
		return math.NaN()
	}
	level := pts[1].value
	trend := pts[1].value - pts[0].value
	for _, p := range pts[2:] {
		next := dupRowSmoothingFactor*p.value + (1-dupRowSmoothingFactor)*(level+trend)
		trend = dupRowTrendFactor*(next-level) + (1-dupRowTrendFactor)*trend
		level = next
	}
	return level
}

// dupRowChanges is changes' reducer: adjacent pairs where the value differs,
// with Prometheus's both-NaN carve-out (cerberus issue #1489).
func dupRowChanges(pts []dupRowPoint) float64 {
	count := 0.0
	for i := 1; i < len(pts); i++ {
		prev, curr := pts[i-1].value, pts[i].value
		if curr != prev && !(math.IsNaN(curr) && math.IsNaN(prev)) {
			count++
		}
	}
	return count
}

// dupRowResets is resets' reducer: adjacent pairs where the value decreased.
func dupRowResets(pts []dupRowPoint) float64 {
	count := 0.0
	for i := 1; i < len(pts); i++ {
		if pts[i].value < pts[i-1].value {
			count++
		}
	}
	return count
}

// dupRowLowering names one alternate strategy for a case's query, alongside
// the lowerer set that selects it. The DEFAULT (fan-out) lowering is always
// run; these are the arms that must agree with it.
type dupRowLowering struct {
	name     string
	lowerers promql.RangeLowerers
}

// dupRowCase is one range function under test.
type dupRowCase struct {
	// call is the PromQL call with `%s` for the metric name.
	call string
	// reduce defines the function's answer over a sample multiset.
	reduce func([]dupRowPoint) float64
	// immune declares that this reducer cannot see the duplicate collapse.
	// The runner recomputes that from the reducer itself and fails on
	// disagreement, so the declaration is an assertion, not a comment.
	immune bool
	// alternates are the non-default lowerings of this same query.
	alternates []dupRowLowering
}

// name renders a case's subtest name: the PromQL function it calls.
func (c dupRowCase) name() string {
	if idx := strings.IndexByte(c.call, '('); idx > 0 {
		return c.call[:idx]
	}
	return c.call
}

// query renders the case's full PromQL over this file's seed. Deliberately
// UNWRAPPED by any aggregation: the native timeSeries*ToGrid arms only engage
// over a plain Scan/Filter input (promql.nativeTSGridMatrixNode), so a `sum
// by(...)` wrapper would silently send every alternate back to the fan-out.
func (c dupRowCase) query() string { return fmt.Sprintf(c.call, dupRowMetric) }

// dupRowFanoutOnly is the default lowerer set — every strategy left at its
// fan-out default.
var dupRowFanoutOnly = promql.RangeLowerers{}

// dupRowFamily is the closed remainder cerberus issue #2927 enumerated from
// chsql's emitRangeWindow dispatch, plus the two functions the rule now
// declares for consistency and which are immune to it.
//
// Each entry lists every alternate lowering the function HAS, so a strategy
// that stops agreeing with the fan-out fails here rather than in production.
var dupRowFamily = []dupRowCase{
	{
		call:   "irate(%s[5m])",
		reduce: dupRowIrate,
		alternates: []dupRowLowering{
			{name: "lag-adjacency", lowerers: promql.RangeLowerers{
				Irate: promql.LagAdjacencyIrateLowerer{Fallback: promql.FanoutIrateLowerer{}},
			}},
			{name: "native-grid", lowerers: promql.RangeLowerers{
				Irate: promql.NativeIrateLowerer{Fallback: promql.FanoutIrateLowerer{}},
			}},
		},
	},
	{
		call:   "idelta(%s[5m])",
		reduce: dupRowIdelta,
		alternates: []dupRowLowering{
			{name: "lag-adjacency", lowerers: promql.RangeLowerers{
				Idelta: promql.LagAdjacencyIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}},
			}},
			{name: "native-grid", lowerers: promql.RangeLowerers{
				Idelta: promql.NativeIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}},
			}},
		},
	},
	{
		call:   "deriv(%s[5m])",
		reduce: dupRowDeriv,
		alternates: []dupRowLowering{
			{name: "native-grid", lowerers: promql.RangeLowerers{
				Deriv: promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}},
			}},
		},
	},
	{
		call:   fmt.Sprintf("predict_linear(%%s[5m], %g)", dupRowPredictHorizon),
		reduce: dupRowPredictLinear,
		alternates: []dupRowLowering{
			{name: "native-grid", lowerers: promql.RangeLowerers{
				PredictLinear: promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}},
			}},
		},
	},
	{
		call: fmt.Sprintf("double_exponential_smoothing(%%s[5m], %g, %g)",
			dupRowSmoothingFactor, dupRowTrendFactor),
		reduce: dupRowDoubleExponentialSmoothing,
	},
	{
		call:   "changes(%s[5m])",
		reduce: dupRowChanges,
		immune: true,
		alternates: []dupRowLowering{
			{name: "lag-adjacency", lowerers: promql.RangeLowerers{
				Changes: promql.LagAdjacencyChangesLowerer{Fallback: promql.FanoutChangesLowerer{}},
			}},
			{name: "native-grid", lowerers: promql.RangeLowerers{
				Changes: promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}},
			}},
		},
	},
	{
		call:   "resets(%s[5m])",
		reduce: dupRowResets,
		immune: true,
		alternates: []dupRowLowering{
			{name: "lag-adjacency", lowerers: promql.RangeLowerers{
				Resets: promql.LagAdjacencyResetsLowerer{Fallback: promql.FanoutResetsLowerer{}},
			}},
			{name: "native-grid", lowerers: promql.RangeLowerers{
				Resets: promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}},
			}},
		},
	},
}

// assertDupRowContract pins one executed answer against the contract window's
// arithmetic, against the both-rows window's, and against the case's own
// immunity declaration.
func assertDupRowContract(t *testing.T, c dupRowCase, got float64, what string) {
	t.Helper()
	contract := c.reduce(dupRowContractWindow)
	bothRows := c.reduce(dupRowBothRowsWindow)
	coincide := dupTSAnswersAgree(contract, bothRows)
	if coincide != c.immune {
		t.Fatalf("%s: case declares immune=%v, but the contract answer %v and the both-rows answer "+
			"%v %s — the declaration and the arithmetic disagree", what, c.immune, contract, bothRows,
			map[bool]string{true: "coincide", false: "differ"}[coincide])
	}
	if math.IsNaN(contract) {
		t.Fatalf("%s: the contract answer is NaN, so this case pins nothing", what)
	}
	if !dupTSAnswersAgree(got, contract) {
		t.Errorf("%s = %v, want %v (the duplicated (timestamp, value) row counted ONCE, as "+
			"Prometheus's own appender stores it)", what, got, contract)
	}
	if !c.immune && dupTSAnswersAgree(got, bothRows) {
		t.Errorf("%s = %v, which is the answer over BOTH stored rows — the duplicate at 00:05:00 "+
			"was counted twice, the divergence cerberus issue #2927 exists to close", what, got)
	}
	if math.IsNaN(got) {
		t.Errorf("%s = NaN, so the series is dropped from the answer entirely rather than merely "+
			"answering a wrong number", what)
	}
}

// dupRowAnswerAt executes query under lowerers and returns the single series'
// value at the final anchor.
func dupRowAnswerAt(t *testing.T, fixture *chdbFixture, query string, lowerers promql.RangeLowerers) dupTSAnswer {
	t.Helper()
	return runDupTSQuery(t, fixture, query, dupTSIdeltaStep, lowerers)
}

// TestDuplicateSampleRow_RangeFamilyCountsItOnce executes every member of the
// remainder on the DEFAULT fan-out lowering and pins each answer to the
// distinct-(timestamp, value) contract.
func TestDuplicateSampleRow_RangeFamilyCountsItOnce(t *testing.T) {
	fixture := newChDBFixture(t, dupRowSeed)
	for _, c := range dupRowFamily {
		t.Run(c.name(), func(t *testing.T) {
			answer := dupRowAnswerAt(t, fixture, c.query(), dupRowFanoutOnly)
			assertDupRowContract(t, c,
				dupTSValueAt(t, answer, dupTSHostAAttributes, dupTSFinalAnchor),
				"fan-out "+c.name()+" at the final anchor")
		})
	}
}

// TestDuplicateSampleRow_EveryLoweringAgrees runs each member through every
// alternate lowering it has and asserts all of them land on the SAME contract
// as the fan-out — one rule across every lowering of one query, not a
// per-path patch.
//
// The native timeSeries*ToGrid arms need ClickHouse's experimental
// time-series aggregates enabled; in production the engine layer puts that
// setting on the per-query context (see chsql's emitRangeWindowGridNative),
// and here it is set on the shared session directly.
func TestDuplicateSampleRow_EveryLoweringAgrees(t *testing.T) {
	fixture := newChDBFixture(t, dupRowSeed)
	if _, err := fixture.db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable the experimental time-series aggregates: %v", err)
	}
	for _, c := range dupRowFamily {
		for _, alt := range c.alternates {
			t.Run(c.name()+"/"+alt.name, func(t *testing.T) {
				fanout := dupRowAnswerAt(t, fixture, c.query(), dupRowFanoutOnly)
				alternate := dupRowAnswerAt(t, fixture, c.query(), alt.lowerers)
				assertDupTSStrategiesAgree(t, alternate, fanout)
				assertDupRowContract(t, c,
					dupTSValueAt(t, alternate, dupTSHostAAttributes, dupTSFinalAnchor),
					alt.name+" "+c.name()+" at the final anchor")
			})
		}
	}
}
