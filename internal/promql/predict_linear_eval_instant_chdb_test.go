//go:build chdb

// chDB proof, against the Prometheus reference engine, that predict_linear
// forecasts from the evaluation instant whatever `@` and `offset` do to its
// window, that the regression x-axis keeps a sample's or a pin's fractional
// second, and that predict_linear over an `@`-pinned subquery answers empty,
// in instant and range mode.
package promql_test

import (
	"context"
	"encoding/json"
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

// predictEvalLineOrigin is the instant at which both seeded series cross
// zero. Every sample sits on the line `value = seconds since origin`, so the
// regression is exact and a forecast is that line read at the forecast
// instant.
var predictEvalLineOrigin = time.Date(2026, 1, 1, 0, 40, 0, 0, time.UTC)

// predictEvalSeed holds two series on the line through
// predictEvalLineOrigin with slope one: `line` at whole seconds and
// `subsecond` at fractional ones.
const predictEvalSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('line', map('job', 'a'), toDateTime64('2026-01-01 00:40:00', 9), 0.0),\n" +
	"    ('line', map('job', 'a'), toDateTime64('2026-01-01 00:44:16', 9), 256.0),\n" +
	"    ('subsecond', map('job', 'a'), toDateTime64('2026-01-01 00:40:00.5', 9), 0.5),\n" +
	"    ('subsecond', map('job', 'a'), toDateTime64('2026-01-01 00:44:16.25', 9), 256.25),\n" +
	"    ('subsecond', map('job', 'a'), toDateTime64('2026-01-01 00:45:00.75', 9), 300.75);\n"

// predictEvalInstant is the instant query's evaluation instant and the
// range query's last step. The range query starts predictEvalRangeSteps
// one-minute steps earlier.
var predictEvalInstant = time.Date(2026, 1, 1, 0, 46, 10, 0, time.UTC)

const predictEvalRangeSteps = 2

// predictEvalHorizon is the forecast horizon, in seconds, every case asks.
const predictEvalHorizon = 60

// predictEvalAnswer is a case's answer at one evaluation instant, and
// whether the case answers there at all.
type predictEvalAnswer func(at time.Time) (float64, bool)

// predictEvalForecast answers the line read predictEvalHorizon seconds past
// the evaluation instant, from the first evaluation instant from on.
func predictEvalForecast(from time.Time) predictEvalAnswer {
	return func(at time.Time) (float64, bool) {
		return at.Sub(predictEvalLineOrigin).Seconds() + predictEvalHorizon, !at.Before(from)
	}
}

// predictEvalSlope answers the line's slope from the evaluation instant from
// on.
func predictEvalSlope(from time.Time) predictEvalAnswer {
	return func(at time.Time) (float64, bool) { return 1, !at.Before(from) }
}

// predictEvalTimeForecast answers what predict_linear over a time()
// subquery forecasts: the evaluation instant's Unix seconds plus the
// horizon. Its series carries no label.
func predictEvalTimeForecast(at time.Time) (float64, bool) {
	return float64(at.Unix() + predictEvalHorizon), true
}

func predictEvalNone(time.Time) (float64, bool) { return 0, false }

// TestPredictLinearEvaluationInstantParity_ChDB pins cerberus issues #3878
// and #3884 against the Prometheus reference engine, with the answers
// written out by hand.
func TestPredictLinearEvaluationInstantParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	// fractionalPin is 00:45:30.5, wholePin 00:45:30, and subqueryPin
	// 00:45:00; all three lie after every sample of `line`.
	const (
		fractionalPin = "1767228330.5"
		wholePin      = "1767228330"
		subqueryPin   = "1767228300"
	)
	first := predictEvalInstant.Add(-predictEvalRangeSteps * time.Minute)
	second := first.Add(time.Minute)
	cases := []struct {
		name   string
		query  string
		answer predictEvalAnswer
		// unlabelled marks a case over vector(), whose series has no label.
		unlabelled bool
	}{
		// #3884: the pin only chooses the window; the forecast starts at
		// the evaluation instant, to the millisecond.
		{"fractional_pin", `predict_linear(line[10m] @ ` + fractionalPin + `, 60)`, predictEvalForecast(first), false},
		{"whole_pin", `predict_linear(line[10m] @ ` + wholePin + `, 60)`, predictEvalForecast(first), false},
		{"fractional_pin_offset", `predict_linear(line[10m] @ ` + fractionalPin + ` offset 30s, 60)`, predictEvalForecast(first), false},
		{"fractional_pin_computed_horizon", `predict_linear(line[10m] @ ` + fractionalPin + `, scalar(vector(60)))`, predictEvalForecast(first), false},
		{"end_pin", `predict_linear(line[10m] @ end(), 60)`, predictEvalForecast(first), false},
		// An offset moves the window, never the instant forecast from.
		{"offset", `predict_linear(line[10m] offset 30s, 60)`, predictEvalForecast(second), false},
		{"subquery_offset", `predict_linear(vector(time())[5m:1m] offset 30s, 60)`, predictEvalTimeForecast, true},
		// The regression x-axis keeps every sample's fractional second.
		{"subsecond_samples", `predict_linear(subsecond[10m], 60)`, predictEvalForecast(second), false},
		{"subsecond_samples_fractional_pin", `predict_linear(subsecond[10m] @ ` + fractionalPin + `, 60)`, predictEvalForecast(first), false},
		{"subsecond_deriv", `deriv(subsecond[10m])`, predictEvalSlope(second), false},
		{"subsecond_deriv_fractional_pin", `deriv(subsecond[10m] @ ` + fractionalPin + `)`, predictEvalSlope(first), false},
		// #3878: predict_linear over an `@`-pinned subquery is empty.
		{"pinned_constant_subquery", `predict_linear(vector(time())[5m:1m] @ ` + subqueryPin + `, 60)`, predictEvalNone, false},
		{"pinned_selector_subquery", `predict_linear(line[10m:1m] @ ` + fractionalPin + `, 60)`, predictEvalNone, false},
		{"end_pinned_subquery", `predict_linear((vector(time())[5m:1m] @ end()), 60)`, predictEvalNone, false},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				start, step, endpoint := predictEvalInstant, time.Duration(0), "/api/v1/query"
				if rangeMode {
					start, step, endpoint = first, time.Minute, "/api/v1/query_range"
				}
				expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, predictEvalInstant, step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
				sql, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				// The answer's own columns: the reported instant only in range
				// mode, where it differs per step.
				cells := [][]any{}
				projection := "`Attributes`, `Value`"
				if rangeMode {
					projection = "`Attributes`, `TimeUnix`, `Value`"
				}
				for at := start; !at.After(predictEvalInstant); at = at.Add(time.Minute) {
					v, ok := tc.answer(at)
					if !ok {
						continue
					}
					labels := map[string]string{"job": "a"}
					if tc.unlabelled {
						labels = map[string]string{}
					}
					if rangeMode {
						cells = append(cells, []any{labels, at.Format(time.RFC3339Nano), v})
					} else {
						cells = append(cells, []any{labels, v})
					}
				}
				sql = "SELECT " + projection + " FROM (" + sql + ")"
				expected, err := json.Marshal(cells)
				if err != nil {
					t.Fatal(err)
				}
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
					{Name: "seed", Data: []byte(predictEvalSeed)},
					{Name: "expected_rows", Data: expected},
				}}
				path := filepath.Join(t.TempDir(), "predict_linear_eval_instant.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				actual := spec.RunRoundTripSQL(t, fixture, sql, args)
				spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: predictEvalInstant, Step: step}, actual)
			})
		}
	}
}
