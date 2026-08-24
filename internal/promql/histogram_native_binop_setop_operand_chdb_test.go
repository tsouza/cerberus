//go:build chdb

// chDB-backed proof that cerberus issue #2559's fix — accepting a
// *chplan.VectorSetOp operand in the `+`/`-`/`==`/`!=` exp-histogram
// binop lowerings, not just a literal *chplan.HistogramProjection — is
// not merely a Go-level type-check loosening: the emitted SQL actually
// EXECUTES against real ClickHouse and produces the correct values.
//
// TestExpHistogramBinopSetOpOperand_MergeDifferentLayout_ChDB mirrors
// histogram_native_mixed_or_vector_hist_merge_chdb_test.go's own
// different-Scale / different-PositiveOffset fixture (reusing the exact
// same numbers) — that file proves the `+`/`-` merge reconciles two
// different bucket layouts when the operands are MIXED float/histogram
// `or` arms; this file proves the same reconciliation when the operands
// are pure (non-mixed) `and` set-ops, the shape #2559 reports.
package promql_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const setOpOperandBinopSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

var setOpOperandBinopEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// setOpOperandBinopRow renders one INSERT VALUES tuple.
func setOpOperandBinopRow(metric, series string, scale int, zeroCount, count int64, sum float64, offset int, buckets string) string {
	return fmt.Sprintf(
		"('%s', map('series', '%s'), toDateTime64('2026-01-01 00:00:00', 9), %d, %s, %d, %d, %d, %s, 0, [])",
		metric, series, count, ftoaLit(sum), scale, zeroCount, offset, buckets,
	)
}

// ftoaLit renders a float64 as a ClickHouse-parseable numeric literal
// with a decimal point, matching the other exp-histogram chDB fixtures'
// own inline `10.0` / `50.0` style.
func ftoaLit(v float64) string {
	return fmt.Sprintf("%g", v)
}

// TestExpHistogramBinopSetOpOperand_MergeDifferentLayout_ChDB seeds two
// `and`-composed operands — `(lhs1 and lhs2) + (rhs1 and rhs2)` — whose
// SURVIVING rows (lhs1's own, rhs1's own — `and` never merges values,
// only filters by label signature) sit at DIFFERENT Scale AND different
// PositiveOffset, so the assertions below only pass if the binop merge
// actually downscales the finer operand's bucket indices onto the
// coarser one's scale before summing.
func TestExpHistogramBinopSetOpOperand_MergeDifferentLayout_ChDB(t *testing.T) {
	const (
		lhs1 = "hb_so_merge_lhs1_exp_hist"
		lhs2 = "hb_so_merge_lhs2_exp_hist"
		rhs1 = "hb_so_merge_rhs1_exp_hist"
		rhs2 = "hb_so_merge_rhs2_exp_hist"
	)
	var seed string
	seed += setOpOperandBinopSeedDDL
	seed += "INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		// LHS `and` pair, series "x": lhs1's own row survives (Scale=0,
		// PositiveOffset=1, buckets=[10,20], matching mvvMergeSeed's LHS).
		"    " + setOpOperandBinopRow(lhs1, "x", 0, 3, 30, 100.0, 1, "[10, 20]") + ",\n" +
		"    " + setOpOperandBinopRow(lhs2, "x", 0, 0, 1, 1.0, 0, "[1]") + ",\n" +
		// RHS `and` pair, series "x": rhs1's own row survives (Scale=1,
		// PositiveOffset=2, buckets=[5,7,9], matching mvvMergeSeed's RHS).
		"    " + setOpOperandBinopRow(rhs1, "x", 1, 4, 21, 50.0, 2, "[5, 7, 9]") + ",\n" +
		"    " + setOpOperandBinopRow(rhs2, "x", 1, 0, 1, 1.0, 0, "[1]") + ";\n"

	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	lhsExpr := "(" + lhs1 + " and " + lhs2 + ")"
	rhsExpr := "(" + rhs1 + " and " + rhs2 + ")"

	run := func(t *testing.T, query string) setOpOperandMergeRow {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, setOpOperandBinopEvalTS, setOpOperandBinopEvalTS)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error (this exact shape used to leak \"internal invariant violated\" — cerberus issue #2559): %v", query, err)
		}
		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", query, err)
		}

		projection := "`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramScale` AS scale, " +
			"`HistogramZeroCount` AS zero_count, " +
			"`HistogramPositiveOffset` AS pos_offset, " +
			"`HistogramPositiveBucketCounts`[1] AS pos_b1, `HistogramPositiveBucketCounts`[2] AS pos_b2, " +
			"`HistogramNegativeOffset` AS neg_offset, length(`HistogramNegativeBucketCounts`) AS neg_len"

		rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			t.Fatalf("query(%q): no rows, want exactly one merged row for series \"x\"", query)
		}
		var r setOpOperandMergeRow
		if err := rows.Scan(&r.cnt, &r.sum, &r.scale, &r.zeroCount, &r.posOffset, &r.posBucket1, &r.posBucket2, &r.negOffset, &r.negBucketsLen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if rows.Next() {
			t.Fatalf("query(%q): got more than one row, want exactly one", query)
		}
		if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		return r
	}

	t.Run("+", func(t *testing.T) {
		r := run(t, lhsExpr+" + "+rhsExpr)
		assertFloatEq(t, "HistogramCount", r.cnt, 30.0+21.0)
		assertFloatEq(t, "HistogramSum", r.sum, 100.0+50.0)
		if r.scale != 0 {
			t.Errorf("HistogramScale = %d, want 0 (min(0,1), the coarser operand's scale)", r.scale)
		}
		assertFloatEq(t, "HistogramZeroCount", r.zeroCount, 3.0+4.0)
		if r.posOffset != 1 {
			t.Errorf("HistogramPositiveOffset = %d, want 1", r.posOffset)
		}
		// index1 = lhs1[1]=10 + rhs1 downscaled (idx2,idx3 -> idx1)=5+7=12 -> 22.
		assertFloatEq(t, "HistogramPositiveBucketCounts[1]", r.posBucket1, 22.0)
		// index2 = lhs1[2]=20 + rhs1 downscaled (idx4 -> idx2)=9 -> 29.
		assertFloatEq(t, "HistogramPositiveBucketCounts[2]", r.posBucket2, 29.0)
		if r.negOffset != 0 || r.negBucketsLen != 0 {
			t.Errorf("negative ladder = (offset=%d, len=%d), want (0, 0)", r.negOffset, r.negBucketsLen)
		}
	})

	t.Run("-", func(t *testing.T) {
		r := run(t, lhsExpr+" - "+rhsExpr)
		assertFloatEq(t, "HistogramCount", r.cnt, 30.0-21.0)
		assertFloatEq(t, "HistogramSum", r.sum, 100.0-50.0)
		if r.scale != 0 {
			t.Errorf("HistogramScale = %d, want 0", r.scale)
		}
		assertFloatEq(t, "HistogramZeroCount", r.zeroCount, 3.0-4.0)
		if r.posOffset != 1 {
			t.Errorf("HistogramPositiveOffset = %d, want 1", r.posOffset)
		}
		// index1 = lhs1[1]=10 - rhs1 downscaled(idx2,idx3->idx1)=5+7=12 -> -2.
		assertFloatEq(t, "HistogramPositiveBucketCounts[1]", r.posBucket1, -2.0)
		// index2 = lhs1[2]=20 - rhs1 downscaled(idx4->idx2)=9 -> 11.
		assertFloatEq(t, "HistogramPositiveBucketCounts[2]", r.posBucket2, 11.0)
	})
}

type setOpOperandMergeRow struct {
	cnt, sum      float64
	scale         int64
	zeroCount     float64
	posOffset     int64
	posBucket1    float64
	posBucket2    float64
	negOffset     int64
	negBucketsLen int64
}

func assertFloatEq(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestExpHistogramBinopSetOpOperand_Equality_ChDB proves the `==`/`!=`
// structural-equality filter over two `and`-composed operands correctly
// keeps/drops each series and forwards the LHS's OWN histogram value
// (not a merge) — pinning both cerberus issue #2559 (the lowering no
// longer errors) and the pre-existing #2273 structural-equality contract
// still holds when the operand is a set-op.
func TestExpHistogramBinopSetOpOperand_Equality_ChDB(t *testing.T) {
	const (
		lhs1 = "hb_so_eq_lhs1_exp_hist"
		lhs2 = "hb_so_eq_lhs2_exp_hist"
		rhs1 = "hb_so_eq_rhs1_exp_hist"
		rhs2 = "hb_so_eq_rhs2_exp_hist"
	)
	var seed string
	seed += setOpOperandBinopSeedDDL
	seed += "INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		// series "same": lhs1 and rhs1 publish BIT-IDENTICAL rows.
		"    " + setOpOperandBinopRow(lhs1, "same", 0, 0, 5, 10.0, 0, "[5]") + ",\n" +
		"    " + setOpOperandBinopRow(lhs2, "same", 0, 0, 1, 1.0, 0, "[1]") + ",\n" +
		"    " + setOpOperandBinopRow(rhs1, "same", 0, 0, 5, 10.0, 0, "[5]") + ",\n" +
		"    " + setOpOperandBinopRow(rhs2, "same", 0, 0, 1, 1.0, 0, "[1]") + ",\n" +
		// series "diff": lhs1 and rhs1 disagree (different Count/Sum/bucket).
		"    " + setOpOperandBinopRow(lhs1, "diff", 0, 0, 5, 10.0, 0, "[5]") + ",\n" +
		"    " + setOpOperandBinopRow(lhs2, "diff", 0, 0, 1, 1.0, 0, "[1]") + ",\n" +
		"    " + setOpOperandBinopRow(rhs1, "diff", 0, 0, 7, 14.0, 0, "[7]") + ",\n" +
		"    " + setOpOperandBinopRow(rhs2, "diff", 0, 0, 1, 1.0, 0, "[1]") + ";\n"

	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	lhsExpr := "(" + lhs1 + " and " + lhs2 + ")"
	rhsExpr := "(" + rhs1 + " and " + rhs2 + ")"

	// series label survives as a Map value, which chdb-go's parquet
	// driver cannot decode directly — read it back via toJSONString, the
	// same workaround emitVectorSetOp's own doc comment describes.
	projection := "`HistogramCount` AS cnt, `HistogramSum` AS sum, " +
		"`HistogramPositiveBucketCounts`[1] AS pos_b1, toJSONString(Attributes) AS attrs_json"

	type row struct {
		cnt, sum float64
		posB1    float64
		attrs    string
	}
	runRows := func(t *testing.T, query string) []row {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, setOpOperandBinopEvalTS, setOpOperandBinopEvalTS)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", query, err)
		}
		rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
		defer func() { _ = rows.Close() }()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.cnt, &r.sum, &r.posB1, &r.attrs); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		return out
	}

	t.Run("==", func(t *testing.T) {
		rows := runRows(t, lhsExpr+" == "+rhsExpr)
		if len(rows) != 1 {
			t.Fatalf("== returned %d rows, want exactly 1 (only series \"same\" structurally matches)", len(rows))
		}
		r := rows[0]
		assertFloatEq(t, "HistogramCount", r.cnt, 5.0)
		assertFloatEq(t, "HistogramSum", r.sum, 10.0)
		assertFloatEq(t, "HistogramPositiveBucketCounts[1]", r.posB1, 5.0)
	})

	t.Run("!=", func(t *testing.T) {
		rows := runRows(t, lhsExpr+" != "+rhsExpr)
		if len(rows) != 1 {
			t.Fatalf("!= returned %d rows, want exactly 1 (only series \"diff\" structurally mismatches)", len(rows))
		}
		r := rows[0]
		// != forwards the LHS's own row unchanged, NOT the RHS's — even
		// though the two disagree.
		assertFloatEq(t, "HistogramCount", r.cnt, 5.0)
		assertFloatEq(t, "HistogramSum", r.sum, 10.0)
		assertFloatEq(t, "HistogramPositiveBucketCounts[1]", r.posB1, 5.0)
	})
}
