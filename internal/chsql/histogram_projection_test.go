package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmit_HistogramProjection_NilInput rejects an IR node with no
// Input — the emitter would otherwise dereference nil while trying to
// recurse for the subquery.
func TestEmit_HistogramProjection_NilInput(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramProjection{
		CountColumn: "Count",
		SumColumn:   "Sum",
		ScaleColumn: "Scale",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("Emit(HistogramProjection with nil Input) returned nil error")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected wrapped ErrUnsupported; got %v", err)
	}
}

// TestEmit_HistogramProjection_MissingColumns covers the column-name
// validation: an IR node missing any of the required exp-histogram
// column names must error rather than producing a query referencing an
// empty identifier.
func TestEmit_HistogramProjection_MissingColumns(t *testing.T) {
	t.Parallel()

	base := &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		ZeroThresholdColumn:        "ZeroThreshold",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
	}

	cases := []struct {
		name string
		mut  func(*chplan.HistogramProjection)
	}{
		{"missing Count", func(h *chplan.HistogramProjection) { h.CountColumn = "" }},
		{"missing Sum", func(h *chplan.HistogramProjection) { h.SumColumn = "" }},
		{"missing Scale", func(h *chplan.HistogramProjection) { h.ScaleColumn = "" }},
		{"missing ZeroCount", func(h *chplan.HistogramProjection) { h.ZeroCountColumn = "" }},
		{"missing PositiveOffset", func(h *chplan.HistogramProjection) { h.PositiveOffsetColumn = "" }},
		{"missing PositiveBucketCounts", func(h *chplan.HistogramProjection) { h.PositiveBucketCountsColumn = "" }},
		{"missing NegativeOffset", func(h *chplan.HistogramProjection) { h.NegativeOffsetColumn = "" }},
		{"missing NegativeBucketCounts", func(h *chplan.HistogramProjection) { h.NegativeBucketCountsColumn = "" }},
		// ZeroThresholdColumn is intentionally NOT in this list — see
		// TestEmit_HistogramProjection_NoZeroThresholdColumn.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := *base
			tc.mut(&h)
			_, _, err := chsql.Emit(context.Background(), &h)
			if err == nil {
				t.Fatalf("Emit returned nil error for %s", tc.name)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("expected wrapped ErrUnsupported; got %v", err)
			}
		})
	}
}

// TestEmit_HistogramProjection_NoZeroThresholdColumn pins the
// constant-zero configuration: an empty ZeroThresholdColumn means the
// physical schema does not persist the OTLP zero_threshold field (the
// default OTel-CH exp-histogram DDL doesn't), so the emitted SQL must
// reference no ZeroThreshold identifier and project a literal 0. under
// the HistogramZeroThreshold alias instead — the output shape stays
// nine columns regardless of what the input schema persists.
func TestEmit_HistogramProjection_NoZeroThresholdColumn(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(HistogramProjection without ZeroThresholdColumn): %v", err)
	}
	if strings.Contains(sql, "ZeroThreshold") && !strings.Contains(sql, "HistogramZeroThreshold") {
		t.Errorf("emitted SQL references a bare ZeroThreshold identifier despite the schema persisting none:\n%s", sql)
	}
	if !strings.Contains(sql, "0. AS `HistogramZeroThreshold`") {
		t.Errorf("emitted SQL does not project the constant-0 HistogramZeroThreshold column:\n%s", sql)
	}
}

// TestEmit_HistogramProjection_ShapeSanity emits SQL for a well-formed
// IR node and asserts the outer SELECT carries every GroupBy alias plus
// all nine fixed Histogram*Column output aliases, reading each source
// column through — the "publish as final output columns instead of
// collapsing to a scalar" contract HistogramProjection exists for.
func TestEmit_HistogramProjection_ShapeSanity(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		ZeroThresholdColumn:        "ZeroThreshold",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:             []string{"Attributes"},
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(HistogramProjection): %v", err)
	}

	wantTokens := []string{
		"`Attributes`",
		"`Count` AS `HistogramCount`",
		"`Sum` AS `HistogramSum`",
		"`Scale` AS `HistogramScale`",
		"`ZeroThreshold` AS `HistogramZeroThreshold`",
		"`ZeroCount` AS `HistogramZeroCount`",
		"`PositiveOffset` AS `HistogramPositiveOffset`",
		"`PositiveBucketCounts` AS `HistogramPositiveBucketCounts`",
		"`NegativeOffset` AS `HistogramNegativeOffset`",
		"`NegativeBucketCounts` AS `HistogramNegativeBucketCounts`",
		"otel_metrics_exponential_histogram",
	}
	for _, tok := range wantTokens {
		if !strings.Contains(sql, tok) {
			t.Errorf("emitted SQL missing token %q:\n%s", tok, sql)
		}
	}
}
