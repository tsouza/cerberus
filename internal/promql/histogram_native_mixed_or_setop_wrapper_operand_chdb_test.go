//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

const (
	woHistMetric  = "wo_hist_exp_hist"
	woFloatMetric = "wo_float_gauge"
	woPlainMetric = "up"
)

// woMixedOr is the mixed `or` every query in
// [TestMixedOrWrapperAsSetOpOperandParity_ChDB] wraps. Its histogram arm
// answers series b="h" and b="d"; its float arm answers b="f" (7) and
// b="d" (42), and the histogram LHS shadows the float b="d" row.
const woMixedOr = `(` + woHistMetric + ` or ` + woFloatMetric + `)`

// woLabelReplace is cerberus issue #3823's own wrapper: it copies `b` into
// `a`, so every mixed row's set-op signature changes from {b} to {a, b}.
const woLabelReplace = `label_replace(` + woMixedOr + `, "a", "$1", "b", "(.*)")`

// woSeed backs every case: two histogram series and two float series for
// the mixed `or`, and three `up` series — two that match the label_replace
// output signatures ({a="f",b="f"} and {a="h",b="h"}) and one that matches
// nothing ({b="u"}).
var woSeed = `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('` + woHistMetric + `', map('b', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 3, 3.0, 0, 0, 0, [3], 0, []),
    ('` + woHistMetric + `', map('b', 'd'), toDateTime64('2026-01-01 00:00:00', 9), 5, 5.0, 0, 0, 0, [5], 0, []);
CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('` + woFloatMetric + `', map('b', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 7.0),
    ('` + woFloatMetric + `', map('b', 'd'), toDateTime64('2026-01-01 00:00:00', 9), 42.0),
    ('` + woPlainMetric + `', map('a', 'f', 'b', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + woPlainMetric + `', map('a', 'h', 'b', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('` + woPlainMetric + `', map('b', 'u'), toDateTime64('2026-01-01 00:00:00', 9), 1.0);
`

// woRow is one expected sample before it is rendered for an evaluation
// timestamp: a histogram row carries count/sum/one positive bucket, a float
// row carries value.
type woRow struct {
	name   string
	labels map[string]string
	hist   bool
	value  float64
	count  float64
	bucket uint64
	// selected marks a float row computed sample-for-sample from selected
	// samples — the sample itself, a per-sample function of it, or a
	// vector-vector operator over a matched pair: in instant mode it keeps
	// the sample's timestamp, while aggregated and histogram rows carry the
	// evaluation timestamp.
	selected bool
}

func woHist(name string, labels map[string]string, count float64, bucket uint64) woRow {
	return woRow{name: name, labels: labels, hist: true, count: count, bucket: bucket}
}

func woFloat(name string, labels map[string]string, value float64) woRow {
	return woRow{name: name, labels: labels, value: value}
}

func woSelected(name string, labels map[string]string, value float64) woRow {
	return woRow{name: name, labels: labels, value: value, selected: true}
}

// woSampleTS is the timestamp every seeded sample carries.
var woSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// woCell renders r at ts in the fourteen-column mixed sample layout (name,
// labels, timestamp, value, count, sum, scale, zero count, zero threshold,
// positive offset, positive buckets, negative offset, negative buckets,
// discriminator) or, when mixed is false, the four-column float layout.
// Every seeded histogram has Sum == Count.
func woCell(r woRow, ts time.Time, instant, mixed bool) []any {
	if instant && r.selected {
		ts = woSampleTS
	}
	at := ts.Format(time.RFC3339)
	if !mixed {
		return []any{r.name, r.labels, at, r.value}
	}
	if r.hist {
		return []any{r.name, r.labels, at, 0, r.count, r.count, 0, 0, 0, 0, []uint64{r.bucket}, 0, []uint64{}, 1}
	}
	return []any{r.name, r.labels, at, r.value, 0, 0, 0, 0, 0, 0, []uint64{}, 0, []uint64{}, 0}
}

// TestMixedOrWrapperAsSetOpOperandParity_ChDB pins cerberus issue #3823's
// shapes against the Prometheus reference engine: a wrapper composed
// directly over a mixed float/histogram `or`, as an operand of a vector
// set operator or as the float arm of a further mixed `or`, answers the
// rows Prometheus answers, in instant and range mode.
func TestMixedOrWrapperAsSetOpOperandParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	var (
		hH  = woHist(woHistMetric, map[string]string{"b": "h"}, 3, 3)
		hD  = woHist(woHistMetric, map[string]string{"b": "d"}, 5, 5)
		lhH = woHist(woHistMetric, map[string]string{"a": "h", "b": "h"}, 3, 3)
		lhD = woHist(woHistMetric, map[string]string{"a": "d", "b": "d"}, 5, 5)
		lfF = woSelected(woFloatMetric, map[string]string{"a": "f", "b": "f"}, 7)
		upU = woSelected(woPlainMetric, map[string]string{"b": "u"}, 1)
		upF = woSelected(woPlainMetric, map[string]string{"a": "f", "b": "f"}, 1)
		upH = woSelected(woPlainMetric, map[string]string{"a": "h", "b": "h"}, 1)
	)
	cases := []struct {
		name  string
		query string
		mixed bool
		rows  []woRow
	}{
		{"label_replace_or_histogram", woLabelReplace + ` or ` + woHistMetric, true, []woRow{lhH, lhD, lfF, hH, hD}},
		{"histogram_or_label_replace", woHistMetric + ` or ` + woLabelReplace, true, []woRow{hH, hD, lhH, lhD, lfF}},
		{"label_replace_or_float", woLabelReplace + ` or ` + woPlainMetric, true, []woRow{lhH, lhD, lfF, upU}},
		{"label_replace_and_float", woLabelReplace + ` and ` + woPlainMetric, true, []woRow{lhH, lfF}},
		{"float_unless_label_replace", woPlainMetric + ` unless ` + woLabelReplace, false, []woRow{upU}},
		// sum() over a group mixing float and histogram samples answers no
		// sample at all in Prometheus, so only the `up` series remain.
		{"sum_or_float", `sum(` + woMixedOr + `) or ` + woPlainMetric, true, []woRow{upF, upH, upU}},
		{"sum_by_or_float", `sum by (b) (` + woMixedOr + `) or ` + woPlainMetric, true, []woRow{
			woHist("", map[string]string{"b": "h"}, 3, 3),
			woHist("", map[string]string{"b": "d"}, 5, 5),
			woFloat("", map[string]string{"b": "f"}, 7),
			upF, upH, upU,
		}},
		// Vector-vector operators over the mixed `or`, as set-op operands.
		// Prometheus refuses histogram + float, so only the float pair joins.
		{"vector_arithmetic_and_float", `(` + woMixedOr + ` + on(b) ` + woPlainMetric + `) and on(b) ` + woPlainMetric, true, []woRow{
			woSelected("", map[string]string{"b": "f"}, 8),
		}},
		{"vector_comparison_or_float", `(` + woMixedOr + ` == ` + woMixedOr + `) or ` + woPlainMetric, true, []woRow{
			hH, hD, woSelected(woFloatMetric, map[string]string{"b": "f"}, 7), upF, upH, upU,
		}},
		{"count_or_histogram", `count(` + woMixedOr + `) or ` + woHistMetric, true, []woRow{
			woFloat("", map[string]string{}, 3), hH, hD,
		}},
		{"histogram_or_abs", woHistMetric + ` or abs(` + woMixedOr + `)`, true, []woRow{
			hH, hD, woSelected("", map[string]string{"b": "f"}, 7),
		}},
	}
	for _, step := range []time.Duration{0, time.Second} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/step=%s", tc.name, step), func(t *testing.T) {
				times := []time.Time{foEvalTS}
				endpoint := "/api/v1/query"
				if step > 0 {
					times = append(times, foEvalTS.Add(step))
					endpoint = "/api/v1/query_range"
				}
				cells := [][]any{}
				for _, at := range times {
					for _, r := range tc.rows {
						cells = append(cells, woCell(r, at, step == 0, tc.mixed))
					}
				}
				expected, err := json.Marshal(cells)
				if err != nil {
					t.Fatal(err)
				}
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
					{Name: "seed", Data: []byte(woSeed)},
					{Name: "expected_rows", Data: expected},
				}}
				path := filepath.Join(t.TempDir(), "mixed_or_wrapper_operand.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(tc.query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS.Add(step), step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
				sql, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				actual := spec.RunRoundTripSQL(t, fixture, sql, args)
				spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS.Add(step), Step: step}, actual)
			})
		}
	}
}
