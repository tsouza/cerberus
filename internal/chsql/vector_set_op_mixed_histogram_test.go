package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// mixedSetOpHistogramArm builds a HistogramProjection-shaped arm — the
// same shape internal/promql's histogram-valued bare-selector /
// sum()/avg() / range-function lowerings cap with, and the shape a
// mixed `and`/`unless` operand ends up as when it IS the histogram side
// (cerberus issue #2325). GroupBy/GroupByAliases publish the canonical
// (MetricName, Attributes, TimeUnix, Value) quartet FIRST, exactly as
// [nativeHistogramProjection] in internal/promql does, so this is a
// faithful stand-in for a real lowering's output.
func mixedSetOpHistogramArm() *chplan.HistogramProjection {
	return &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		GroupBy: []chplan.Expr{
			&chplan.LitString{V: "http_server_duration_exp_hist"},
			&chplan.ColumnRef{Name: "Attributes"},
			&chplan.ColumnRef{Name: "TimeUnix"},
			&chplan.LitFloat{V: 0},
		},
		GroupByAliases: []string{"MetricName", "Attributes", "TimeUnix", "Value"},
	}
}

// mixedSetOpFloatArm builds a plain canonical float-shaped arm — a bare
// Scan, which already publishes the (MetricName, Attributes, TimeUnix,
// Value) quartet as real physical columns, standing in for
// histogram_quantile()'s own float-valued output.
func mixedSetOpFloatArm() *chplan.Scan {
	return &chplan.Scan{Table: "otel_metrics_gauge"}
}

func mixedSetOp(op chplan.VectorSetOpKind, left, right chplan.Node, histogram bool) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left:             left,
		Right:            right,
		Op:               op,
		Histogram:        histogram,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

// TestEmitVectorSetOp_Mixed_FloatLeftHistogramRight pins the shape a
// mixed `and`/`unless` emits when the FLOAT operand is on the LEFT
// (cerberus issue #2325): the output (outer SELECT, and the LEFT arm's
// own canonicalisation) carries just the four-column Sample shape — no
// Histogram*Column reference — because the histogram-valued RIGHT arm
// is used ONLY for its label-signature membership test, never as a
// source of output columns. Before this fix the per-arm canonicalisation
// followed the single node-level Histogram flag for BOTH arms, which
// would have referenced Histogram*Column on this float LEFT arm and
// broken it, or omitted them from the histogram RIGHT arm — harmless
// there, but the wrong reasoning either way.
func TestEmitVectorSetOp_Mixed_FloatLeftHistogramRight(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.VectorSetOpKind{chplan.VectorSetAnd, chplan.VectorSetUnless} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			plan := mixedSetOp(op, mixedSetOpFloatArm(), mixedSetOpHistogramArm(), false /*Histogram*/)
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: unexpected error: %v", err)
			}
			// The RIGHT (histogram) arm's own canonicalisation legitimately
			// references HistogramCount deep inside the sig-only membership
			// subquery — that's the arm's OWN real shape, harmlessly unused
			// beyond its Attributes column (see vectorSetOpCanonicalArmFrag's
			// doc comment). Only the OUTERMOST SELECT — the actual query
			// output — must stay float-only.
			outerSelect := sql[:strings.Index(sql, " FROM ")]
			if strings.Contains(outerSelect, "Histogram") {
				t.Errorf("mixed float-LHS %s outer SELECT references a Histogram*Column, want float-only output:\n%s", op, outerSelect)
			}
			if !strings.Contains(sql, "`otel_metrics_gauge`") {
				t.Errorf("mixed float-LHS %s SQL doesn't scan the float table:\n%s", op, sql)
			}
			if !strings.Contains(sql, "`otel_metrics_exponential_histogram`") {
				t.Errorf("mixed float-LHS %s SQL doesn't scan the histogram table for its membership test:\n%s", op, sql)
			}
		})
	}
}

// TestEmitVectorSetOp_Mixed_HistogramLeftFloatRight is the mirror of
// TestEmitVectorSetOp_Mixed_FloatLeftHistogramRight: the histogram
// operand on the LEFT, so the output DOES carry the nine
// Histogram*Column outputs, while the float RIGHT arm — used only for
// its label signature — is referenced with no Histogram*Column at all
// (it publishes none).
func TestEmitVectorSetOp_Mixed_HistogramLeftFloatRight(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.VectorSetOpKind{chplan.VectorSetAnd, chplan.VectorSetUnless} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			plan := mixedSetOp(op, mixedSetOpHistogramArm(), mixedSetOpFloatArm(), true /*Histogram*/)
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: unexpected error: %v", err)
			}
			if !strings.Contains(sql, "HistogramCount") {
				t.Errorf("mixed histogram-LHS %s SQL doesn't reference HistogramCount, want histogram output:\n%s", op, sql)
			}
			if !strings.Contains(sql, "`otel_metrics_gauge`") {
				t.Errorf("mixed histogram-LHS %s SQL doesn't scan the float table for its membership test:\n%s", op, sql)
			}
			if !strings.Contains(sql, "`otel_metrics_exponential_histogram`") {
				t.Errorf("mixed histogram-LHS %s SQL doesn't scan the histogram table:\n%s", op, sql)
			}
		})
	}
}
