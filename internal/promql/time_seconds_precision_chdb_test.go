//go:build chdb

// chDB proof, against the Prometheus reference engine, that time() and
// timestamp() report the same float64 Prometheus computes, bit for bit:
// `float64(t) / 1000` over the instant's Unix milliseconds.
package promql_test

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

const secondsPrecisionMetric = "seconds_precision_probe"

// secondsPrecisionInstants are evaluation instants whose Unix
// nanoseconds, divided by 1e9 in float64, land one ULP away from their
// milliseconds divided by 1000 — one in 1811, one just before 2^62
// nanoseconds before the epoch (late 1823), and a modern one with a
// millisecond fraction — and, as a control, one a second after the 1823
// instant, where the two conversions agree.
var secondsPrecisionInstants = []struct {
	name string
	at   time.Time
}{
	{"1811", time.UnixMilli(-5000000005000).UTC()},
	{"near_1824", time.UnixMilli(-4611686019000).UTC()},
	{"near_1824_agreeing", time.UnixMilli(-4611686018000).UTC()},
	{"modern", time.UnixMilli(1767225601123).UTC()},
}

// secondsPrecisionSampleOffset is how long before each evaluation instant
// the seed stores a sample: before the first step of a range query and
// inside the five-minute lookback of its last.
const secondsPrecisionSampleOffset = 150 * time.Second

// secondsPrecisionSeed stores one sample per evaluation instant, each in
// its own series labelled with the instant's name.
func secondsPrecisionSeed() string {
	rows := make([]string, 0, len(secondsPrecisionInstants))
	for _, ev := range secondsPrecisionInstants {
		rows = append(rows, "    ('"+secondsPrecisionMetric+"', map('at', '"+ev.name+"'), fromUnixTimestamp64Nano(toInt64("+
			strconv.FormatInt(ev.at.Add(-secondsPrecisionSampleOffset).UnixNano(), 10)+")), 1.0)")
	}
	return "CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
		"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
		"INSERT INTO otel_metrics_gauge VALUES\n" + strings.Join(rows, ",\n") + ";\n"
}

// secondsPrecisionSeries is secondsPrecisionSeed as the reference engine
// reads it.
func secondsPrecisionSeries() []oracle.Series {
	series := make([]oracle.Series, 0, len(secondsPrecisionInstants))
	for _, ev := range secondsPrecisionInstants {
		series = append(series, oracle.Series{
			Labels: map[string]string{"__name__": secondsPrecisionMetric, "at": ev.name},
			Points: []oracle.Point{{TMillis: ev.at.Add(-secondsPrecisionSampleOffset).UnixMilli(), Value: 1}},
		})
	}
	return series
}

// promSeconds is Prometheus's own `float64(t) / 1000` for the instant at.
func promSeconds(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }

// TestTimeAndTimestampSecondsBitExact_ChDB pins cerberus issue #3882: the
// value time() and timestamp() report equals Prometheus's bit for bit, in
// instant and range mode, before 1824, around it, and today.
func TestTimeAndTimestampSecondsBitExact_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	evalInstant := func(at time.Time) float64 { return promSeconds(at) }
	for _, rangeMode := range []bool{false, true} {
		mode := "instant"
		if rangeMode {
			mode = "range"
		}
		for _, ev := range secondsPrecisionInstants {
			sampleAt := ev.at.Add(-secondsPrecisionSampleOffset)
			queries := []struct {
				name   string
				query  string
				labels map[string]string
				// value is what the query reports at the step Prometheus
				// evaluates it at.
				value func(step time.Time) float64
				// reportedAt is the instant an instant query's row
				// carries: the selected sample's own for
				// timestamp(<selector>), and the one the round trip
				// substitutes for now64() otherwise. The API replaces
				// either with the evaluation instant.
				reportedAt time.Time
			}{
				{"time", `time()`, noLabels, evalInstant, instantDomainEval},
				{"vector_time", `vector(time())`, noLabels, evalInstant, instantDomainEval},
				{"time_scalar_operand", `vector(0) + time()`, noLabels, evalInstant, instantDomainEval},
				{"timestamp_vector", `timestamp(vector(1))`, noLabels, evalInstant, instantDomainEval},
				{
					"timestamp_selector", `timestamp(` + secondsPrecisionMetric + `{at="` + ev.name + `"})`,
					map[string]string{"at": ev.name},
					idConst(promSeconds(sampleAt)), sampleAt,
				},
			}
			for _, q := range queries {
				t.Run(q.name+"/"+ev.name+"/"+mode, func(t *testing.T) {
					row := idRow(q.labels, q.value)
					row.sampleAt = q.reportedAt
					runHandWrittenParity(t, secondsPrecisionSeed(), q.query, ev.at, []instantDomainRow{row}, rangeMode)
					assertBitExactAgainstReference(t, q.query, ev.at, rangeMode, q.value)
				})
			}
		}
	}

	// The issue's own reproductions: a subquery pinned before 1824, and a
	// year-one pin Prometheus evaluates at a wrapped instant in 1754.
	pinned := []struct {
		name  string
		query string
		want  float64
	}{
		{"subquery_pinned_1811", `max_over_time(timestamp(vector(1))[5m:7s] @ -5000000000)`, -5000000005},
		{"subquery_pinned_year_one", `max_over_time(timestamp(vector(1))[5m:7s] @ -62135596800)`, -6795364583},
	}
	for _, rangeMode := range []bool{false, true} {
		mode := "instant"
		if rangeMode {
			mode = "range"
		}
		for _, p := range pinned {
			t.Run(p.name+"/"+mode, func(t *testing.T) {
				runHandWrittenParity(t, secondsPrecisionSeed(), p.query, instantDomainEval, []instantDomainRow{idRow(noLabels, idConst(p.want))}, rangeMode)
				assertBitExactAgainstReference(t, p.query, instantDomainEval, rangeMode, idConst(p.want))
			})
		}
	}
}

// assertBitExactAgainstReference evaluates query at eval (or over the
// instantDomainRangeSteps one-minute steps ending there) in chDB over
// secondsPrecisionSeed and in the Prometheus reference engine over the
// same samples, and requires every value of both to have exactly the bits
// of want at its step.
func assertBitExactAgainstReference(t *testing.T, query string, eval time.Time, rangeMode bool, want func(time.Time) float64) {
	t.Helper()
	start, step := eval, time.Duration(0)
	if rangeMode {
		start, step = eval.Add(-instantDomainRangeSteps*time.Minute), time.Minute
	}
	var steps []time.Time
	for at := start; !at.After(eval); at = at.Add(time.Minute) {
		steps = append(steps, at)
	}
	wantBits := make([]uint64, 0, len(steps))
	for _, at := range steps {
		wantBits = append(wantBits, math.Float64bits(want(at)))
	}
	slices.Sort(wantBits)

	results, err := oracle.Evaluate(t, secondsPrecisionSeries(), oracle.Query{Expr: query, Start: start, End: eval, Step: step})
	if err != nil {
		t.Fatalf("reference engine: %v", err)
	}
	refBits := make([]uint64, 0, len(results))
	for _, r := range results {
		refBits = append(refBits, math.Float64bits(r.Value))
	}
	slices.Sort(refBits)
	if !slices.Equal(refBits, wantBits) {
		t.Fatalf("reference engine bits %v, want %v", bitsAsFloats(refBits), bitsAsFloats(wantBits))
	}

	gotBits := cerberusValueBits(t, query, start, eval, step)
	if !slices.Equal(gotBits, refBits) {
		t.Fatalf("cerberus values %v differ from the reference engine's %v (bits %x vs %x)",
			bitsAsFloats(gotBits), bitsAsFloats(refBits), gotBits, refBits)
	}
}

// cerberusValueBits lowers query, runs the emitted SQL in chDB over
// secondsPrecisionSeed, and returns the sorted bits of its Value column.
func cerberusValueBits(t *testing.T, query string, start, end time.Time, step time.Duration) []uint64 {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	db := spec.OpenChDB(t)
	spec.ApplySeed(t, db, secondsPrecisionSeed())
	// Only the Value column is read: the outer SELECT keeps the driver
	// away from the Map and anchor columns it does not need.
	q, a := spec.SubstituteNow64("SELECT `Value` FROM ("+sqlText+")", args)
	rows, err := db.Query(q, a...)
	if err != nil {
		t.Fatalf("query emitted SQL: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var bits []uint64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		bits = append(bits, math.Float64bits(v))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(bits)
	return bits
}

func bitsAsFloats(bits []uint64) []float64 {
	out := make([]float64, len(bits))
	for i, b := range bits {
		out[i] = math.Float64frombits(b)
	}
	return out
}
