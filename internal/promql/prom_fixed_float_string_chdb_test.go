//go:build chdb

// chDB-backed proof that the two label values cerberus derives from a
// runtime float — `count_values`' synthetic label and a classic
// histogram's `le` — are spelled the way Prometheus spells them, and
// not the way ClickHouse's own `toString(Float64)` does.
//
// The expectation is computed in the test from
// `strconv.FormatFloat(v, 'f', -1, 64)`, the Go standard-library call
// both reference sites make:
//
//   - `promql/engine.go`'s aggregationCountValues:
//     `enh.lb.Set(valueLabel, strconv.FormatFloat(s.F, 'f', -1, 64))`.
//   - `storage/remote/otlptranslator/prometheusremotewrite/helper.go`,
//     the code that explodes an OTel explicit-bucket histogram into
//     classic `<name>_bucket{le="…"}` series:
//     `boundStr := strconv.FormatFloat(bound, 'f', -1, 64)`.
//
// Nothing in the assertion restates a literal cerberus produced, so it
// cannot be satisfied by regenerating anything.
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
	"github.com/tsouza/cerberus/internal/testsql"
)

// promFloatLabelSeedDDL declares both carriers the two cases below read.
// Seeds spell their tables `CREATE OR REPLACE TABLE` because the chDB
// session outlives any one test (see fixture_chdb_test.go).
const promFloatLabelSeedDDL = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

const (
	// Metric names distinct from every sibling fixture sharing this
	// package's chDB session, so neither case can read another's rows.
	promFloatLabelGaugeMetric = "prom_fixed_float_label_gauge_metric"
	promFloatLabelHistMetric  = "prom_fixed_float_label_hist_metric"
)

var (
	promFloatLabelSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// One second after the sample, well inside the 5m staleness window.
	promFloatLabelEvalTS = promFloatLabelSampleTS.Add(time.Second)
)

// promFloatLabelDivergentValues spans BOTH sides of the disagreement:
//
//   - rows where CH's `toString` and Go's `'f'` already AGREE
//     (1e6, 0.3, 0, 1e20, 1e-6) — these are what stop the test from
//     passing on an implementation that expanded everything, or that
//     hard-coded the divergent spellings.
//   - rows where they do NOT (NaN, ±Inf, 1e21, 1e-7, 1.2345e22,
//     -1.5e-9) — CH writes `nan` / `inf` / `-inf` / `1e21` / `1e-7` /
//     `1.2345e22` / `-1.5e-9`, Go writes `NaN` / `+Inf` / `-Inf` and
//     the positional expansions.
//
// Negative zero is in the list because it is the one value whose sign
// survives only if the formatter reads CH's rendering rather than
// testing `v < 0`.
var promFloatLabelDivergentValues = []float64{
	math.NaN(),
	math.Inf(1),
	math.Inf(-1),
	1e21,
	-1e21,
	1e-7,
	1.2345e22,
	-1.5e-9,
	1e6,
	1e20,
	1e-6,
	0.3,
	0,
	math.Copysign(0, -1),
}

// chLiteral renders v as a ClickHouse Float64 literal for the seed.
// Special values have their own spelling in CH's parser.
//
// The finite arm uses POSITIONAL notation deliberately. ClickHouse's
// literal parser evaluates an exponent-form literal arithmetically
// rather than converting the decimal exactly — `SELECT
// toString(-1.5e-09::Float64)` answers `-1.5000000000000002e-9` — so an
// exponent-form seed would store a DIFFERENT double from the one the
// case names, and the test would be measuring CH's literal parser
// instead of cerberus's label formatter. The positional spelling of the
// same value round-trips exactly.
func chLiteral(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	case math.Signbit(v) && v == 0:
		return "-0.0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// promFloatLabelBucketBounds are the finite divergent magnitudes the
// `le` case seeds, ascending as ExplicitBounds requires. The non-finite
// members of [promFloatLabelDivergentValues] are absent because the
// classic-histogram pre-filter drops non-finite bounds before the
// fanout ever sees them; the +Inf overflow rung is spelled by
// bucketBoundInfLabel instead and is asserted alongside them.
var promFloatLabelBucketBounds = []float64{1e-7, 1e-6, 0.3, 1e6, 1e20, 1e21, 1.2345e22}

// promFloatLabelSeed builds the one seed both cases share. They share it
// because the chDB session is process-wide: two fixtures issuing
// `CREATE OR REPLACE` for the same table would wipe each other's rows.
func promFloatLabelSeed() string {
	var seed strings.Builder
	seed.WriteString(promFloatLabelSeedDDL)

	seed.WriteString("INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ")
	for i, v := range promFloatLabelDivergentValues {
		if i > 0 {
			seed.WriteString(",")
		}
		fmt.Fprintf(&seed, "('%s', map('instance', 'i-%d'), toDateTime64('%s', 9), %s)",
			promFloatLabelGaugeMetric, i,
			promFloatLabelSampleTS.Format("2006-01-02 15:04:05"),
			chLiteral(v))
	}
	seed.WriteString(";")

	boundLits := make([]string, len(promFloatLabelBucketBounds))
	counts := make([]string, len(promFloatLabelBucketBounds)+1)
	for i, b := range promFloatLabelBucketBounds {
		boundLits[i] = chLiteral(b)
		counts[i] = "1"
	}
	counts[len(counts)-1] = "1"
	fmt.Fprintf(&seed,
		"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES "+
			"('%s', map('job', 'api'), toDateTime64('%s', 9), [%s], [%s]);",
		promFloatLabelHistMetric,
		promFloatLabelSampleTS.Format("2006-01-02 15:04:05"),
		strings.Join(counts, ","), strings.Join(boundLits, ","))

	return seed.String()
}

// TestPromFixedFloatLabelsMatchGo drives both label-producing sites over
// the same seed and compares each against Go's own formatting.
func TestPromFixedFloatLabelsMatchGo(t *testing.T) {
	fixture := newChDBFixture(t, promFloatLabelSeed())

	// count_values: reference Prometheus's own call at promql/engine.go's
	// aggregationCountValues.
	t.Run("count_values", func(t *testing.T) {
		query := fmt.Sprintf(`count_values("v", %s)`, promFloatLabelGaugeMetric)
		got := promFloatLabelStrings(t, fixture, query, "`Attributes`['v']")

		// -0 and 0 are distinct label values upstream ("-0" vs "0") and
		// distinct groups here, so the want set does not collapse them.
		want := make([]string, 0, len(promFloatLabelDivergentValues))
		for _, v := range promFloatLabelDivergentValues {
			want = append(want, strconv.FormatFloat(v, 'f', -1, 64))
		}
		sort.Strings(want)
		assertLabelSet(t, "count_values", got, want)
	})

	// le: the call Prometheus's own OTLP receiver makes when it explodes
	// the SAME OTel histogram into classic bucket series.
	t.Run("le", func(t *testing.T) {
		got := promFloatLabelStrings(t, fixture, promFloatLabelHistMetric+"_bucket", "`Attributes`['le']")

		want := make([]string, 0, len(promFloatLabelBucketBounds)+1)
		for _, b := range promFloatLabelBucketBounds {
			want = append(want, strconv.FormatFloat(b, 'f', -1, 64))
		}
		want = append(want, "+Inf")
		sort.Strings(want)
		assertLabelSet(t, "le", got, want)
	})
}

// promFloatLabelStrings lowers + emits query, runs it against fixture,
// and returns the sorted values of the given projection.
func promFloatLabelStrings(t *testing.T, fixture *chdbFixture, query, projection string) []string {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, promFloatLabelEvalTS, promFloatLabelEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// assertLabelSet compares two sorted label slices element-wise, naming
// the first difference.
func assertLabelSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s labels = %q (%d); want %q (%d)", label, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s label[%d] = %q; want %q (strconv.FormatFloat(_, 'f', -1, 64))", label, i, got[i], want[i])
		}
	}
}
