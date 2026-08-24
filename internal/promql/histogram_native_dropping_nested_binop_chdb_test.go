//go:build chdb

// chDB-backed proof that a drop-family exp-histogram binop nested as
// ANOTHER binop's own operand (cerberus issue #2534) now lowers to valid
// SQL and EXECUTES against real ClickHouse — not merely that a Go-level
// lowering error stopped being raised.
//
// binary.go's lowerScalarBinopOperand / lowerVectorVectorOperand and
// histogram_native_set_op.go's lowerVectorSetOpOperand are the new
// per-operand opt-ins this issue's fix threads into the generic recursive
// binop lowering; TestLower_ExpHistogram_DroppingShapeComposesUnderWrappers
// (histogram_native_dropping_shape_test.go) already pins the SAME leaf
// composed under a WRAPPER's own argument position (cerberus issue #2528)
// at the Go-plan-shape level — this file is that fixture's chDB-executed,
// binop-nested sibling.
//
// Two metrics are seeded with REAL, non-empty rows — nestedBinopDropMetric
// and nestedBinopDropMetric2 — so a returned zero-row result can only mean
// a drop actually fired, never that no data was ever scanned. The second
// series exists solely for the two-operand cases below, where a single
// metric would let the query collapse to a self-join and hide a
// mismatched-series bug.
//
// Per-query routing (verified by tracing lowerBinary's actual dispatch,
// not assumed from the shape of the query — see the per-query comments in
// TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB below):
//   - `(m1 + 0) * 2`, `(m1 + 0) + 5`, `sum(m1 + 0) + 1`: all three route
//     through lowerVectorScalar → lowerScalarBinopOperand, because the
//     scalar literal on one side is exactly what routes lowerBinary away
//     from lowerVectorVector. None of these three reach
//     lowerVectorVectorOperand — a prior version of this file's header
//     incorrectly claimed the third one did.
//   - `(m1 + 0) or (m1 + 0)`: a set operator, routes through
//     lowerVectorSetOp → lowerVectorSetOpOperand, not lowerVectorVectorOperand
//     either — set operators never reach lowerVectorVector at all
//     (lowerBinary branches on b.Op.IsSetOperator() before ever computing
//     which of lowerVectorScalar / lowerVectorVector applies).
//   - `(m1 < m2) * (m1 < m2)`: the genuine vector-vector case, and the one
//     this file's original four queries never actually reached. `m1 < m2`
//     (no `bool`) is expHistogramDroppingHistogramBinop's own leaf shape
//     (a comparison between two BARE histogram-valued selectors, an
//     unsupported histogram/histogram combo reference drops) — nested as
//     the operand of a further, real vector-vector `*`. Neither outer
//     operand is a scalar literal, so lowerBinary falls to the default
//     lowerVectorVector branch, and BOTH operands separately hit
//     lowerVectorVectorOperand's drop-family match. This is deliberately
//     NOT `(m1 + 0) * (m2 + 0)` (this file's own first draft): that shape,
//     while it DOES route through lowerVectorVectorOperand, would still
//     resolve correctly even with lowerVectorVectorOperand's drop-family
//     check ripped out — `m1 + 0` recurses back into lowerBinary on its
//     own via the generic [lower] fallback and gets caught a second time by
//     the already-covered lowerScalarBinopOperand, silently laundering a
//     hollow test. `m1 < m2` has no such redundant path: with
//     lowerVectorVectorOperand's drop check removed, lowering this exact
//     query regresses to a hard LowerAt error ("... is an exponential
//     histogram metric; only a bare ... selector ... are supported") —
//     verified directly against this worktree before writing this comment,
//     not assumed.
//   - `(m1 == m2) + 1`: exercises lowerScalarBinopOperand's OTHER half —
//     the "preserve" family opt-in (lowerExpHistogramArgAsCanonicalFloat's
//     first branch, lowerExpHistogramValuedShape) rather than the "drop"
//     family. `m1 == m2` (no `bool`) is recognised by
//     expHistogramHistogramCompareBinop as histogram-VALUED, but that
//     recogniser is (deliberately) absent from isExpHistogramValuedShape's
//     own predicate, so lowerRoot's top-level dispatch never claims the
//     WHOLE `(m1 == m2) + 1` expression — it only escapes to
//     lowerVectorScalar → lowerScalarBinopOperand(`m1 == m2`), which then
//     matches via the preserve-family branch and reprojects to the
//     canonical empty float quartet, exactly the way ADD over a real
//     histogram sample must (reference drops the sample; there is no
//     scalar Value to add 1 to).
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	nestedBinopDropMetric  = "nested_binop_drop_probe_exp_hist"
	nestedBinopDropMetric2 = "nested_binop_drop_probe2_exp_hist"
)

var nestedBinopDropSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + nestedBinopDropMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + nestedBinopDropMetric2 + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 7, 14.0, 0, 0, 0, [7], 0, []);\n"

var nestedBinopDropEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB pins cerberus
// issue #2534's own four trigger queries plus two follow-on cases that
// close a coverage gap an adversarial review found in this file's
// original four: none of them actually reached lowerVectorVectorOperand
// (the function this issue added specifically for a drop-family binop
// nested as a genuine vector-vector op's operand), and the "preserve"
// family half of lowerScalarBinopOperand's opt-in was likewise never
// exercised nested. See the per-query routing notes in this file's own
// header comment.
func TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, nestedBinopDropSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	queries := []string{
		// lowerVectorScalar's operand opt-in (a scalar on the outer
		// binop's RHS) — routes through lowerScalarBinopOperand's drop
		// family.
		"(" + nestedBinopDropMetric + " + 0) * 2",
		"(" + nestedBinopDropMetric + " + 0) + 5",
		// lowerVectorScalar's operand opt-in again, this time over an
		// AggregateExpr wrapping the drop-family leaf (composes
		// [lowerExpHistogramDroppingShape]'s own aggregation recursion,
		// cerberus issue #2528, one level deeper) — still
		// lowerScalarBinopOperand, not lowerVectorVectorOperand.
		"sum(" + nestedBinopDropMetric + " + 0) + 1",
		// lowerVectorSetOpOperand's drop-family opt-in.
		"(" + nestedBinopDropMetric + " + 0) or (" + nestedBinopDropMetric + " + 0)",
		// lowerVectorVectorOperand's drop-family opt-in — the genuine
		// vector-vector case, previously untested. Two DISTINCT series so
		// this can't accidentally pass via a self-join, and a comparison
		// (not `+0`) so this can't accidentally pass via the redundant
		// lowerScalarBinopOperand path — see this file's header comment.
		"(" + nestedBinopDropMetric + " < " + nestedBinopDropMetric2 + ") * (" + nestedBinopDropMetric + " < " + nestedBinopDropMetric2 + ")",
		// lowerScalarBinopOperand's PRESERVE-family opt-in, nested — the
		// sibling gap to the drop-family cases above. `m1 == m2` (no
		// `bool`) is histogram-valued but invisible to
		// isExpHistogramValuedShape, so it only resolves via
		// lowerExpHistogramArgAsCanonicalFloat's first branch, reached
		// through lowerScalarBinopOperand.
		"(" + nestedBinopDropMetric + " == " + nestedBinopDropMetric2 + ") + 1",
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, nestedBinopDropEvalTS, nestedBinopDropEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2534's fix): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}

			rows := fixture.queryOverEmitted(t, "count() AS n", sqlStr, args)
			defer func() { _ = rows.Close() }()
			if !rows.Next() {
				t.Fatalf("count() query returned no rows")
			}
			var n int64
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan count(): %v", err)
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != 0 {
				t.Fatalf(
					"query %q returned count()=%d, want 0 — reference drops every sample through an incompatible-type histogram/scalar (or histogram/histogram) binop; the metric(s) were seeded with REAL non-empty rows, so a non-zero count would mean the drop never actually fired at execution",
					query, n,
				)
			}
		})
	}
}
