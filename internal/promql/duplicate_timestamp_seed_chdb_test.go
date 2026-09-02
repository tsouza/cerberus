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
	"math"
	"strconv"
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
	p := promparser.NewParser(promparser.Options{})
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
