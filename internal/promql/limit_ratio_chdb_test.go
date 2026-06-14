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
// SQL directly, runs it against an ephemeral chDB session seeded with a
// known series set, and asserts the surviving `instance` label set
// matches the set computed independently in Go via the real
// `prometheus/model/labels` Hash — the reference behaviour itself.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package promql_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
	promparser "github.com/prometheus/prometheus/promql/parser"

	prommodel "github.com/prometheus/prometheus/model/labels"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
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
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	seedLimitRatioCorpus(t, db)

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
			got := selectInstances(t, db, sqlStr, args)
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
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	seedLimitRatioCorpus(t, db)

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
			got := selectInstances(t, db, sqlStr, args)
			want := refSelected(tc.ratio)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s selected %v; reference (r=%g) selects %v\n(offsets: %s)",
					tc.query, got, tc.ratio, want, offsetsDebug())
			}
		})
	}
}

// seedLimitRatioCorpus creates the gauge + sum metric tables and inserts
// the five-instance `up` corpus into the gauge table.
func seedLimitRatioCorpus(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`)"); err != nil {
		t.Fatalf("create gauge: %v", err)
	}
	if _, err := db.Exec("CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`)"); err != nil {
		t.Fatalf("create sum: %v", err)
	}
	for _, inst := range seriesInstances {
		ins := fmt.Sprintf(
			"INSERT INTO otel_metrics_gauge VALUES ('up', map('instance', '%s', 'job', 'demo'), toDateTime64('2026-01-01 00:00:00', 9), 1.0)", inst,
		)
		if _, err := db.Exec(ins); err != nil {
			t.Fatalf("seed %s: %v", inst, err)
		}
	}
}

// selectInstances runs the lowered Sample-shape SQL and returns the
// sorted set of surviving `instance` labels.
func selectInstances(t *testing.T, db *sql.DB, sqlStr string, args []any) []string {
	t.Helper()
	wrapped := "SELECT `Attributes`['instance'] FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, wrapped)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var inst string
		if err := rows.Scan(&inst); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, inst)
	}
	if err := rows.Err(); err != nil && !strings.Contains(err.Error(), "empty row") {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	return got
}
