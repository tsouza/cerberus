//go:build chdb

// chDB-backed differential proof of cerberus's duplicate-timestamp contract
// (cerberus issue #2905): over a seed carrying two samples at ONE
// (series, timestamp) with DIFFERENT values, a ClickHouse-native range
// strategy must answer exactly what the fan-out array-fold strategy answers.
//
// # Why this is the right oracle, and a reference engine is not
//
// Three TXTAR fixtures seed such a duplicate on purpose, to pin how their
// strategy treats the pair: sorted_slab_sum_over_time_range_step.txtar,
// sorted_slab_avg_over_time_range_step.txtar and
// lag_adjacency_idelta_duplicate_ts.txtar. All three used to also enrol for
// parity against the upstream Prometheus engine, and that enrolment was
// self-contradictory: Prometheus's TSDB appender stores at most one sample per
// (series, timestamp) — parityoracle/promql/oracle.go feeds a real teststorage
// head, whose commit drops the second — so the reference answers over a seed
// cerberus never saw, with a survivor chosen by ingestion order. Agreement
// would have been an accident, and disagreement was: the `roundtrip-promql`
// lane went red on a divergence that was exactly the discarded sample.
//
// The three fixtures are parity-EXEMPT now (reason `duplicate-timestamp-seed`,
// test/spec/parity_exempt.go), and this file is where the contract they exist
// for is actually tested. It is a sharper oracle than a reference backend for
// this one question: it compares the strategy under test against the fan-out
// strategy it must agree with, over the identical rows, rather than against an
// engine that discarded one of them before evaluating anything.
//
// # Why each case also pins a hand-derived value
//
// Two strategies agreeing proves they agree, not that they are right — and the
// specific way they could BOTH be wrong here is by both deduplicating, which
// is what the rate family does by design (increase_duplicate_timestamp_dedup
// .txtar pins that). So every case additionally asserts the no-dedup answer it
// must produce AND asserts that this differs from the deduplicated answer the
// reference engine computes. Without that second assertion the differential
// would keep passing the day the contract silently inverted.
package promql_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// dupTSMetricsDDL creates both arms of the metrics fan-out a bare selector
// resolves to, so the emitted `merge(...)` reads a complete catalog whichever
// arm a given metric name routes to. `CREATE OR REPLACE` because the chDB
// session outlives any one test — see chdbFixture's own doc.
const dupTSMetricsDDL = `
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

// The metric names the cases query. Distinct from every other fixture sharing
// this package's chDB session (fixture_chdb_test.go), and distinct from the
// TXTAR fixtures' own names for the same reason.
const (
	dupTSOverTimeMetric = "duplicate_ts_over_time_test_metric"
	dupTSIdeltaMetric   = "duplicate_ts_idelta_test_metric"
)

// dupTSOverTimeSeed is sorted_slab_{sum,avg}_over_time_range_step.txtar's own
// seed: job 'api' carries 00:02:00 twice with 2.0 and 3.0, job 'web' is a
// plain monotonic control that no duplicate touches.
const dupTSOverTimeSeed = dupTSMetricsDDL + `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 3.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 8.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 6.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:01:00', 9), 20.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('` + dupTSOverTimeMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:05:00', 9), 60.0);
`

// dupTSIdeltaSeed is lag_adjacency_idelta_duplicate_ts.txtar's own seed:
// host 'a' carries 00:02:00 twice with 2.0 and 5.0.
const dupTSIdeltaSeed = dupTSMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('` + dupTSIdeltaMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),
    ('` + dupTSIdeltaMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('` + dupTSIdeltaMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 5.0),
    ('` + dupTSIdeltaMetric + `', map('host', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 10.0);
`

// The evaluation window every case shares, matching the TXTAR fixtures'
// deterministic range anchor (internal/promql/lower_test.go).
var (
	dupTSRangeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dupTSRangeEnd   = dupTSRangeStart.Add(5 * time.Minute)
)

// The two range steps the fixtures use: 30s for the over_time pair, 5m for
// idelta (whose single anchor is the range end).
const (
	dupTSOverTimeStep = 30 * time.Second
	dupTSIdeltaStep   = 5 * time.Minute
)

// dupTSFloatTolerance absorbs ordinary float64 representation error between
// two constructions that sum the same values in a different order (arrayAvg
// over a slab versus the fan-out fold). It is many orders above nothing and
// many orders below every gap this file asserts on: the smallest is
// avg_over_time's 25/6 vs 22/5, a gap of ~0.23.
const dupTSFloatTolerance = 1e-9

// dupTSSample is one output row, keyed for cross-strategy comparison.
type dupTSSample struct {
	attributes string
	timestamp  string
	value      float64
}

// dupTSAnswer is one strategy's whole answer plus the SQL that produced it.
// The SQL is carried so the differential can prove the two strategies were
// actually DIFFERENT — see [assertDupTSStrategiesAgree].
type dupTSAnswer struct {
	samples []dupTSSample
	sql     string
}

// runDupTSQuery lowers query under lowerers, executes the emitted SQL against
// fixture and returns every output row in the emitted order.
func runDupTSQuery(
	t *testing.T, fixture *chdbFixture, query string, step time.Duration, lowerers promql.RangeLowerers,
) dupTSAnswer {
	t.Helper()
	// EnableExperimentalFunctions so the family table can cover
	// mad_over_time, which upstream still gates behind that flag. It changes
	// nothing for the non-experimental calls.
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(
		context.Background(), expr, schema.DefaultOTelMetrics(),
		dupTSRangeStart, dupTSRangeEnd, step, promql.LowerOpts{Lowerers: lowerers},
	)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", query, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	// Projected to plain strings: chdb-go's parquet driver cannot decode a
	// Map(String, String) cell into a Go destination — see
	// chdbFixture.queryOverEmitted's own doc.
	rows := fixture.queryOverEmitted(t,
		"toString(Attributes), toString(TimeUnix), toString(Value)", sqlText, args)
	defer func() { _ = rows.Close() }()

	var out []dupTSSample
	for rows.Next() {
		var attributes, timestamp, value string
		if err := rows.Scan(&attributes, &timestamp, &value); err != nil {
			t.Fatalf("scan(%q): %v", query, err)
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("parse Value %q (%q): %v", value, query, err)
		}
		out = append(out, dupTSSample{attributes: attributes, timestamp: timestamp, value: parsed})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err(%q): %v", query, err)
	}
	if len(out) == 0 {
		t.Fatalf("query %q returned no rows; the differential would compare two empty answers", query)
	}
	return dupTSAnswer{samples: out, sql: sqlText}
}

// assertDupTSStrategiesAgree pins the differential itself: the native strategy
// and the fan-out strategy answer identically, row for row, over a seed whose
// duplicate is the only thing that could separate them.
//
// It first asserts the two EMITTED STATEMENTS differ. Without that, a native
// strategy that silently declined this shape and chained to its own Fallback
// would make both legs run the identical SQL, and the comparison would pass by
// comparing an answer with itself — the hollow green a differential is
// supposed to be immune to.
func assertDupTSStrategiesAgree(t *testing.T, nativeAnswer, fanoutAnswer dupTSAnswer) {
	t.Helper()
	if nativeAnswer.sql == fanoutAnswer.sql {
		t.Fatalf("both strategies emitted the SAME SQL, so this compares an answer with itself; "+
			"the native strategy declined this shape and fell back:\n%s", nativeAnswer.sql)
	}
	native, fanout := nativeAnswer.samples, fanoutAnswer.samples
	if len(native) != len(fanout) {
		t.Fatalf("native returned %d row(s), fan-out returned %d", len(native), len(fanout))
	}
	for i := range native {
		n, f := native[i], fanout[i]
		if n.attributes != f.attributes || n.timestamp != f.timestamp {
			t.Fatalf("row %d: native is (%s, %s), fan-out is (%s, %s)",
				i, n.attributes, n.timestamp, f.attributes, f.timestamp)
		}
		if math.Abs(n.value-f.value) > dupTSFloatTolerance {
			t.Errorf("row %d (%s @ %s): native = %v, fan-out = %v; the two strategies must treat a "+
				"duplicate (series, timestamp) identically", i, n.attributes, n.timestamp, n.value, f.value)
		}
	}
}

// dupTSValueAt returns the value of the single row matching attributes at the
// LAST timestamp both strategies produced for it.
func dupTSValueAt(t *testing.T, answer dupTSAnswer, attributes, timestamp string) float64 {
	t.Helper()
	for _, s := range answer.samples {
		if s.attributes == attributes && s.timestamp == timestamp {
			return s.value
		}
	}
	t.Fatalf("no row for %s @ %s in %v", attributes, timestamp, answer.samples)
	return 0
}

// assertNoDedup pins the CONTRACT rather than the agreement: the answer is the
// one that counts every seeded sample, and it is NOT the one a deduplicating
// path (or the reference engine's appender, which keeps a single sample per
// timestamp) would produce.
func assertNoDedup(t *testing.T, got, wantNoDedup, dedupedWouldBe float64, what string) {
	t.Helper()
	if math.Abs(got-wantNoDedup) > dupTSFloatTolerance {
		t.Errorf("%s = %v, want %v (every seeded sample counted)", what, got, wantNoDedup)
	}
	if math.Abs(wantNoDedup-dedupedWouldBe) <= dupTSFloatTolerance {
		t.Fatalf("%s: the no-dedup answer %v and the deduplicated answer %v are indistinguishable, "+
			"so this case pins nothing about the contract", what, wantNoDedup, dedupedWouldBe)
	}
	if math.Abs(got-dedupedWouldBe) <= dupTSFloatTolerance {
		t.Errorf("%s = %v, which is the DEDUPLICATED answer — the duplicate at 00:02:00 was dropped, "+
			"inverting the contract this seed exists to pin", what, got)
	}
}

// The window (00:00:00, 00:05:00] over job 'api' holds 5.0, 2.0, 3.0, 8.0,
// 1.0, 6.0 with every sample counted, and loses 3.0 under deduplication.
const (
	dupTSAPISumNoDedup   = 25.0
	dupTSAPISumDeduped   = 22.0
	dupTSAPICountNoDedup = 6.0
	dupTSAPICountDeduped = 5.0
)

// dupTSAPIAttributes is how the emitted `sum by(job)` / `avg by(job)`
// projection renders job 'api', and dupTSHostAAttributes how the ungrouped
// idelta projection renders host 'a'.
const (
	dupTSAPIAttributes   = `{'job':'api'}`
	dupTSHostAAttributes = `{'host':'a'}`
)

// dupTSFinalAnchor is the range end, the anchor whose 5m window covers every
// seeded sample including the duplicate.
const dupTSFinalAnchor = "2026-01-01 00:05:00.000000000"

// TestDuplicateTimestampSeed_SumOverTimeSortedSlabMatchesFanout is
// sorted_slab_sum_over_time_range_step.txtar's contract, executed: the
// sorted-slab reducer and the fan-out array fold must sum the SAME six
// samples, duplicate included.
func TestDuplicateTimestampSeed_SumOverTimeSortedSlabMatchesFanout(t *testing.T) {
	fixture := newChDBFixture(t, dupTSOverTimeSeed)
	query := "sum by(job) (sum_over_time(" + dupTSOverTimeMetric + "[5m]))"

	native := runDupTSQuery(t, fixture, query, dupTSOverTimeStep, promql.RangeLowerers{
		OverTime: promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}},
	})
	fanout := runDupTSQuery(t, fixture, query, dupTSOverTimeStep, promql.RangeLowerers{})

	assertDupTSStrategiesAgree(t, native, fanout)
	assertNoDedup(t,
		dupTSValueAt(t, native, dupTSAPIAttributes, dupTSFinalAnchor),
		dupTSAPISumNoDedup, dupTSAPISumDeduped,
		"sorted-slab sum_over_time at the final anchor")
}

// TestDuplicateTimestampSeed_AvgOverTimeSortedSlabMatchesFanout is
// sorted_slab_avg_over_time_range_step.txtar's contract: the duplicate counts
// in the DIVISOR too, so the answer is 25/6 rather than the reference
// engine's 22/5.
func TestDuplicateTimestampSeed_AvgOverTimeSortedSlabMatchesFanout(t *testing.T) {
	fixture := newChDBFixture(t, dupTSOverTimeSeed)
	query := "avg by(job) (avg_over_time(" + dupTSOverTimeMetric + "[5m]))"

	native := runDupTSQuery(t, fixture, query, dupTSOverTimeStep, promql.RangeLowerers{
		OverTime: promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}},
	})
	fanout := runDupTSQuery(t, fixture, query, dupTSOverTimeStep, promql.RangeLowerers{})

	assertDupTSStrategiesAgree(t, native, fanout)
	assertNoDedup(t,
		dupTSValueAt(t, native, dupTSAPIAttributes, dupTSFinalAnchor),
		dupTSAPISumNoDedup/dupTSAPICountNoDedup, dupTSAPISumDeduped/dupTSAPICountDeduped,
		"sorted-slab avg_over_time at the final anchor")
}

// TestDuplicateTimestampSeed_IdeltaLagAdjacencyMatchesFanout is
// lag_adjacency_idelta_duplicate_ts.txtar's contract: the lagInFrame survivor
// flag must select the SAME duplicate the array-fold path's
// arraySort(groupArray((ts, value))) tuple order selects — the max-valued one
// — so idelta is 10.0 - 5.0 and not the reference engine's 10.0 - 2.0.
func TestDuplicateTimestampSeed_IdeltaLagAdjacencyMatchesFanout(t *testing.T) {
	fixture := newChDBFixture(t, dupTSIdeltaSeed)
	query := "idelta(" + dupTSIdeltaMetric + "[5m])"

	native := runDupTSQuery(t, fixture, query, dupTSIdeltaStep, promql.RangeLowerers{
		Idelta: promql.LagAdjacencyIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}},
	})
	fanout := runDupTSQuery(t, fixture, query, dupTSIdeltaStep, promql.RangeLowerers{})

	assertDupTSStrategiesAgree(t, native, fanout)

	// The last two samples in the window are 5.0 (the max-valued duplicate at
	// 00:02:00) and 10.0 at 00:04:00. Deduplicating to the first-ingested 2.0
	// makes the pair (2.0, 10.0) instead.
	const (
		lastSample               = 10.0
		survivingDuplicate       = 5.0
		firstIngestedDuplicate   = 2.0
		ideltaNoDedup            = lastSample - survivingDuplicate
		ideltaIfDuplicateDeduped = lastSample - firstIngestedDuplicate
	)
	assertNoDedup(t,
		dupTSValueAt(t, native, dupTSHostAAttributes, dupTSFinalAnchor),
		ideltaNoDedup, ideltaIfDuplicateDeduped,
		"lag-adjacency idelta at the final anchor")
}

// ---------------------------------------------------------------------------
// The IDENTICAL-value duplicate — cerberus issue #2914
// ---------------------------------------------------------------------------
//
// Everything above seeds a duplicate whose two rows carry DIFFERENT values.
// There, "which value survives" is implementation-defined on both sides, so
// only cerberus's own deterministic tie-break can be pinned and the fixtures
// are parity-exempt.
//
// This section seeds the other shape: two rows at one (series, timestamp)
// carrying the SAME value. No survivor question arises, so the answer is
// determinate and both engines can agree on it — Prometheus's TSDB stores at
// most one sample per (series, timestamp), so the window holds one logical
// sample there, not two.
//
// That is the same contract PR #1092 already gave the rate family, via
// chsql's dedupWindowPairsByTsFrag: one sample per distinct timestamp, the
// max-valued one where the rows disagree. These cases pin that the
// *_over_time family answers under the SAME contract rather than a second,
// contradictory one — and pin, per function, which reducers the collapse can
// move at all.

// dupTSIdenticalMetric is the identical-value seed's own metric name, distinct
// from every other name sharing this package's chDB session.
const dupTSIdenticalMetric = "duplicate_ts_identical_over_time_test_metric"

// dupTSIdenticalSeed is dupTSOverTimeSeed's shape with the 00:02:00 duplicate
// carrying 2.0 TWICE instead of 2.0 and 3.0.
const dupTSIdenticalSeed = dupTSMetricsDDL + `
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 8.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 6.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:01:00', 9), 20.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('` + dupTSIdenticalMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:05:00', 9), 60.0);
`

// The two candidate sample multisets for job 'api' over the final anchor's
// (00:00:00, 00:05:00] window, both in ascending timestamp order:
//
//	dupTSContractWindow — the contract's window: one sample per distinct
//	  timestamp, so the 00:02:00 pair contributes a single 2.0. This is also
//	  exactly what Prometheus's own head storage would hold for this seed.
//	dupTSBothRowsWindow — the window cerberus's *_over_time family built
//	  before this issue: both stored ROWS, so 2.0 appears twice.
//
// Each case below reduces both with the same Go reducer, which makes the
// expectation a property of the SAMPLE SET rather than a transcription of any
// SQL. A function whose two reductions coincide is inherently immune to the
// collapse; the table declares that per function and the runner checks the
// declaration against the arithmetic, so neither can drift alone.
var (
	dupTSContractWindow = []float64{5, 2, 8, 1, 6}
	dupTSBothRowsWindow = []float64{5, 2, 2, 8, 1, 6}
)

// dupTSMedianPhi is the quantile the quantile_over_time and mad_over_time
// cases probe with — the median, where a one-element change in an even/odd
// -length window moves the answer the most visibly.
const dupTSMedianPhi = 0.5

func dupTSSum(vals []float64) float64 {
	total := 0.0
	for _, v := range vals {
		total += v
	}
	return total
}

func dupTSCount(vals []float64) float64 { return float64(len(vals)) }

func dupTSMean(vals []float64) float64 { return dupTSSum(vals) / dupTSCount(vals) }

func dupTSMin(vals []float64) float64 {
	out := vals[0]
	for _, v := range vals[1:] {
		out = math.Min(out, v)
	}
	return out
}

func dupTSMax(vals []float64) float64 {
	out := vals[0]
	for _, v := range vals[1:] {
		out = math.Max(out, v)
	}
	return out
}

// dupTSLast reads the time-LATEST sample: the slices are in ascending
// timestamp order, so that is the final element.
func dupTSLast(vals []float64) float64 { return vals[len(vals)-1] }

// dupTSPresent is present_over_time's reducer: 1 for any non-empty window.
func dupTSPresent([]float64) float64 { return 1 }

// dupTSPopVariance is the POPULATION variance (divisor N, not N-1) — the
// definition PromQL's stdvar_over_time uses.
func dupTSPopVariance(vals []float64) float64 {
	mean := dupTSMean(vals)
	total := 0.0
	for _, v := range vals {
		total += (v - mean) * (v - mean)
	}
	return total / dupTSCount(vals)
}

func dupTSPopStddev(vals []float64) float64 { return math.Sqrt(dupTSPopVariance(vals)) }

// dupTSQuantileInclusive is the linearly-interpolating quantile ClickHouse's
// quantileExactInclusive and Prometheus's own quantile_over_time both compute:
// rank phi*(N-1) into the ascending-sorted values, interpolating between the
// two neighbouring ranks.
func dupTSQuantileInclusive(vals []float64, phi float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	rank := phi * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rank-float64(lo))
}

func dupTSMedian(vals []float64) float64 { return dupTSQuantileInclusive(vals, dupTSMedianPhi) }

// dupTSMAD is mad_over_time: the median of the absolute deviations from the
// window's own median.
func dupTSMAD(vals []float64) float64 {
	median := dupTSMedian(vals)
	deviations := make([]float64, len(vals))
	for i, v := range vals {
		deviations[i] = math.Abs(v - median)
	}
	return dupTSMedian(deviations)
}

// dupTSOverTimeCase is one *_over_time function under test: the PromQL call
// (with `%s` for the metric name), the Go reducer that defines its answer
// over a sample multiset, and whether collapsing the duplicate is expected to
// leave the answer untouched.
type dupTSOverTimeCase struct {
	call   string
	reduce func([]float64) float64
	// immune declares that this reducer cannot see the duplicate collapse.
	// The runner recomputes that from the reducer and fails on disagreement,
	// so the declaration is an assertion rather than a comment.
	immune bool
}

// dupTSOverTimeFamily is the whole *_over_time family, each with the reducer
// that defines its answer. The immune ones are exactly the order statistics
// and the presence flag: repeating a value that is already in the multiset
// cannot move a minimum, a maximum, the time-latest sample, or "is the window
// non-empty". Every counting, summing, averaging or rank-based reducer can.
var dupTSOverTimeFamily = []dupTSOverTimeCase{
	{call: "sum_over_time(%s[5m])", reduce: dupTSSum},
	{call: "avg_over_time(%s[5m])", reduce: dupTSMean},
	{call: "count_over_time(%s[5m])", reduce: dupTSCount},
	{call: "stddev_over_time(%s[5m])", reduce: dupTSPopStddev},
	{call: "stdvar_over_time(%s[5m])", reduce: dupTSPopVariance},
	{call: "quantile_over_time(0.5, %s[5m])", reduce: dupTSMedian},
	{call: "mad_over_time(%s[5m])", reduce: dupTSMAD},
	{call: "min_over_time(%s[5m])", reduce: dupTSMin, immune: true},
	{call: "max_over_time(%s[5m])", reduce: dupTSMax, immune: true},
	{call: "last_over_time(%s[5m])", reduce: dupTSLast, immune: true},
	{call: "present_over_time(%s[5m])", reduce: dupTSPresent, immune: true},
}

// dupTSCaseName renders a case's subtest name: the PromQL function it calls.
func (c dupTSOverTimeCase) name() string {
	if idx := strings.IndexByte(c.call, '('); idx > 0 {
		return c.call[:idx]
	}
	return c.call
}

// query renders the case's full PromQL, wrapped in `sum by(job)` so the
// emitted projection carries the same grouped Attributes shape the
// differing-value cases above read. The wrap is an identity for this seed:
// each job holds exactly one series.
func (c dupTSOverTimeCase) query() string {
	return "sum by(job) (" + fmt.Sprintf(c.call, dupTSIdenticalMetric) + ")"
}

// assertIdenticalDuplicateContract pins one function's answer against the
// contract window, and — for a reducer the collapse can move — additionally
// pins that the answer is NOT the both-rows one. The second assertion is what
// makes the case fail on a lowering that counts the duplicated row twice.
func assertIdenticalDuplicateContract(t *testing.T, c dupTSOverTimeCase, got float64, what string) {
	t.Helper()
	wantContract := c.reduce(dupTSContractWindow)
	bothRows := c.reduce(dupTSBothRowsWindow)
	coincide := math.Abs(wantContract-bothRows) <= dupTSFloatTolerance
	if coincide != c.immune {
		t.Fatalf("%s: case declares immune=%v, but the contract answer %v and the both-rows "+
			"answer %v %s — the declaration and the arithmetic disagree",
			what, c.immune, wantContract, bothRows,
			map[bool]string{true: "coincide", false: "differ"}[coincide])
	}
	if math.Abs(got-wantContract) > dupTSFloatTolerance {
		t.Errorf("%s = %v, want %v (the duplicated (series, timestamp) counted ONCE, as "+
			"Prometheus stores it and as the rate family already counts it)", what, got, wantContract)
	}
	if !c.immune && math.Abs(got-bothRows) <= dupTSFloatTolerance {
		t.Errorf("%s = %v, which is the answer over BOTH stored rows — the duplicate at 00:02:00 "+
			"was counted twice, the divergence cerberus issue #2914 exists to close", what, got)
	}
}

// TestIdenticalDuplicateTimestamp_OverTimeFamilyCountsItOnce runs the whole
// *_over_time family over the identical-value seed on the DEFAULT fan-out
// lowering and pins each answer to the one-sample-per-timestamp contract.
func TestIdenticalDuplicateTimestamp_OverTimeFamilyCountsItOnce(t *testing.T) {
	fixture := newChDBFixture(t, dupTSIdenticalSeed)
	for _, c := range dupTSOverTimeFamily {
		t.Run(c.name(), func(t *testing.T) {
			answer := runDupTSQuery(t, fixture, c.query(), dupTSOverTimeStep, promql.RangeLowerers{})
			assertIdenticalDuplicateContract(t, c,
				dupTSValueAt(t, answer, dupTSAPIAttributes, dupTSFinalAnchor),
				"fan-out "+c.name()+" at the final anchor")
		})
	}
}

// dupTSSortedSlabEligible is the set of *_over_time functions
// promql.SortedSlabOverTimeLowerer actually decomposes — its scope is
// sum_over_time / avg_over_time (internal/chsql/range_window_sorted_slab.go).
// The cross-strategy test asserts the emitted SQL differs for EXACTLY these
// and for no others, so an eligibility change cannot quietly turn a
// cross-strategy comparison into a comparison of an answer with itself.
var dupTSSortedSlabEligible = map[string]bool{
	"sum_over_time": true,
	"avg_over_time": true,
}

// TestIdenticalDuplicateTimestamp_EveryLoweringAgrees runs the same family
// through the sorted-slab strategy as well, and asserts that wherever that
// strategy actually engages it lands on the SAME contract as the fan-out fold
// — so the fix is one contract across every lowering of one query, not a
// per-path patch.
func TestIdenticalDuplicateTimestamp_EveryLoweringAgrees(t *testing.T) {
	fixture := newChDBFixture(t, dupTSIdenticalSeed)
	for _, c := range dupTSOverTimeFamily {
		t.Run(c.name(), func(t *testing.T) {
			slab := runDupTSQuery(t, fixture, c.query(), dupTSOverTimeStep, promql.RangeLowerers{
				OverTime: promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}},
			})
			fanout := runDupTSQuery(t, fixture, c.query(), dupTSOverTimeStep, promql.RangeLowerers{})

			engaged := slab.sql != fanout.sql
			if engaged != dupTSSortedSlabEligible[c.name()] {
				t.Fatalf("sorted-slab %s: emitted %s SQL than the fan-out, but the eligible set says "+
					"engaged=%v; update dupTSSortedSlabEligible or the eligibility predicate",
					c.name(), map[bool]string{true: "different", false: "identical"}[engaged],
					dupTSSortedSlabEligible[c.name()])
			}
			if !engaged {
				return
			}
			assertDupTSStrategiesAgree(t, slab, fanout)
			assertIdenticalDuplicateContract(t, c,
				dupTSValueAt(t, slab, dupTSAPIAttributes, dupTSFinalAnchor),
				"sorted-slab "+c.name()+" at the final anchor")
		})
	}
}

// ---------------------------------------------------------------------------
// The count-shaped lowering's timestamp binding — cerberus issue #2914
// ---------------------------------------------------------------------------
//
// count_over_time is the one *_over_time member the direct-aggregate fast path
// renders WITHOUT materialising a sample array (chsql's overTimeDirectAggFrag),
// and the only one whose aggregate reads the sample TIMESTAMP once the
// duplicate-row contract is in force: it counts distinct (timestamp, value)
// pairs rather than stored rows.
//
// That makes it the one member exposed to a silent binding hazard in the
// matrix fan-out. chsql's fanoutTsSource renames the per-sample timestamp
// column to `_src_ts` for a NESTED matrix shape — where the input relation
// already publishes its timestamps under the fan-out's own `anchor_ts` output
// alias — and in exactly that shape `anchor_ts` ALSO survives one subquery up
// as the regroup layer's own GROUP BY key. An aggregate built against the
// pre-rename name therefore resolves to the grouping key rather than to the
// sample's own timestamp, and ClickHouse raises no error at all: the count
// silently degrades to the number of distinct VALUES in the group.
//
// The case below is that nested shape, executed. It carries TWO series chosen
// so the degraded answer is a different wrong number in each, which is what
// makes the pin discriminating rather than a single equality that several
// mistakes could satisfy.

// dupTSNestedMetric is the nested-matrix case's own metric name, distinct from
// every other name sharing this package's chDB session.
const dupTSNestedMetric = "duplicate_ts_nested_matrix_test_metric"

// dupTSNestedSeed samples two series every minute:
//
//	job 'api' — 1, 5, 5, 7, 7, 9. Its inner per-minute maxima repeat a value
//	  at two distinct anchors, so losing the per-sample timestamp merges those
//	  two and answers 2.
//	job 'web' — a flat 4. Every inner maximum is the same value, so losing the
//	  per-sample timestamp merges ALL of them and answers 1 — the exact
//	  symptom observed on this hazard.
//
// Both series hold three inner anchors in the outer window, so the contract's
// answer is 3 for both and neither degraded answer can be reached by accident.
const dupTSNestedSeed = dupTSMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 5.0),
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 7.0),
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 7.0),
    ('` + dupTSNestedMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 9.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:00:00', 9), 4.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:01:00', 9), 4.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:02:00', 9), 4.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:03:00', 9), 4.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:04:00', 9), 4.0),
    ('` + dupTSNestedMetric + `', map('job', 'web'), toDateTime64('2026-01-01 00:05:00', 9), 4.0);
`

// dupTSNestedExpectedCount is the contract's answer for both series: the inner
// `max_over_time(m[1m:1m])` grid walks back from 00:05:00 in 1m steps, so the
// inner anchors inside the outer `[3m:…]` window (00:02:00, 00:05:00] are
// 00:03:00, 00:04:00 and 00:05:00 — three samples at three distinct
// timestamps, whatever values they carry.
const dupTSNestedExpectedCount = 3.0

// dupTSWebAttributes is how the emitted ungrouped projection renders job 'web'
// — dupTSAPIAttributes' sibling for the nested case's second series.
const dupTSWebAttributes = `{'job':'web'}`

// dupTSNestedDegradedCount maps each series to the answer it produces when the
// count loses the per-sample timestamp and degrades to counting distinct
// values: two for 'api' (7, 7, 9 → 7 and 9) and one for 'web' (4, 4, 4 → 4).
var dupTSNestedDegradedCount = map[string]float64{
	dupTSAPIAttributes: 2,
	dupTSWebAttributes: 1,
}

// TestNestedMatrixCountOverTime_CountsSamplesNotItsGroupingKey executes the
// nested-matrix count_over_time shape and pins both series against the
// contract's answer and against their own degraded one.
func TestNestedMatrixCountOverTime_CountsSamplesNotItsGroupingKey(t *testing.T) {
	fixture := newChDBFixture(t, dupTSNestedSeed)
	query := "count_over_time(max_over_time(" + dupTSNestedMetric + "[1m:1m])[3m:1m])"

	answer := runDupTSQuery(t, fixture, query, dupTSOverTimeStep, promql.RangeLowerers{})
	for _, attributes := range []string{dupTSAPIAttributes, dupTSWebAttributes} {
		degraded := dupTSNestedDegradedCount[attributes]
		if degraded == dupTSNestedExpectedCount {
			t.Fatalf("%s: the contract answer and the degraded answer are both %v, so this "+
				"series pins nothing about the timestamp binding", attributes, degraded)
		}
		got := dupTSValueAt(t, answer, attributes, dupTSFinalAnchor)
		if got != dupTSNestedExpectedCount {
			t.Errorf("nested count_over_time for %s at the final anchor = %v, want %v",
				attributes, got, dupTSNestedExpectedCount)
		}
		if got == degraded {
			t.Errorf("nested count_over_time for %s at the final anchor = %v — the count lost the "+
				"per-sample timestamp and fell back to counting distinct VALUES, which is what "+
				"binding it to the regroup's own anchor_ts grouping key produces", attributes, got)
		}
	}
}

// ---------------------------------------------------------------------------
// The subquery INTERIOR — cerberus issue #2914
// ---------------------------------------------------------------------------
//
// A PromQL subquery lowers to two stacked range windows, and only the OUTER
// one carries the duplicate-row contract. `sum_over_time(m[5m:1m])` builds a
// sum_over_time window (DistinctSampleRows set) over an `Identity` window
// (chplan.RangeWindow.Identity, Func empty, DistinctSampleRows unset) — and
// it is the INTERIOR that aggregates raw rows keyed by the schema timestamp
// column, which is exactly where a duplicate (series, timestamp) lives. The
// emitted SQL shows the asymmetry plainly: the interior assembles a bare
// `arraySort(groupArray((TimeUnix, Value)))` while the outer assembles
// `arrayCompact(arraySort(groupArray((_src_ts, Value))))`.
//
// That asymmetry is deliberate and it is safe, for a structural reason: the
// interior is PromQL's subquery resampling step, which reports the value of
// its input AT each anchor. Its reducer is the time-latest sample of the
// anchor's window — chsql's lastWindowValOrNaNFrag,
// `window_vals[length(window_vals)]` over the arraySort-by-(ts, value) order
// — so it emits exactly ONE row per (series, anchor) whatever the window
// held, and repeating a value already present at the winning timestamp
// cannot change which value that is. A duplicated raw row therefore cannot
// reach the outer window as a second sample; there is nothing there for the
// outer collapse to have missed.
//
// TestIdenticalDuplicateTimestamp_EveryLoweringAgrees does NOT cover this: it
// compares two lowerings of one INSTANT-input query and never builds a
// subquery interior. This case is what makes the claim executed rather than
// argued.

// dupTSSubqueryMetric is the subquery-interior case's own metric name,
// distinct from every other name sharing this package's chDB session.
const dupTSSubqueryMetric = "duplicate_ts_subquery_interior_test_metric"

// dupTSSubquerySeedRows is the per-minute sample series both seeds below
// share; dupTSSubqueryDuplicatedRow is the extra row that makes one of them
// carry an identical duplicate at 00:02:00.
const (
	dupTSSubquerySeedRows = `
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 8.0),
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 6.0)`
	dupTSSubqueryDuplicatedRow = `,
    ('` + dupTSSubqueryMetric + `', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0)`
)

// dupTSSubquerySeedWithDuplicate / dupTSSubquerySeedWithout are the same
// series with and without the duplicated 00:02:00 row. They differ by exactly
// one stored row, which is what the differential below turns on.
const (
	dupTSSubquerySeedWithDuplicate = dupTSMetricsDDL + `INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES` +
		dupTSSubquerySeedRows + dupTSSubqueryDuplicatedRow + `;`
	dupTSSubquerySeedWithout = dupTSMetricsDDL + `INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES` +
		dupTSSubquerySeedRows + `;`
)

// TestSubqueryInterior_DuplicateRowCannotReachTheOuterWindow executes a
// subquery whose interior is the un-deduped Identity window and asserts that
// adding an identical duplicate raw row changes NOTHING about the answer.
//
// The two seeds are asserted to actually differ first, so the differential
// cannot pass by comparing two identical inputs — the hollow green a
// same-shape comparison is otherwise open to.
func TestSubqueryInterior_DuplicateRowCannotReachTheOuterWindow(t *testing.T) {
	if dupTSSubquerySeedWithDuplicate == dupTSSubquerySeedWithout {
		t.Fatal("the two seeds are identical, so this differential asserts nothing")
	}
	query := "sum_over_time(" + dupTSSubqueryMetric + "[5m:1m])"

	withDup := runDupTSQuery(t, newChDBFixture(t, dupTSSubquerySeedWithDuplicate),
		query, dupTSOverTimeStep, promql.RangeLowerers{})
	without := runDupTSQuery(t, newChDBFixture(t, dupTSSubquerySeedWithout),
		query, dupTSOverTimeStep, promql.RangeLowerers{})

	if len(withDup.samples) != len(without.samples) {
		t.Fatalf("the duplicated row changed the ROW COUNT: %d with it, %d without — it reached the "+
			"outer window as a second sample through the un-deduped subquery interior",
			len(withDup.samples), len(without.samples))
	}
	for i := range withDup.samples {
		d, w := withDup.samples[i], without.samples[i]
		if d.attributes != w.attributes || d.timestamp != w.timestamp {
			t.Fatalf("row %d: with the duplicate (%s, %s), without it (%s, %s)",
				i, d.attributes, d.timestamp, w.attributes, w.timestamp)
		}
		if math.Abs(d.value-w.value) > dupTSFloatTolerance {
			t.Errorf("row %d (%s @ %s): %v with the duplicated row, %v without it. The subquery "+
				"interior is the Identity resampling window and carries no duplicate-row collapse; "+
				"it is safe only because it reports one time-latest sample per anchor. A difference "+
				"here means that no longer holds and the interior needs the contract too",
				i, d.attributes, d.timestamp, d.value, w.value)
		}
	}
}
