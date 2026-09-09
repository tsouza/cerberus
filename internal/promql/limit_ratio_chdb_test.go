//go:build chdb

// chDB-backed parity pin for the PromQL `limit_ratio` experimental
// aggregator. `limit_ratio(r, v)` deterministically samples ~|r| of the
// input series by comparing a per-series hash offset against the ratio
// threshold (reference: prometheus/promql/engine.go HashRatioSampler):
//
//	offset(series) = float64(labels.Hash()) / float64(math.MaxUint64)
//	keep when r >= 0: offset <  r
//	keep when r <  0: offset >= 1 + r        (the complement of |r|)
//
// Byte-for-byte parity hinges on cerberus reproducing the EXACT hash the
// reference engine computes. On the default `stringlabels` build,
// `labels.Hash()` is `xxhash.Sum64` over the label set sorted by name and
// encoded as len-prefix(name)+name+len-prefix(value)+value per label.
// cerberus reconstructs that byte string in ClickHouse from the
// Attributes map (plus `__name__` restored from MetricName) and hashes it
// with CH's `xxHash64`, which is byte-identical to cespare/xxhash/v2.
//
// This test does NOT go through the test/spec round-trip harness: that
// harness rewrites Map projections to `toJSONString(Attributes)` for the
// chdb-go parquet driver, and that rewrite is incompatible with the
// WHERE-clause map operations the ratio offset needs (CH mis-dispatches
// `concat` to `arrayConcat`). Instead it lowers + emits the production
// SQL directly, runs it against the package-shared chDB session seeded
// with a known series set, and asserts the surviving `instance` label set
// matches the set computed independently in Go via the real
// `prometheus/model/labels` Hash — the reference behaviour itself.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package promql_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	prommodel "github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// seriesInstances is the deterministic series corpus: five `up` series
// distinguished only by their `instance` label. The job label is fixed
// so the only varying byte in the encoded label set is the instance
// value — keeps the reference offsets easy to reason about.
var seriesInstances = []string{"i0", "i1", "i2", "i3", "i4"}

// refOffset reproduces HashRatioSampler.SampleOffset for the `up` series
// with the given instance label using the real Prometheus labels.Hash().
func refOffset(instance string) float64 {
	ls := prommodel.FromStrings("__name__", "up", "instance", instance, "job", "demo")
	return float64(ls.Hash()) / float64(uint64(math.MaxUint64))
}

// refSelected returns the instance set the reference engine keeps for the
// given ratio, sorted.
func refSelected(ratio float64) []string {
	var out []string
	for _, inst := range seriesInstances {
		off := refOffset(inst)
		keep := (ratio >= 0 && off < ratio) || (ratio < 0 && off >= 1.0+ratio)
		if keep {
			out = append(out, inst)
		}
	}
	sort.Strings(out)
	return out
}

func TestLimitRatio_ChDBParity(t *testing.T) {
	fixture := newChDBFixture(t, limitRatioSeed())

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, ratio := range []float64{0.5, -0.5, 1.0, -1.0, 0.0} {
		ratio := ratio
		t.Run(fmt.Sprintf("ratio=%g", ratio), func(t *testing.T) {
			query := fmt.Sprintf("limit_ratio(%g, up)", ratio)
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			got := selectInstances(t, fixture, sqlStr, args)
			want := refSelected(ratio)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("limit_ratio(%g) selected %v; reference HashRatioSampler selects %v\n(offsets: %s)",
					ratio, got, want, offsetsDebug())
			}
		})
	}
}

// offsetsDebug renders each series' reference offset, for failure output.
func offsetsDebug() string {
	var b strings.Builder
	for _, inst := range seriesInstances {
		fmt.Fprintf(&b, "%s=%.6f ", inst, refOffset(inst))
	}
	return strings.TrimSpace(b.String())
}

// TestLimitRatio_ChDBParity_ComputedRatio pins the computed-ratio path
// (`limit_ratio(scalar(vector(r)), v)`): the ratio's sign isn't known at
// plan time, so the lowering emits the full
// `(r>=0 AND off<r) OR (r<0 AND off>=1+r)` runtime predicate. The
// selected set must match the literal-ratio reference for the same r.
func TestLimitRatio_ChDBParity_ComputedRatio(t *testing.T) {
	fixture := newChDBFixture(t, limitRatioSeed())

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		query string
		ratio float64
	}{
		{"limit_ratio(scalar(vector(0.5)), up)", 0.5},
		{"limit_ratio(scalar(vector(-0.5)), up)", -0.5},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			got := selectInstances(t, fixture, sqlStr, args)
			want := refSelected(tc.ratio)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s selected %v; reference (r=%g) selects %v\n(offsets: %s)",
					tc.query, got, tc.ratio, want, offsetsDebug())
			}
		})
	}
}

// limitRatioSeed declares BOTH arms of the metrics fan-out the emitted
// SQL scans — `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')`
// — even though every `up` row lands in the gauge table. merge() takes
// its column set from the union of the tables that match, so a seed
// missing an arm silently borrows that arm from whichever sibling fixture
// last ran in the process-shared chDB session, and the fixture then fails
// on its own under a narrowed `-run`.
// [chdbFixture.queryOverEmitted] asserts the arms are all present here.
//
// ResourceAttributes (DEFAULT map()) mirrors the OTel-CH default schema:
// the read path projects mapUpdate(sanitize(ResourceAttributes), …) and
// toString(ServiceName) unconditionally, so both columns must exist or
// the chDB round-trip fails with UNKNOWN_IDENTIFIER. The INSERTs stay
// column-explicit (naming neither) so the DEFAULTs fill them.
//
// CREATE OR REPLACE keeps the seed idempotent against that shared
// session, which outlives any single test.
const limitRatioSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// limitRatioSeed is the DDL plus the five-instance `up` corpus and the
// long-label `up_long` corpus, all of which lands in the gauge table.
func limitRatioSeed() string {
	var b strings.Builder
	b.WriteString(limitRatioSeedDDL)
	for _, inst := range seriesInstances {
		fmt.Fprintf(&b,
			"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ('up', map('instance', '%s', 'job', 'demo'), toDateTime64('2026-01-01 00:00:00', 9), 1.0);\n",
			inst)
	}
	for _, size := range longLabelValueSizes {
		fmt.Fprintf(&b,
			"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ('%s', map('instance', '%s', 'detail', '%s'), toDateTime64('2026-01-01 00:00:00', 9), 1.0);\n",
			longLabelMetric, longLabelInstance(size), longLabelValue(size))
	}
	return b.String()
}

// longLabelValueSizes straddles Prometheus's stringlabels size-prefix
// boundary (`v < 255` is one byte; 255 and up is `0xFF` + 3 little-endian
// bytes — model/labels/labels_stringlabels.go:552-564) from both sides,
// including the two values immediately either side of it and one that
// exercises the second little-endian byte.
var longLabelValueSizes = []int{100, 254, 255, 256, 1000}

// longLabelMetric names the long-label corpus. A distinct metric name
// keeps it invisible to the `up` cases above, which share the table.
const longLabelMetric = "up_long"

// longLabelValue is the `detail` label value of the given byte length.
// The content is irrelevant to the encoding — only the SIZE selects
// encodeSize's branch — so a single repeated ASCII byte keeps the seed
// readable and the byte length equal to the rune length.
func longLabelValue(size int) string { return strings.Repeat("a", size) }

// longLabelInstance is the short, human-readable series key projected
// back out of the query; the long bytes live in `detail`.
func longLabelInstance(size int) string { return fmt.Sprintf("L%d", size) }

// refSelectedLong is [refSelected] for the long-label corpus: the same
// HashRatioSampler rule, over label sets whose `detail` value crosses
// the size-prefix boundary.
func refSelectedLong(ratio float64) []string {
	var out []string
	for _, size := range longLabelValueSizes {
		ls := prommodel.FromStrings(
			"__name__", longLabelMetric,
			"detail", longLabelValue(size),
			"instance", longLabelInstance(size),
		)
		off := float64(ls.Hash()) / float64(uint64(math.MaxUint64))
		if (ratio >= 0 && off < ratio) || (ratio < 0 && off >= 1.0+ratio) {
			out = append(out, longLabelInstance(size))
		}
	}
	sort.Strings(out)
	return out
}

// TestLimitRatio_ChDBParity_LongLabelValues pins the OTHER branch of
// Prometheus's stringlabels size prefix.
//
// `encodeSize` (model/labels/labels_stringlabels.go:552-564) writes a
// single byte only while the size is `< 255`; at 255 and above it writes
// the escape byte `0xFF` followed by three little-endian bytes. Because
// `labels.Hash()` is `xxhash.Sum64` over exactly those bytes
// (labels_stringlabels.go:90-92), a label value of 255 bytes or more
// hashes to a DIFFERENT offset than a single-byte prefix produces — and
// `limit_ratio` therefore keeps a different subset of series.
//
// OTel attribute values reach these sizes routinely (`http.url`,
// `db.statement`, `exception.message`), so this is a live wire-format
// divergence, not a theoretical one. The corpus straddles the boundary
// from both sides so the test also fails if the comparison drifts to
// `<= 255`.
func TestLimitRatio_ChDBParity_LongLabelValues(t *testing.T) {
	fixture := newChDBFixture(t, limitRatioSeed())

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, ratio := range []float64{0.25, 0.5, 0.75, -0.5} {
		ratio := ratio
		t.Run(fmt.Sprintf("ratio=%g", ratio), func(t *testing.T) {
			query := fmt.Sprintf("limit_ratio(%g, %s)", ratio, longLabelMetric)
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			got := selectInstances(t, fixture, sqlStr, args)
			want := refSelectedLong(ratio)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("limit_ratio(%g, %s) selected %v; reference HashRatioSampler selects %v",
					ratio, longLabelMetric, got, want)
			}
		})
	}
}

// selectInstances runs the lowered Sample-shape SQL and returns the
// sorted set of surviving `instance` labels.
func selectInstances(t *testing.T, fixture *chdbFixture, sqlStr string, args []any) []string {
	t.Helper()
	rows := fixture.queryOverEmitted(t, "`Attributes`['instance']", sqlStr, args)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var inst string
		if err := rows.Scan(&inst); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, inst)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	return got
}
