//go:build chdb

// chDB proof, against the Prometheus reference engine, that `step()` and
// `range()` answer the request's step and span wherever they sit, and
// that a subquery evaluates its inner expression at its step truncated
// to whole milliseconds.
package promql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// preprocessSeed holds the series `qc_probe{job="a"}` with a sample every
// minute from 2026-01-01 00:00 to 00:40.
const preprocessSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 3.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 4.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 5.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:06:00', 9), 6.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:07:00', 9), 7.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:08:00', 9), 8.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:09:00', 9), 9.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:10:00', 9), 10.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:11:00', 9), 11.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:12:00', 9), 12.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:13:00', 9), 13.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:14:00', 9), 14.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:15:00', 9), 15.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:16:00', 9), 16.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:17:00', 9), 17.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:18:00', 9), 18.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:19:00', 9), 19.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:20:00', 9), 20.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:21:00', 9), 21.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:22:00', 9), 22.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:23:00', 9), 23.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:24:00', 9), 24.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:25:00', 9), 25.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:26:00', 9), 26.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:27:00', 9), 27.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:28:00', 9), 28.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:29:00', 9), 29.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:30:00', 9), 30.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:31:00', 9), 31.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:32:00', 9), 32.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:33:00', 9), 33.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:34:00', 9), 34.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:35:00', 9), 35.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:36:00', 9), 36.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:37:00', 9), 37.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:38:00', 9), 38.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:39:00', 9), 39.0),\n" +
	"    ('qc_probe', map('job', 'a'), toDateTime64('2026-01-01 00:40:00', 9), 40.0);\n"

// preprocessEnd is the instant query's evaluation instant and the range
// query's last step; the range query starts preprocessRangeSteps steps of
// preprocessStep earlier.
var preprocessEnd = time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)

const (
	preprocessStep       = 5 * time.Minute
	preprocessRangeSteps = 4
)

// TestPreprocessParity_ChDB pins the query-context fold and the subquery
// step truncation against the Prometheus reference engine, in instant and
// range mode, with the answers written out by hand.
func TestPreprocessParity_ChDB(t *testing.T) {
	start := preprocessEnd.Add(-preprocessRangeSteps * preprocessStep)
	span := preprocessEnd.Sub(start).Seconds()
	job := map[string]string{"job": "a"}
	none := map[string]string{}
	cases := []struct {
		name   string
		query  string
		labels map[string]string
		// instant and ranged are the answer of an instant and of a range
		// query at every one of its steps.
		instant, ranged float64
		// instantOnly marks a case asked only as an instant query: a range
		// query evaluates its millisecond grid over the whole range, more
		// samples than the reference engine loads.
		instantOnly bool
	}{
		{"subquery_step", `max_over_time(vector(step())[10m:1m])`, none, 0, preprocessStep.Seconds(), false},
		{"subquery_range", `max_over_time(vector(range())[10m:1m])`, none, 0, span, false},
		{"subquery_step_over_series", `max_over_time((qc_probe * 0 + step())[10m:1m])`, job, 0, preprocessStep.Seconds(), false},
		{"nested_subquery_range", `max_over_time(max_over_time(vector(range())[10m:1m])[20m:2m])`, none, 0, span, false},
		{"scalar_argument_step", `clamp_max(vector(1000), step())`, none, 0, preprocessStep.Seconds(), false},
		{"root_range", `range()`, none, 0, span, false},
		// Prometheus evaluates a subquery at its step truncated to whole
		// milliseconds: 1.5ms evaluates at every millisecond, ten of which
		// the 10ms window holds.
		{"fractional_millisecond_step", `count_over_time(vector(1)[10ms:0.0015])`, none, 10, 10, true},
		{"fractional_millisecond_step_over_series", `count_over_time(qc_probe[10ms:0.0015])`, job, 10, 10, true},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			if rangeMode && tc.instantOnly {
				continue
			}
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				first, step := preprocessEnd, time.Duration(0)
				if rangeMode {
					first, step = start, preprocessStep
				}
				var cells [][]any
				for at := first; !at.After(preprocessEnd); at = at.Add(preprocessStep) {
					if rangeMode {
						cells = append(cells, []any{tc.labels, at.Format(time.RFC3339Nano), tc.ranged})
					} else {
						cells = append(cells, []any{tc.labels, tc.instant})
					}
				}
				assertPreprocessParity(t, tc.query, first, preprocessEnd, step, cells)
			})
		}
	}
	// A range spanning more than one lowering run, about 146 years, is
	// answered in runs of its steps; each names the request's step and
	// span, never its own.
	t.Run("split_range", func(t *testing.T) {
		const splitSteps = 2500
		splitStep := 30 * 24 * time.Hour
		splitStart := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		splitEnd := splitStart.Add(splitSteps * splitStep)
		for _, tc := range []struct {
			query string
			want  float64
		}{
			{`range()`, splitEnd.Sub(splitStart).Seconds()},
			{`vector(range())`, splitEnd.Sub(splitStart).Seconds()},
			{`step()`, splitStep.Seconds()},
		} {
			t.Run(tc.query, func(t *testing.T) {
				cells := make([][]any, 0, splitSteps+1)
				for at := splitStart; !at.After(splitEnd); at = at.Add(splitStep) {
					cells = append(cells, []any{none, at.Format(time.RFC3339Nano), tc.want})
				}
				assertPreprocessParity(t, tc.query, splitStart, splitEnd, splitStep, cells)
			})
		}
	})
}

// assertPreprocessParity pins query, an instant query at end when step is
// zero and a range query over [start, end] otherwise, to the rows cells,
// in ClickHouse and in the Prometheus reference engine.
func assertPreprocessParity(t *testing.T, query string, start, end time.Time, step time.Duration, cells [][]any) {
	t.Helper()
	const fixturePermissions = 0o600
	expr, err := queryContextParser().ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	projection, endpoint := "`Attributes`, `Value`", "/api/v1/query"
	if step > 0 {
		projection, endpoint = "`Attributes`, `TimeUnix`, `Value`", "/api/v1/query_range"
	}
	sql = "SELECT " + projection + " FROM (" + sql + ")"
	expected, err := json.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	archive := &txtar.Archive{Files: []txtar.File{
		{Name: "query.promql", Data: []byte(query + "\n")},
		{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
		{Name: "seed", Data: []byte(preprocessSeed)},
		{Name: "expected_rows", Data: expected},
	}}
	path := filepath.Join(t.TempDir(), "preprocess.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
		t.Fatal(err)
	}
	fixture, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := spec.RunRoundTripSQL(t, fixture, sql, args)
	spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: end, Step: step}, actual)
}
