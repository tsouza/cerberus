//go:build chdb

// chDB proof, against the Prometheus reference engine, that queries whose
// `@` pins, offsets or subquery grids leave ClickHouse's DateTime64(9)
// range are answered in SQL with Prometheus's own answer, in instant and
// range mode.
package promql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

const instantDomainMetric = "instant_domain_probe"

// instantDomainSeed holds a sample just before instantDomainEval, and one
// in 1811, before the earliest instant a parsed DateTime64 string names.
const instantDomainSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('" + instantDomainMetric + "', map('job', 'api'), toDateTime64('2025-12-31 23:59:00', 9), 2.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'old'), fromUnixTimestamp64Nano(toInt64(-5000000060000000000)), 3.0);\n"

var (
	instantDomainEval = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	// instantDomainOldSample is the 1811 sample's timestamp.
	instantDomainOldSample = time.Unix(-5000000060, 0).UTC()
	// Request instants a few minutes inside the int64-nanosecond floor and
	// ceiling, to which the API's request parsing clamps a request.
	instantDomainNearFloor   = time.Date(1677, 9, 21, 0, 20, 0, 0, time.UTC)
	instantDomainNearCeiling = time.Date(2262, 4, 11, 23, 46, 0, 0, time.UTC)
)

// instantDomainRangeSteps is how many one-minute steps before the
// evaluation instant a range case starts.
const instantDomainRangeSteps = 2

// instantDomainRangeVectorCases names the cases whose query is a range
// vector, which only an instant query may ask.
var instantDomainRangeVectorCases = map[string]bool{
	"root_matrix_near_ceiling":     true,
	"root_matrix_read_at_year_one": true,
}

// instantDomainRow is one series of a case's answer. at maps a step
// instant to the value the series carries there; sampleAt, when set, is
// the instant an instant query reports for a selected sample in place of
// the evaluation instant.
type instantDomainRow struct {
	name     string
	labels   map[string]string
	at       func(step time.Time) float64
	sampleAt time.Time
}

func idConst(v float64) func(time.Time) float64 { return func(time.Time) float64 { return v } }

// idStepSeconds is the value of a series that carries, at each step, that
// step's own Unix seconds: the last grid instant of a 1m-step subquery
// ending on a whole minute.
func idStepSeconds(step time.Time) float64 { return float64(step.Unix()) }

func idRow(labels map[string]string, at func(time.Time) float64) instantDomainRow {
	return instantDomainRow{labels: labels, at: at}
}

// TestInstantDomainParity_ChDB pins cerberus issue #3824's shapes against
// the Prometheus reference engine, with the answers written out by hand.
func TestInstantDomainParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	noLabels := map[string]string{}
	five := []instantDomainRow{idRow(noLabels, idConst(5))}
	cases := []struct {
		name  string
		query string
		eval  time.Time
		rows  []instantDomainRow
	}{
		// The issue's reference answers: a constant inner is evaluated at
		// the instants Prometheus evaluates the pin at, a stored series is
		// empty there.
		{"year_one_constant", `count_over_time(vector(1)[5m:1m] @ -62135596800)`, instantDomainEval, five},
		{"year_one_selector_subquery", `count_over_time(` + instantDomainMetric + `[5m:1m] @ -62135596800)`, instantDomainEval, nil},
		{"year_one_selector", instantDomainMetric + ` @ -62135596800`, instantDomainEval, nil},
		{"before_int64_floor_constant", `count_over_time(vector(1)[5m:1m] @ -9300000000)`, instantDomainEval, five},
		{"pin_1e17_constant", `count_over_time(vector(1)[5m:1m] @ 1e17)`, instantDomainEval, five},
		{"past_int64_ceiling_constant", `count_over_time(vector(1)[5m:1m] @ 10000000000)`, instantDomainEval, five},
		// A subquery window reaching below the floor, by its range or by
		// an offset.
		{"window_below_floor", `count_over_time(vector(1)[5m:1m] @ -9223372036.854)`, instantDomainEval, five},
		{"window_below_floor_by_offset", `count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`, instantDomainEval, five},
		// The evaluation instant itself as a value: Prometheus's wrapped
		// instant in 1754 for year one, the pin itself past 2262.
		{
			"year_one_timestamp", `max_over_time(timestamp(vector(1))[5m:1m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(-6795364620))},
		},
		{
			"past_ceiling_timestamp", `max_over_time(timestamp(vector(1))[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_parenthesised_timestamp", `max_over_time((timestamp(vector(1)))[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_time", `max_over_time(vector(time())[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_year", `max_over_time(year()[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(2286))},
		},
		{
			"past_ceiling_ts_of_max", `ts_of_max_over_time(vector(1)[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		// Prometheus reports max(t, 0) for the last sample of a float-only
		// window, so a window before 1970 reports 0.
		{
			"year_one_ts_of_last", `ts_of_last_over_time(vector(1)[5m:1m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(0))},
		},
		// Mixes of a constant arm and a read: `or` keeps the constant
		// side, arithmetic answers nothing.
		{"or_keeps_constant_in_subquery", `count_over_time((` + instantDomainMetric + ` or vector(1))[5m:1m] @ -62135596800)`, instantDomainEval, five},
		{
			"or_keeps_constant", instantDomainMetric + ` @ -62135596800 or vector(7)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(7))},
		},
		{"arithmetic_is_empty", `vector(1) * on() (` + instantDomainMetric + ` @ -62135596800)`, instantDomainEval, nil},
		{"aggregated_read_past_ceiling", `sum(rate(` + instantDomainMetric + `[5m] @ 10000000000))`, instantDomainEval, nil},
		// absent answers from the selector's matchers.
		{
			"absent_pin_1e17", `absent(` + instantDomainMetric + `{job="a"} @ 1e17)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "a"}, idConst(1))},
		},
		{
			"absent_over_time_year_one", `absent_over_time(` + instantDomainMetric + `{job="api"}[5m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "api"}, idConst(1))},
		},
		// A sample before 1900 is read at its own instant.
		{"selector_before_1900", instantDomainMetric + `{job="old"} @ -5000000000`, instantDomainEval, []instantDomainRow{{
			name: instantDomainMetric, labels: map[string]string{"job": "old"}, at: idConst(3), sampleAt: instantDomainOldSample,
		}}},
		{
			"window_before_1900", `count_over_time(` + instantDomainMetric + `{job="old"}[10m] @ -5000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "old"}, idConst(1))},
		},
		// A root subquery answers its grid instants, which lie in the
		// range although its window reaches past it.
		{name: "root_matrix_near_ceiling", query: `vector(1)[5m:1m] @ -9223372036.854`, eval: instantDomainEval, rows: []instantDomainRow{
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371780000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371840000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371900000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371960000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223372020000).UTC()},
		}},
		{name: "root_matrix_read_at_year_one", query: instantDomainMetric + `[5m] @ -62135596800`, eval: instantDomainEval},
		// Requests near the bounds, where the unpinned grid leaves the
		// range.
		{"near_floor_constant", `count_over_time(vector(1)[5m:1m])`, instantDomainNearFloor, five},
		{
			"near_floor_timestamp", `max_over_time(timestamp(vector(1))[5m:1m])`, instantDomainNearFloor,
			[]instantDomainRow{idRow(noLabels, idStepSeconds)},
		},
		{"near_ceiling_constant", `count_over_time(vector(1)[5m:1m] offset -3m)`, instantDomainNearCeiling, five},
		{
			"near_ceiling_timestamp", `max_over_time(timestamp(vector(1))[5m:1m])`, instantDomainNearCeiling,
			[]instantDomainRow{idRow(noLabels, idStepSeconds)},
		},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			if rangeMode && instantDomainRangeVectorCases[tc.name] {
				continue
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				start, step, endpoint := tc.eval, time.Duration(0), "/api/v1/query"
				if rangeMode {
					start, step, endpoint = tc.eval.Add(-instantDomainRangeSteps*time.Minute), time.Minute, "/api/v1/query_range"
				}
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(tc.query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, tc.eval, step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
				sql, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				sql, cells := instantDomainCells(t, sql, args, tc.rows, start, tc.eval, rangeMode)
				expected, err := json.Marshal(cells)
				if err != nil {
					t.Fatal(err)
				}
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
					{Name: "seed", Data: []byte(instantDomainSeed)},
					{Name: "expected_rows", Data: expected},
				}}
				path := filepath.Join(t.TempDir(), "instant_domain.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				actual := spec.RunRoundTripSQL(t, fixture, sql, args)
				spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: tc.eval, Step: step}, actual)
			})
		}
	}
}

// instantDomainCells lays the hand-written answer rows out as the
// expected_rows of the emitted SQL, whose result columns depend on the
// plan's shape: each column takes the series name, its labels, the
// reported instant or the value, by the column's name.
//
// A range plan over a subquery also returns anchor_ts, the instant its
// grid is evaluated at, which differs from the reported TimeUnix by any
// offset and by the shift that moves a grid into ClickHouse's range. No
// client sees it, so the returned SQL, which the round trip runs, leaves
// it out whenever TimeUnix is there to report the instant.
func instantDomainCells(t *testing.T, sql string, args []any, want []instantDomainRow, start, end time.Time, rangeMode bool) (string, [][]any) {
	t.Helper()
	db := spec.OpenChDB(t)
	spec.ApplySeed(t, db, instantDomainSeed)
	q, a := spec.SubstituteNow64(sql, args)
	rows, err := db.Query(q, a...)
	if err != nil {
		t.Fatalf("query emitted SQL: %v", err)
	}
	cols, err := rows.Columns()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("result columns: %v", err)
	}
	if slices.Contains(cols, "anchor_ts") && slices.Contains(cols, "TimeUnix") {
		cols = slices.DeleteFunc(cols, func(c string) bool { return c == "anchor_ts" })
		sql = "SELECT `" + strings.Join(cols, "`, `") + "` FROM (" + sql + ")"
	}
	cells := [][]any{}
	for at := start; !at.After(end); at = at.Add(time.Minute) {
		for _, r := range want {
			ts := at
			if !rangeMode && !r.sampleAt.IsZero() {
				ts = r.sampleAt
			}
			cell := make([]any, 0, len(cols))
			for _, c := range cols {
				switch c {
				case "MetricName":
					cell = append(cell, r.name)
				case "Attributes":
					cell = append(cell, r.labels)
				case "Value":
					cell = append(cell, r.at(at))
				default:
					cell = append(cell, ts.Format(time.RFC3339))
				}
			}
			cells = append(cells, cell)
		}
	}
	return sql, cells
}
