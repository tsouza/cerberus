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

// minMaxSeed is a flat `cpu` series and a `bound` series that starts late
// and climbs, so scalar(bound) is NaN at the first two anchors of the
// range window and different at every later one.
const minMaxSeed = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 100.0),
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 100.0),
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 100.0),
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 100.0),
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 100.0);
INSERT INTO otel_metrics_gauge VALUES
    ('bound', map('env', 'prod'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    ('bound', map('env', 'prod'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    ('bound', map('env', 'prod'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('bound', map('env', 'prod'), toDateTime64('2026-01-01 00:05:00', 9), 60.0);
`

// minMaxInstantSeed carries one sample per series inside the lookback of
// the round-trip harness's instant, 2026-01-01T00:00:01Z, which is where a
// synthetic scalar row (min_of at the root, vector(...)) is stamped.
const minMaxInstantSeed = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('cpu', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),
    ('bound', map('env', 'prod'), toDateTime64('2026-01-01 00:00:00', 9), 60.0);
`

// minMaxInstant is the instant every instant case evaluates at.
var minMaxInstant = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

const minMaxInstantStamp = "2026-01-01T00:00:01Z"

var (
	minMaxRangeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	minMaxRangeEnd   = time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
)

// minMaxAnchors are the six range anchors, one minute apart.
var minMaxAnchors = []string{
	"2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z", "2026-01-01T00:02:00Z",
	"2026-01-01T00:03:00Z", "2026-01-01T00:04:00Z", "2026-01-01T00:05:00Z",
}

// TestScalarMinOfMaxOf_ReferenceParity_ChDB runs min_of / max_of through
// ClickHouse and grades every answer twice: against the rows below, and
// against the Prometheus engine the spec lane's oracle runs. The cases
// cover a computed argument at the root, as a function's scalar argument
// and as a binary operand, per step in a range query, NaN against a
// finite value, NaN against the dominating infinity, and the sign of
// zero (observed through 1/x).
func TestScalarMinOfMaxOf_ReferenceParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	nan, inf, negInf := "NaN", "+Inf", "-Inf"
	empty := map[string]string{}
	api := map[string]string{"job": "api"}
	perAnchor := func(labels map[string]string, values ...any) [][]any {
		rows := make([][]any, 0, len(values))
		for i, v := range values {
			rows = append(rows, []any{"", labels, minMaxAnchors[i], v})
		}
		return rows
	}
	for _, tc := range []struct {
		query string
		step  time.Duration
		rows  [][]any
	}{
		{"min_of(scalar(bound), 45)", 0, [][]any{{"", empty, minMaxInstantStamp, 45}}},
		{"max_of(scalar(bound), 45)", 0, [][]any{{"", empty, minMaxInstantStamp, 60}}},
		{"min_of(scalar(bound), 45)", time.Minute, perAnchor(empty, nan, nan, 30, 40, 45, 45)},
		{"max_of(45, scalar(bound))", time.Minute, perAnchor(empty, nan, nan, 45, 45, 50, 60)},
		{"max_of(scalar(bound), +Inf)", time.Minute, perAnchor(empty, inf, inf, inf, inf, inf, inf)},
		{"min_of(-Inf, scalar(bound))", time.Minute, perAnchor(empty, negInf, negInf, negInf, negInf, negInf, negInf)},
		{"min_of(scalar(bound), +Inf)", time.Minute, perAnchor(empty, nan, nan, 30, 40, 50, 60)},
		{"1 / min_of(scalar(bound) * 0, -0)", time.Minute, perAnchor(empty, nan, nan, negInf, negInf, negInf, negInf)},
		{"1 / max_of(-0, scalar(bound) * 0)", time.Minute, perAnchor(empty, nan, nan, inf, inf, inf, inf)},
		{"1 / max_of(-0, scalar(bound) * -0)", time.Minute, perAnchor(empty, nan, nan, negInf, negInf, negInf, negInf)},
		{"clamp_max(cpu, max_of(scalar(bound), 45))", time.Minute, perAnchor(api, nan, nan, 45, 45, 50, 60)},
		{"cpu - min_of(scalar(bound), 45)", 0, [][]any{{"", api, "2026-01-01T00:00:00Z", 55}}},
		{"vector(max_of(scalar(bound), time() - 1767225581))", 0, [][]any{{"", empty, minMaxInstantStamp, 60}}},
		{"cpu * min_of(2, 3)", 0, [][]any{{"", api, "2026-01-01T00:00:00Z", 200}}},
	} {
		t.Run(fmt.Sprintf("%s/step=%s", tc.query, tc.step), func(t *testing.T) {
			endpoint, start, end, seed := "/api/v1/query", minMaxInstant, minMaxInstant, minMaxInstantSeed
			files := []txtar.File{{Name: "query.promql", Data: []byte(tc.query + "\n")}}
			if tc.step > 0 {
				endpoint, start, end, seed = "/api/v1/query_range", minMaxRangeStart, minMaxRangeEnd, minMaxSeed
			}
			files = append(files, txtar.File{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")})
			if tc.step > 0 {
				files = append(files, txtar.File{Name: "range_step", Data: []byte(tc.step.String() + "\n")})
			}
			expected, err := json.Marshal(tc.rows)
			if err != nil {
				t.Fatal(err)
			}
			files = append(
				files,
				txtar.File{Name: "seed", Data: []byte(seed)},
				txtar.File{Name: "expected_rows", Data: expected},
			)
			path := filepath.Join(t.TempDir(), "min_max.txtar")
			if err := os.WriteFile(path, txtar.Format(&txtar.Archive{Files: files}), fixturePermissions); err != nil {
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
			plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, tc.step)
			if err != nil {
				t.Fatal(err)
			}
			optimized := spec.AssertScanTimeBoundAccepts(t, plan)
			sql, params, err := chsql.Emit(context.Background(), optimized)
			if err != nil {
				t.Fatal(err)
			}
			result := spec.RunRoundTripSQL(t, fixture, sql, params)
			spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: end, Step: tc.step}, result)
		})
	}
}
