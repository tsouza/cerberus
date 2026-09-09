//go:build chdb

// chDB-backed proof that the label value `histogram_quantiles` stamps for
// a COMPUTED phi is spelled the way Prometheus spells it, across the
// whole value sweep — not just at the two phis the TXTAR fixtures pin.
//
// The expectation is computed in the test from
// `labels.FormatOpenMetricsFloat`, the reference call
// `promql.funcHistogramQuantiles` makes:
//
//	enh.lb.Set(labelName, labels.FormatOpenMetricsFloat(phi))
//
// Nothing in the assertion restates a literal cerberus produced, so it
// cannot be satisfied by regenerating a golden.
//
// # Why the sweep, and why now
//
// [openMetricsFloatExpr] and [nativeHistogramShortestGString] used to
// carry two transcriptions of one decomposition, and that is how they
// drifted into disagreeing about the scientific-notation threshold
// (#3214). #3228 collapsed them onto [shortestGExpr]. What pinned the
// OpenMetrics side at the time was two fixtures — phi = 0.5 and
// phi = 1e-5 — against a formatter with eight branches: the hardcoded
// 1 / 0 / -1 / NaN / ±Inf cases, the scientific arm and the fixed arm.
// Six of those eight had no execution behind them at all, so a shared
// helper could have moved one and left every golden green.
//
// [openMetricsFloatSweep] walks all eight, including both sides of each
// layout boundary and the exponent-padding boundary.
package promql_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// openMetricsFloatMetric is the classic histogram the swept
// `histogram_quantiles` reads. Distinct from every sibling fixture
// sharing this package's chDB session, so neither can read the other's
// rows.
const openMetricsFloatMetric = "open_metrics_float_sweep_hist"

// openMetricsFloatPhiMetric carries the computed phi. It is a gauge
// because `scalar()` needs exactly one series and a gauge is the
// cheapest carrier of one.
const openMetricsFloatPhiMetric = "open_metrics_float_sweep_phi"

// openMetricsFloatLabel is the label name the swept call stamps.
const openMetricsFloatLabel = "q"

// openMetricsFloatBaseline anchors the seeded sample; each swept phi is
// seeded one step later than the last so a single instant query per phi
// reads exactly one of them.
var openMetricsFloatBaseline = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// openMetricsFloatStep spaces the seeded phi samples. It is under the 5m
// staleness window, so the histogram's own single sample stays visible at
// every evaluation instant while each phi is read at its own.
const openMetricsFloatStep = 10 * time.Second

// openMetricsFloatSweep is the phi values walked. Each names the arm it
// reaches, and the set covers every arm of the formatter:
//
//   - the hardcoded cases 1 / 0 / -1 / NaN / +Inf / -Inf, which
//     Prometheus spells "1.0" / "0.0" / "-1.0" / "NaN" / "+Inf" /
//     "-Inf" rather than through `%g` at all;
//   - the fixed arm, at an integral value (where Go appends ".0" and
//     ClickHouse does not) and at a fractional one;
//   - the scientific arm on both sides, below `1e-4` and at or above
//     `1e6` — the latter being the boundary the two renderers used to
//     disagree about, where ClickHouse still writes fixed notation;
//   - both sides of the two-digit exponent padding, `1e-05` and `1e-15`;
//   - a negative value in each layout, since the magnitude is what
//     ClickHouse renders and the sign is reattached separately.
//
// Values outside [0, 1] are not legal phis for the QUANTILE, but they are
// legal inputs to the LABEL formatter and reference Prometheus formats
// them the same way (it saturates the quantile and warns, and the label
// still carries the phi it was given). The label is what this test reads.
var openMetricsFloatSweep = []float64{
	1,
	0,
	-1,
	math.NaN(),
	math.Inf(1),
	math.Inf(-1),
	0.5,
	-0.25,
	2,
	-3,
	0.0001,
	0.00001,
	1e-15,
	-1e-15,
	999999,
	1e6,
	-1e6,
	1.0098e6,
	1e21,
}

// openMetricsFloatDDL declares both carriers. `CREATE OR REPLACE` because
// the chDB session outlives any one test (see fixture_chdb_test.go).
const openMetricsFloatDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), `BucketCounts` Array(UInt64), `ExplicitBounds` Array(Float64), " +
	"`AggregationTemporality` Int32 DEFAULT 2" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), `Value` Float64" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// TestOpenMetricsFloatLabel_ChDB_MatchesPrometheusFormatter runs one
// `histogram_quantiles(<hist>, "q", scalar(<phi>))` per swept phi and
// asserts the stamped label equals labels.FormatOpenMetricsFloat(phi).
func TestOpenMetricsFloatLabel_ChDB_MatchesPrometheusFormatter(t *testing.T) {
	seed := openMetricsFloatDDL +
		"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES " +
		"('" + openMetricsFloatMetric + "', map('job', 'api'), toDateTime64('" +
		openMetricsFloatBaseline.Format("2006-01-02 15:04:05") + "', 9), [1, 2, 3], [0.1, 0.5, 1.0]);\n"
	phiRows := make([]string, 0, len(openMetricsFloatSweep))
	for i, phi := range openMetricsFloatSweep {
		phiRows = append(phiRows, fmt.Sprintf("('%s', map(), toDateTime64('%s', 9), %s)",
			openMetricsFloatPhiMetric,
			openMetricsFloatPhiAt(i).Format("2006-01-02 15:04:05"),
			chLiteral(phi)))
	}
	seed += "INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n    " +
		strings.Join(phiRows, ",\n    ") + ";\n"
	fixture := newChDBFixture(t, seed)

	for i, phi := range openMetricsFloatSweep {
		want := labels.FormatOpenMetricsFloat(phi)
		got := openMetricsFloatLabelAt(t, fixture, openMetricsFloatPhiAt(i))
		if got != want {
			t.Errorf("phi %v (%s): label = %q, want %q",
				phi, chLiteral(phi), got, want)
		}
	}
}

// openMetricsFloatPhiAt is the instant the i-th swept phi is seeded at,
// and therefore the instant its own query evaluates at.
func openMetricsFloatPhiAt(i int) time.Time {
	return openMetricsFloatBaseline.Add(time.Duration(i) * openMetricsFloatStep)
}

// openMetricsFloatLabelAt lowers and runs the swept call at `at` and
// returns the single stamped label value.
func openMetricsFloatLabelAt(t *testing.T, fixture *chdbFixture, at time.Time) string {
	t.Helper()
	expr, err := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true}).ParseExpr(fmt.Sprintf(
		"histogram_quantiles(%s_bucket, %q, scalar(%s))",
		openMetricsFloatMetric, openMetricsFloatLabel, openMetricsFloatPhiMetric,
	))
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rows, err := fixture.db.Query(
		"SELECT `Attributes`['"+openMetricsFloatLabel+"'] FROM ("+sqlStr+")", args...,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, got)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("at %s: %d rows, want exactly 1 — the seeded phi is not the one being read",
			at.Format(time.RFC3339), len(out))
	}
	return out[0]
}
