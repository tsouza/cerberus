package optimizer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// instantRateLeaf builds the pre-#1098 plan shape: an instant
// (OuterRange == 0) rate() RangeWindow reading a raw Scan with NO scan-time
// bound established (InstantScanBounded == false). This is exactly the shape
// whose unbounded innermost groupArray read the recurring bug class
// (#1027 … #1098) produced.
func instantRateLeaf() *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func:            "rate",
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// recoverScanTimeBoundViolation runs fn and returns the
// *ScanTimeBoundViolation it panics with, or nil if it did not panic.
func recoverScanTimeBoundViolation(t *testing.T, fn func()) (v *optimizer.ScanTimeBoundViolation) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		got, ok := r.(*optimizer.ScanTimeBoundViolation)
		if !ok {
			t.Fatalf("expected *ScanTimeBoundViolation, got %T: %v", r, r)
		}
		v = got
	}()
	fn()
	return nil
}

// TestRequireScanTimeBound_RejectsUnboundedInstantLeaf is the fail-closed
// proof: the synthetic pre-#1098 shape (instant windowed-array leaf, no bound)
// is REJECTED by RequireScanTimeBound. This is also the "would have caught the
// original instant bug" demonstration — the plan shape #1098 fixed by adding
// the emit-time bound is precisely the one this analyzer now refuses to ship.
func TestRequireScanTimeBound_RejectsUnboundedInstantLeaf(t *testing.T) {
	t.Parallel()

	leaf := instantRateLeaf()
	if leaf.InstantScanBounded {
		t.Fatal("fixture must start unbounded")
	}
	if !chplan.IsInstantWindowedLeaf(leaf) {
		t.Fatal("fixture must be an instant windowed-array leaf")
	}

	// Require alone (no Normalize) over the unbounded leaf must panic.
	d := optimizer.NewWithBatches(
		optimizer.AnalyzerBatch("test.require", optimizer.RequireScanTimeBound{}),
	)
	v := recoverScanTimeBoundViolation(t, func() {
		d.Run(context.Background(), leaf)
	})
	if v == nil {
		t.Fatal("RequireScanTimeBound must reject an unbounded instant windowed-array leaf")
		return
	}
	if v.Func != "rate" {
		t.Errorf("violation Func = %q, want %q", v.Func, "rate")
	}
	if v.TimestampColumn != "TimeUnix" {
		t.Errorf("violation TimestampColumn = %q, want %q", v.TimestampColumn, "TimeUnix")
	}
	if msg := v.Error(); !strings.Contains(msg, "ScanTimeBound") || !strings.Contains(msg, "rate") {
		t.Errorf("violation message should name the contract and func, got %q", msg)
	}
}

// TestScanTimeBound_NormalizeThenRequireAccepts proves the establish→verify
// pairing wired into Default(): NormalizeScanTimeBound marks the same pre-#1098
// shape, and RequireScanTimeBound then accepts it without panicking. This is
// the post-#1098 / post-fix half of the invariant.
func TestScanTimeBound_NormalizeThenRequireAccepts(t *testing.T) {
	t.Parallel()

	d := optimizer.NewWithBatches(
		optimizer.AnalyzerBatch(
			"analyzer.scan-time-bound",
			optimizer.NormalizeScanTimeBound{},
			optimizer.RequireScanTimeBound{},
		),
	)

	out := d.Run(context.Background(), instantRateLeaf())
	rw, ok := out.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected *chplan.RangeWindow, got %T", out)
	}
	if !rw.InstantScanBounded {
		t.Fatal("NormalizeScanTimeBound must mark the instant windowed-array leaf bounded")
	}
}

// TestDefault_AcceptsBoundedInstantLeaf checks the full production pipeline
// (optimizer.Default(), which contains the scan-time-bound batch) accepts the
// shape end-to-end without panicking.
func TestDefault_AcceptsBoundedInstantLeaf(t *testing.T) {
	t.Parallel()

	v := recoverScanTimeBoundViolation(t, func() {
		optimizer.Default().Run(context.Background(), instantRateLeaf())
	})
	if v != nil {
		t.Fatalf("Default() must accept an instant leaf (Normalize establishes the bound): %v", v)
	}
}

// TestRequireScanTimeBound_IgnoresNonInstantLeaves proves scope honesty: the
// invariant fires ONLY for instant windowed-array leaves. The matrix
// (OuterRange > 0) and the three metrics-emitter Input node kinds
// (chplan.IsInstantWindowedLeaf's own type switch — MetricsAggregate,
// MetricsHistogramOverTime, MetricsCompare) carry their own emit-time bound
// (maybePushInnerScanTimeBounds) and are NOT instant windowed-array leaves, so
// RequireScanTimeBound must accept them even with the flag unset — it must
// never demand an IR flag for a path whose bound it does not govern. Every
// case here mirrors one arm of that type switch; dropping any one of the three
// Input kinds from IsInstantWindowedLeaf would make its own case's
// self-check (`chplan.IsInstantWindowedLeaf(rw)`) fail loudly rather than let
// RequireScanTimeBound silently start rejecting the shape.
func TestRequireScanTimeBound_IgnoresNonInstantLeaves(t *testing.T) {
	t.Parallel()

	cases := map[string]chplan.Node{
		// Matrix shape (OuterRange > 0): bounded at emit by the matrix
		// emitters via maybePushInnerScanTimeBounds.
		"matrix": &chplan.RangeWindow{
			Func:            "rate",
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Range:           5 * time.Minute,
			OuterRange:      time.Hour,
			Step:            time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		// MetricsAggregate input: routes to the metrics emitters, which
		// carry their own emit-time bound; excluded from the leaf set.
		"metrics-aggregate": &chplan.RangeWindow{
			Func:            "rate",
			Input:           &chplan.MetricsAggregate{},
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		// MetricsHistogramOverTime input: the TraceQL histogram-over-time
		// matrix path, same emit-time-bounded family as MetricsAggregate —
		// a distinct node kind in IsInstantWindowedLeaf's type switch.
		"metrics-histogram-over-time": &chplan.RangeWindow{
			Func:            "rate",
			Input:           &chplan.MetricsHistogramOverTime{},
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		// MetricsCompare input: the TraceQL compare() matrix path, the third
		// and last excluded node kind in the type switch.
		"metrics-compare": &chplan.RangeWindow{
			Func:            "rate",
			Input:           &chplan.MetricsCompare{},
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		// A bare Scan is not a RangeWindow at all.
		"bare-scan": &chplan.Scan{Table: "otel_metrics_sum"},
	}

	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if rw, ok := plan.(*chplan.RangeWindow); ok && chplan.IsInstantWindowedLeaf(rw) {
				t.Fatalf("%s must not be classified as an instant windowed-array leaf", name)
			}
			d := optimizer.NewWithBatches(
				optimizer.AnalyzerBatch("test.require", optimizer.RequireScanTimeBound{}),
			)
			v := recoverScanTimeBoundViolation(t, func() {
				d.Run(context.Background(), plan)
			})
			if v != nil {
				t.Fatalf("RequireScanTimeBound must not reject a non-leaf shape %q: %v", name, v)
			}
		})
	}
}
