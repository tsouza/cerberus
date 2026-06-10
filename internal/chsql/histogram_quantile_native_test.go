package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmit_HistogramQuantileNative_NilInput rejects an IR node with no
// Input — the emitter would otherwise dereference nil while trying to
// recurse for the subquery.
func TestEmit_HistogramQuantileNative_NilInput(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramQuantileNative{
		Phi:                        0.95,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		ZeroThresholdColumn:        "ZeroThreshold",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("Emit(HistogramQuantileNative with nil Input) returned nil error")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected wrapped ErrUnsupported; got %v", err)
	}
}

// TestEmit_HistogramQuantileNative_NoZeroThresholdColumn pins the
// constant-zero zero-bucket configuration: an empty
// ZeroThresholdColumn means the physical schema does not persist the
// OTLP zero_threshold field (the upstream OTel-CH exp-histogram DDL
// doesn't), so the emitted SQL must reference no ZeroThreshold
// identifier and use a literal 0. for the zero-bucket width instead.
func TestEmit_HistogramQuantileNative_NoZeroThresholdColumn(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramQuantileNative{
		Input:                      &chplan.Scan{Table: "otel_metrics_exp_histogram"},
		Phi:                        0.95,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(HistogramQuantileNative without ZeroThresholdColumn): %v", err)
	}
	if strings.Contains(sql, "ZeroThreshold") {
		t.Errorf("emitted SQL references ZeroThreshold despite the schema persisting none:\n%s", sql)
	}
	if !strings.Contains(sql, "-0. + 2 * 0. *") {
		t.Errorf("emitted SQL does not render the constant-0 zero-bucket interpolation:\n%s", sql)
	}
}

// TestEmit_HistogramQuantileNative_MissingColumns covers the column-name
// validation: an IR node missing any of the required exp-histogram
// column names must error rather than producing a query referencing
// empty identifiers.
func TestEmit_HistogramQuantileNative_MissingColumns(t *testing.T) {
	t.Parallel()

	base := &chplan.HistogramQuantileNative{
		Input:                      &chplan.Scan{Table: "otel_metrics_exp_histogram"},
		Phi:                        0.95,
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
		mut  func(*chplan.HistogramQuantileNative)
	}{
		{"missing PositiveBucketCounts", func(h *chplan.HistogramQuantileNative) { h.PositiveBucketCountsColumn = "" }},
		{"missing PositiveOffset", func(h *chplan.HistogramQuantileNative) { h.PositiveOffsetColumn = "" }},
		{"missing NegativeBucketCounts", func(h *chplan.HistogramQuantileNative) { h.NegativeBucketCountsColumn = "" }},
		{"missing NegativeOffset", func(h *chplan.HistogramQuantileNative) { h.NegativeOffsetColumn = "" }},
		{"missing Scale", func(h *chplan.HistogramQuantileNative) { h.ScaleColumn = "" }},
		{"missing ZeroCount", func(h *chplan.HistogramQuantileNative) { h.ZeroCountColumn = "" }},
		// ZeroThresholdColumn is intentionally NOT in this list: an
		// empty value is the valid "schema persists no zero_threshold"
		// configuration (the upstream OTel-CH DDL has no such column)
		// and renders a constant-0 zero-bucket width — see
		// TestEmit_HistogramQuantileNative_NoZeroThresholdColumn.
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

// TestEmit_HistogramQuantileNative_ShapeSanity emits SQL for a
// well-formed IR node and asserts the key tokens that prove the
// Phase 4 algorithm landed in the right shape: pow(base, ...),
// arrayConcat(arrayReverse(NegativeBucketCounts), [ZeroCount],
// PositiveBucketCounts) cum-sum over the full walk,
// length(NegativeBucketCounts) boundary, negative/zero/positive
// interpolation branches, and edge cases (total=0, phi<=0, phi>=1).
func TestEmit_HistogramQuantileNative_ShapeSanity(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramQuantileNative{
		Input:                      &chplan.Scan{Table: "otel_metrics_exp_histogram"},
		Phi:                        0.95,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		ZeroThresholdColumn:        "ZeroThreshold",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:             []string{"Attributes"},
		MetricNameColumn:           "MetricName",
		AttributesColumn:           "Attributes",
		TimestampColumn:            "TimeUnix",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	wantTokens := []string{
		"pow(2, pow(2, -`Scale`))",
		"arrayConcat(arrayReverse(`NegativeBucketCounts`), [`ZeroCount`], `PositiveBucketCounts`)",
		"arrayFirstIndex(c -> c >= (0.95",
		"length(`NegativeBucketCounts`)",
		"`PositiveOffset` + length(`PositiveBucketCounts`)",
		"0.95 <= 0",
		"0.95 >= 1",
		"-`ZeroThreshold`",
		"2 * `ZeroThreshold`",
		"FROM `otel_metrics_exp_histogram`",
	}
	for _, tok := range wantTokens {
		if !strings.Contains(sql, tok) {
			t.Errorf("SQL missing expected token %q\n--- sql ---\n%s", tok, sql)
		}
	}

	// Native-path SQL must NOT mention classic-histogram columns
	// (BucketCounts, ExplicitBounds). PositiveBucketCounts /
	// NegativeBucketCounts substring-match BucketCounts; use the
	// quoted form ``BucketCounts`` to differentiate the bare
	// classic-table column name from its native-prefixed siblings.
	for _, banned := range []string{"`BucketCounts`", "`ExplicitBounds`"} {
		if strings.Contains(sql, banned) {
			t.Errorf("SQL contains classic-histogram token %q (should be native-only)\n--- sql ---\n%s", banned, sql)
		}
	}

	// Parenthesis balance — a quick guard against the easy class of bugs
	// in this emitter (nested if()s with edge cases). Counts the run.
	opens := strings.Count(sql, "(")
	closes := strings.Count(sql, ")")
	if opens != closes {
		t.Errorf("parenthesis imbalance: %d open, %d close", opens, closes)
	}
}
