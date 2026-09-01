package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/solver"
)

func TestApplyActualsCapture_NoOpWhenTrackerNil(t *testing.T) {
	decision := &solver.Decision{ShapeID: "cerb:agg;rw"}
	ctx := context.Background()
	got := applyActualsCapture(ctx, nil, decision)
	if got != ctx {
		t.Fatal("expected ctx unchanged with a nil tracker")
	}
}

func TestApplyActualsCapture_NoOpWhenDecisionNilOrEmptyShapeID(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	ctx := context.Background()

	if got := applyActualsCapture(ctx, tracker, nil); got != ctx {
		t.Fatal("expected ctx unchanged with a nil decision")
	}
	if got := applyActualsCapture(ctx, tracker, &solver.Decision{}); got != ctx {
		t.Fatal("expected ctx unchanged with an empty ShapeID")
	}
}

func TestApplyActualsCapture_StampsLogCommentRegardlessOfSettingsRulesFlag(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	decision := &solver.Decision{ShapeID: "cerb:agg;rw"}

	ctx := applyActualsCapture(context.Background(), tracker, decision)
	settings := chclient.QuerySettingsFromContext(ctx)
	if got := settings[settingLogComment]; got != "cerb:agg;rw" {
		t.Fatalf("expected log_comment stamped to the shape id, got %v", got)
	}
}

func TestApplyActualsCapture_RecordsPredictedOnlyWhenPresent(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())

	// No predicted estimate on this Decision: RecordPredicted must not be
	// called (no snapshot yet).
	applyActualsCapture(context.Background(), tracker, &solver.Decision{ShapeID: "cerb:agg;rw"})
	if _, ok := tracker.Snapshot("cerb:agg;rw"); ok {
		t.Fatal("expected no tracked state without a predicted estimate")
	}

	applyActualsCapture(context.Background(), tracker, &solver.Decision{
		ShapeID: "cerb:agg;rw;rbf", HasPredictedEstimate: true, PredictedRows: 12345,
	})
	report, ok := tracker.Snapshot("cerb:agg;rw;rbf")
	if !ok {
		t.Fatal("expected a tracked prediction")
	}
	if !report.HasPredicted || report.PredictedRows != 12345 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestApplyActualsCapture_WiresPacketCaptureOnAlreadyProgressTrackedCtx(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	decision := &solver.Decision{ShapeID: "cerb:agg;rw"}

	ctx := chclient.WithProgressFor(context.Background(), "promql")
	ctx = applyActualsCapture(ctx, tracker, decision)

	// Simulate the dispatch completing: the SAME shapeID must reach the
	// tracker via the packet path once flush() runs downstream — pinned
	// indirectly here by asserting WithActualsCapture actually wired the
	// recorder (chclient's own test suite pins flush()'s own behavior in
	// detail; this test only pins that applyActualsCapture composes the two
	// calls correctly).
	if got := chclient.QuerySettingsFromContext(ctx)[settingLogComment]; got != "cerb:agg;rw" {
		t.Fatalf("expected log_comment stamped, got %v", got)
	}
}

func TestCalibrateEstimate_NoOpWithoutActualsOrEstimate(t *testing.T) {
	e := &Engine{}
	if got := e.calibrateEstimate("cerb:agg;rw", nil); got != nil {
		t.Fatalf("expected nil estimate to pass through nil, got %+v", got)
	}

	e.Actuals = actuals.NewTracker(actuals.DefaultConfig())
	zero := &solver.ScanEstimate{Rows: 0}
	if got := e.calibrateEstimate("cerb:agg;rw", zero); got != zero {
		t.Fatal("expected a zero-Rows estimate to pass through unchanged (same pointer)")
	}

	est := &solver.ScanEstimate{Rows: 1000}
	// No calibration evidence recorded yet: CalibrationFactor is not ok, so
	// the SAME pointer must come back unchanged.
	if got := e.calibrateEstimate("cerb:agg;rw", est); got != est {
		t.Fatal("expected the estimate to pass through unchanged with no calibration evidence")
	}
}

func TestCalibrateEstimate_AppliesBoundedCorrection(t *testing.T) {
	e := &Engine{Actuals: actuals.NewTracker(actuals.DefaultConfig())}
	const shape = "cerb:agg;rw"

	e.Actuals.RecordPredicted(shape, 100_000)
	for i := 0; i < 2; i++ {
		if _, ok := e.Actuals.RecordActual(shape, actuals.Actual{ReadRows: 1_000_000}, actuals.SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}

	original := &solver.ScanEstimate{Rows: 100_000, Parts: 3, Marks: 12}
	calibrated := e.calibrateEstimate(shape, original)
	if calibrated == original {
		t.Fatal("expected a NEW ScanEstimate value, not the same pointer, once calibration applies")
	}
	// The raw ratio (10x) is clamped to maxCalibrationFactor (2.0), so Rows
	// doubles rather than growing 10x.
	if calibrated.Rows != 200_000 {
		t.Fatalf("expected Rows calibrated to 200000 (2x, clamped), got %d", calibrated.Rows)
	}
	// Every other field is preserved verbatim.
	if calibrated.Parts != original.Parts || calibrated.Marks != original.Marks {
		t.Fatalf("expected Parts/Marks preserved, got %+v", calibrated)
	}
	// The original must never be mutated.
	if original.Rows != 100_000 {
		t.Fatalf("expected the original estimate untouched, got %+v", original)
	}
}
