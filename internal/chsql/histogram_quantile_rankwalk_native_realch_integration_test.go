//go:build integration

package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// quantilePrometheusHistogramImage pins a ClickHouse version at or above
// chopt.FeatureQuantilePromHistogram's 25.10 floor (see that registry
// entry). A plain literal rather than an import of internal/chopt.
// MinVersion: this test's only dependency on the feature is the version its
// own testcontainers image is pinned to.
const quantilePrometheusHistogramImage = "clickhouse/clickhouse-server:25.10-alpine"

// hqRankWalkDiffCase is one seeded classic-histogram row for the
// differential sweep — a distinct Attributes value (the GroupBy key) paired
// with a (BucketCounts, ExplicitBounds) layout exercising one edge case
// named in chopt.FeatureQuantilePromHistogram's own doc.
type hqRankWalkDiffCase struct {
	name    string
	buckets []uint64
	bounds  []float64
}

var hqRankWalkDiffCases = []hqRankWalkDiffCase{
	// Normal crossing, genuine +Inf overflow rung (cumCount = boundCount+1).
	{name: "normal_overflow", buckets: []uint64{1, 2, 3, 4, 0}, bounds: []float64{1, 2, 4, 8}},
	// Duplicate-bound layout — the one case the aggregate answers WRONG on
	// without the Stage-1 coalescing this emission keeps.
	{name: "duplicate_bound", buckets: []uint64{2, 3, 5, 0}, bounds: []float64{1, 1, 5}},
	// Equal-length shape: no genuine overflow rung (cumCount == boundCount).
	{name: "no_overflow_rung", buckets: []uint64{1, 2, 3, 4}, bounds: []float64{1, 2, 4, 8}},
	// Empty histogram: no buckets at all.
	{name: "empty", buckets: []uint64{}, bounds: []float64{}},
	// Every bucket populated but a first-bucket non-positive upper bound —
	// Prometheus's b==0 && buckets[0].UpperBound<=0 short-circuit.
	{name: "negative_first_bound", buckets: []uint64{2, 3, 5, 0}, bounds: []float64{-5, -1, 3}},
	// Populated buckets but zero total (every count zero) — distinct from
	// "empty": non-empty arrays, still total == 0.
	{name: "all_zero_counts", buckets: []uint64{0, 0, 0, 0, 0}, bounds: []float64{1, 2, 4, 8}},
	// Single finite bucket plus the overflow rung.
	{name: "single_bucket", buckets: []uint64{5, 3}, bounds: []float64{10}},
}

// hqRankWalkDiffPhis covers reference Prometheus's full domain split: below
// range, the two saturating edges, an interior crossing on both sides of
// reverseWalkPhi-style midpoints, and above range.
var hqRankWalkDiffPhis = []float64{-0.5, 0, 0.3, 0.5, 0.95, 1, 1.5}

// TestHistogramQuantile_RankWalkNative_DifferentialRealCH is the
// differential sweep chopt.FeatureQuantilePromHistogram's registry doc
// promises: for every (bucket layout, phi) pair above, the ClickHouse-native
// quantilePrometheusHistogram emission (chplan.HistogramQuantile.
// UseNativeQuantileAggregate) and the hand-rolled rank-walk emission run
// against the SAME real ClickHouse >= 25.10 server and must agree exactly
// (NaN treated as equal to NaN; every other value compared with a tight
// relative tolerance to absorb ULP-level floating rounding order
// differences between the two independently-computed expression chains).
//
// A real >= 25.10 server is required — this repo's shared chDB substrate
// (test-strategy.md) and its testcontainers pin elsewhere in the suite
// (25.9-alpine) both sit BELOW quantilePrometheusHistogram's floor, so this
// is the one lane that can exercise the native path at all; it needs Docker
// and is gated by the `integration` build tag accordingly.
func TestHistogramQuantile_RankWalkNative_DifferentialRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		quantilePrometheusHistogramImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
	})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	assertQuantilePrometheusHistogramPresent(ctx, t, db)

	const table = "hq_rankwalk_diff"
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			Attributes Map(String, String),
			BucketCounts Array(UInt64),
			ExplicitBounds Array(Float64)
		) ENGINE = MergeTree() ORDER BY tuple()
	`, table)); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for _, c := range hqRankWalkDiffCases {
		if _, err := db.ExecContext(
			ctx,
			fmt.Sprintf("INSERT INTO %s (Attributes, BucketCounts, ExplicitBounds) VALUES (?, ?, ?)", table),
			map[string]string{"case": c.name}, c.buckets, c.bounds,
		); err != nil {
			t.Fatalf("insert %s: %v", c.name, err)
		}
	}

	for _, phi := range hqRankWalkDiffPhis {
		phi := phi
		t.Run(fmt.Sprintf("phi=%v", phi), func(t *testing.T) {
			classic := hqRankWalkDiffQuery(table, phi, nil, false)
			native := hqRankWalkDiffQuery(table, phi, nil, true)
			assertHQRankWalkAgree(ctx, t, db, classic, native)
		})
	}

	// PhiExpr path (a runtime-bound parameter, exercising the isNaN(phi)
	// guard both emitters carry only when PhiExpr != nil) — NaN and a
	// well-in-range value.
	for _, phi := range []float64{math.NaN(), 0.5} {
		phi := phi
		t.Run(fmt.Sprintf("phi_expr=%v", phi), func(t *testing.T) {
			phiExpr := &chplan.LitFloat{V: phi}
			classic := hqRankWalkDiffQuery(table, 0, phiExpr, false)
			native := hqRankWalkDiffQuery(table, 0, phiExpr, true)
			assertHQRankWalkAgree(ctx, t, db, classic, native)
		})
	}
}

// assertQuantilePrometheusHistogramPresent probes system.functions directly
// — chopt.FeatureQuantilePromHistogram's registry doc records this probe as
// the source of truth (the function is undocumented as of this writing) —
// so a future ClickHouse release that renames or removes it fails this test
// with a clear message instead of the differential assertions failing
// opaquely.
func assertQuantilePrometheusHistogramPresent(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(
		ctx,
		"SELECT count() > 0 FROM system.functions WHERE name = 'quantilePrometheusHistogram'",
	).Scan(&present); err != nil {
		t.Fatalf("probe system.functions: %v", err)
	}
	if !present {
		t.Fatalf("quantilePrometheusHistogram not present in system.functions on %s — floor probe assumption broken", quantilePrometheusHistogramImage)
	}
}

// hqRankWalkDiffQuery builds the chplan.HistogramQuantile plan over table
// and emits its SQL, native selecting UseNativeQuantileAggregate.
type hqRankWalkDiffQueryResult struct {
	sql  string
	args []any
}

func hqRankWalkDiffQuery(table string, phi float64, phiExpr chplan.Expr, native bool) hqRankWalkDiffQueryResult {
	plan := &chplan.HistogramQuantile{
		Input:                      &chplan.Scan{Table: table},
		Phi:                        phi,
		PhiExpr:                    phiExpr,
		BucketCountsColumn:         "BucketCounts",
		ExplicitBoundsColumn:       "ExplicitBounds",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:             []string{"Attributes"},
		MetricNameColumn:           "MetricName",
		AttributesColumn:           "Attributes",
		TimestampColumn:            "TimeUnix",
		UseNativeQuantileAggregate: native,
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		panic(fmt.Sprintf("emit (native=%v): %v", native, err))
	}
	return hqRankWalkDiffQueryResult{sql: sqlText, args: args}
}

// assertHQRankWalkAgree runs both queries against db and asserts every
// (Attributes, Value) pair agrees, NaN-aware.
func assertHQRankWalkAgree(ctx context.Context, t *testing.T, db *sql.DB, classic, native hqRankWalkDiffQueryResult) {
	t.Helper()
	classicRows, err := hqRankWalkQueryRows(ctx, db, classic)
	if err != nil {
		t.Fatalf("classic query: %v\nSQL: %s", err, classic.sql)
	}
	nativeRows, err := hqRankWalkQueryRows(ctx, db, native)
	if err != nil {
		t.Fatalf("native query: %v\nSQL: %s", err, native.sql)
	}
	if len(classicRows) != len(nativeRows) {
		t.Fatalf("row count mismatch: classic=%d native=%d\nclassic SQL: %s\nnative SQL: %s",
			len(classicRows), len(nativeRows), classic.sql, native.sql)
	}
	for i := range classicRows {
		c, n := classicRows[i], nativeRows[i]
		if c.attrs != n.attrs {
			t.Fatalf("row %d attrs mismatch: classic=%q native=%q", i, c.attrs, n.attrs)
		}
		if !hqRankWalkValuesAgree(c.value, n.value) {
			t.Errorf("case %q: classic=%v native=%v (mismatch)\nclassic SQL: %s\nnative SQL: %s",
				c.attrs, c.value, n.value, classic.sql, native.sql)
		}
	}
}

type hqRankWalkRow struct {
	attrs string
	value float64
}

func hqRankWalkQueryRows(ctx context.Context, db *sql.DB, q hqRankWalkDiffQueryResult) ([]hqRankWalkRow, error) {
	rows, err := db.QueryContext(ctx, q.sql, q.args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []hqRankWalkRow
	for rows.Next() {
		var attrs map[string]string
		var value float64
		if err := rows.Scan(&attrs, &value); err != nil {
			return nil, err
		}
		out = append(out, hqRankWalkRow{attrs: attrs["case"], value: value})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].attrs < out[j].attrs })
	return out, nil
}

// hqRankWalkTolerance is the relative tolerance the differential comparison
// allows — two independently-derived floating-point expression chains
// (arrayCumSum-based interpolation vs. the aggregate's own internal walk)
// are not guaranteed bit-identical even when both are correct, only
// equal to within a few ULPs. Mirrors this repo's own documented precedent
// for small ULP tolerances on other quantile differential paths (the #2403
// backward-accumulation citation in chopt.FeatureQuantilePromHistogram's
// own doc) rather than asserting exact equality.
const hqRankWalkTolerance = 1e-9

func hqRankWalkValuesAgree(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsInf(a, 1) && math.IsInf(b, 1) {
		return true
	}
	if math.IsInf(a, -1) && math.IsInf(b, -1) {
		return true
	}
	if a == b {
		return true
	}
	denom := math.Max(math.Abs(a), math.Abs(b))
	if denom == 0 {
		return true
	}
	return math.Abs(a-b)/denom <= hqRankWalkTolerance
}
