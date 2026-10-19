package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// beyondTestEdge is how far a cut window's end lies before Prometheus's.
const beyondTestEdge = 103146 * time.Millisecond

// beyondTestWindow is an instant rate window cut at the end by
// beyondTestEdge.
func beyondTestWindow() *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input: &chplan.Scan{
			Table:   "otel_metrics_sum",
			Columns: []string{"Attributes", "TimeUnix", "Value"},
			Roles:   []chplan.Column{{Name: "TimeUnix", Role: chplan.RoleTimestamp}},
		},
		Func:            "rate",
		Range:           10*time.Minute - beyondTestEdge,
		End:             time.UnixMilli(9223372036854).UTC(),
		EndBeyond:       beyondTestEdge,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// TestRangeWindowBeyond_AdmitsOnlyTheExtrapolatingPath pins that a window
// whose edges lie beyond the window it reads is emitted only by the path
// that extrapolates to them, and refused by every other.
func TestRangeWindowBeyond_AdmitsOnlyTheExtrapolatingPath(t *testing.T) {
	for _, fn := range []string{"rate", "increase", "delta"} {
		r := beyondTestWindow()
		r.Func = fn
		if err := validateRangeWindowBeyond(r); err != nil {
			t.Fatalf("%s over a cut window: %v", fn, err)
		}
	}
	refused := map[string]func(*chplan.RangeWindow){
		"other function":        func(r *chplan.RangeWindow) { r.Func = "deriv" },
		"downsample tier":       func(r *chplan.RangeWindow) { r.DownsampleTier = true },
		"variants":              func(r *chplan.RangeWindow) { r.Variants = []chplan.RangeWindowVariant{{Func: "rate"}} },
		"identity":              func(r *chplan.RangeWindow) { r.Identity = true },
		"outer range":           func(r *chplan.RangeWindow) { r.OuterRange = time.Minute },
		"step":                  func(r *chplan.RangeWindow) { r.Step = time.Minute },
		"fixed accumulator":     func(r *chplan.RangeWindow) { r.FixedAccumulatorExtrapolated = true },
		"delta prefix":          func(r *chplan.RangeWindow) { r.DeltaPrefixAggregateInput = r.Input },
		"start beyond, deriv":   func(r *chplan.RangeWindow) { r.EndBeyond, r.StartBeyond, r.Func = 0, time.Second, "deriv" },
		"start beyond, stepped": func(r *chplan.RangeWindow) { r.EndBeyond, r.StartBeyond, r.Step = 0, time.Second, time.Minute },
	}
	for name, perturb := range refused {
		t.Run(name, func(t *testing.T) {
			r := beyondTestWindow()
			perturb(r)
			if err := validateRangeWindowBeyond(r); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("validateRangeWindowBeyond = %v, want ErrUnsupported", err)
			}
			if _, _, err := Emit(context.Background(), r); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Emit = %v, want ErrUnsupported", err)
			}
		})
	}
	uncut := beyondTestWindow()
	uncut.EndBeyond, uncut.Func = 0, "deriv"
	if err := validateRangeWindowBeyond(uncut); err != nil {
		t.Fatalf("an uncut window: %v", err)
	}
}

// TestRangeWindowBeyond_WidensTheExtrapolation pins the SQL: the distance
// to the end gains the cut-off part in Int64 nanoseconds, and rate divides
// by Prometheus's own window length.
func TestRangeWindowBeyond_WidensTheExtrapolation(t *testing.T) {
	sql, _, err := Emit(context.Background(), beyondTestWindow())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"last_ts, parseDateTime64BestEffort('2262-04-11 23:47:16.854000000Z', 9)) + 103146000000",
		"/ sampled_interval / 600)",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "first_ts)) + ") {
		t.Fatalf("the distance to an uncut start is widened:\n%s", sql)
	}
}
