package actuals

import (
	"testing"
	"time"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Enabled = true
	return cfg
}

func TestTracker_NilSafe(t *testing.T) {
	var tr *Tracker
	tr.RecordPredicted("cerb:agg;rw", 1000)
	if _, ok := tr.RecordActual("cerb:agg;rw", Actual{ReadRows: 1000}, SourcePacket); ok {
		t.Fatal("RecordActual on nil Tracker must report ok=false")
	}
	if _, ok := tr.Snapshot("cerb:agg;rw"); ok {
		t.Fatal("Snapshot on nil Tracker must report ok=false")
	}
	if factor, ok := tr.CalibrationFactor("cerb:agg;rw"); ok || factor != 0 {
		t.Fatalf("CalibrationFactor on nil Tracker must report ok=false, got (%v, %v)", factor, ok)
	}
	if s := tr.Stats(); s != (Stats{}) {
		t.Fatalf("Stats on nil Tracker must be zero, got %+v", s)
	}
}

func TestTracker_EmptyShapeIDNoOp(t *testing.T) {
	tr := NewTracker(testConfig())
	tr.RecordPredicted("", 1000)
	if _, ok := tr.RecordActual("", Actual{ReadRows: 1000}, SourcePacket); ok {
		t.Fatal("RecordActual with empty shapeID must be a no-op")
	}
	if s := tr.Stats(); s.Entries != 0 {
		t.Fatalf("expected no entries recorded for empty shapeID, got %+v", s)
	}
}

// TestTracker_NoDriftWithinBand exercises the common case: EXPLAIN
// ESTIMATE's granule-resolution upper bound overestimates a real,
// index-pruned scan (the expected direction), and the ratio stays within
// the configured band — never alerting.
func TestTracker_NoDriftWithinBand(t *testing.T) {
	tr := NewTracker(testConfig())
	const shape = "cerb:agg;rw"
	tr.RecordPredicted(shape, 100_000)
	// Two corroborating observations, both close to the prediction.
	for i := 0; i < 2; i++ {
		report, ok := tr.RecordActual(shape, Actual{ReadRows: 90_000}, SourcePacket)
		if !ok {
			t.Fatal("RecordActual should succeed")
		}
		if i == 1 && report.Alerting {
			t.Fatalf("expected no drift alert for a 0.9 ratio, got %+v", report)
		}
	}
}

// TestTracker_AlertsOnUnderPrediction is the DANGEROUS direction the issue
// exists to catch: real read_rows repeatedly exceeds EXPLAIN ESTIMATE's own
// upper bound by more than DriftUpperRatio, which should never happen if
// the estimate were a true bound — this is exactly the silent
// mis-admission case the drift alert surfaces.
func TestTracker_AlertsOnUnderPrediction(t *testing.T) {
	tr := NewTracker(testConfig())
	const shape = "cerb:agg;rw;rbf"
	tr.RecordPredicted(shape, 10_000)

	// First observation: not enough evidence yet even though the ratio is
	// already outside the band (MinObservations=2 by default).
	report, ok := tr.RecordActual(shape, Actual{ReadRows: 100_000}, SourcePacket)
	if !ok {
		t.Fatal("RecordActual should succeed")
	}
	if report.Alerting {
		t.Fatalf("must not alert before MinObservations is reached, got %+v", report)
	}

	// Second corroborating observation: now it alerts.
	report, ok = tr.RecordActual(shape, Actual{ReadRows: 100_000}, SourcePacket)
	if !ok {
		t.Fatal("RecordActual should succeed")
	}
	if !report.Alerting {
		t.Fatalf("expected a drift alert once corroborated, got %+v", report)
	}
	if report.Ratio <= testConfig().DriftUpperRatio {
		t.Fatalf("expected ratio above the upper band, got %v", report.Ratio)
	}
}

// TestTracker_ZeroPredictionNeverAlerts pins driftLocked's own documented
// exception: a zero prediction (the index analysis pruned the scan to
// nothing) never alerts, regardless of the actual — the ratio is undefined,
// not "infinite drift".
func TestTracker_ZeroPredictionNeverAlerts(t *testing.T) {
	tr := NewTracker(testConfig())
	const shape = "cerb:filter"
	tr.RecordPredicted(shape, 0)
	for i := 0; i < 5; i++ {
		report, ok := tr.RecordActual(shape, Actual{ReadRows: 1_000_000}, SourcePacket)
		if !ok {
			t.Fatal("RecordActual should succeed")
		}
		if report.Alerting {
			t.Fatalf("a zero prediction must never alert, got %+v", report)
		}
		if report.Ratio != 0 {
			t.Fatalf("a zero prediction must report ratio 0, got %v", report.Ratio)
		}
	}
}

// TestTracker_EMABoundedInfluence pins the anti-autotune bounded-influence
// property: a single wildly anomalous observation cannot swing the tracked
// EMA past EMAAlpha's own fraction of the gap.
func TestTracker_EMABoundedInfluence(t *testing.T) {
	tr := NewTracker(testConfig())
	const shape = "cerb:agg;rw"
	tr.RecordPredicted(shape, 100_000)
	if _, ok := tr.RecordActual(shape, Actual{ReadRows: 100_000}, SourcePacket); !ok {
		t.Fatal("RecordActual should succeed")
	}
	// A wild single anomaly: 100x the established baseline.
	report, ok := tr.RecordActual(shape, Actual{ReadRows: 10_000_000}, SourcePacket)
	if !ok {
		t.Fatal("RecordActual should succeed")
	}
	// EMA after one 0.2-alpha step from 100,000 toward 10,000,000:
	// 100,000 + 0.2*(10,000,000-100,000) = 2,080,000.
	const wantEMA = 100_000 + defaultEMAAlpha*(10_000_000-100_000)
	if report.ActualEMARows != wantEMA {
		t.Fatalf("expected bounded EMA step to %v, got %v", wantEMA, report.ActualEMARows)
	}
	if report.ActualEMARows >= 10_000_000 {
		t.Fatalf("a single anomalous observation must not fully move the EMA, got %v", report.ActualEMARows)
	}
}

func TestTracker_CalibrationFactor_BoundedAndGated(t *testing.T) {
	tr := NewTracker(testConfig())
	const shape = "cerb:agg;rw"

	// No prediction yet: not ok.
	if _, ok := tr.CalibrationFactor(shape); ok {
		t.Fatal("CalibrationFactor must be ok=false with no prediction recorded")
	}

	tr.RecordPredicted(shape, 100_000)
	// Prediction only, no actual yet: still not ok.
	if _, ok := tr.CalibrationFactor(shape); ok {
		t.Fatal("CalibrationFactor must be ok=false with no actual recorded")
	}

	// One observation: below MinObservations (2), still not ok.
	if _, ok := tr.RecordActual(shape, Actual{ReadRows: 1_000_000}, SourcePacket); !ok {
		t.Fatal("RecordActual should succeed")
	}
	if _, ok := tr.CalibrationFactor(shape); ok {
		t.Fatal("CalibrationFactor must be ok=false below MinObservations")
	}

	// Second observation reaches MinObservations: now ok, and the factor is
	// clamped to maxCalibrationFactor even though the raw ratio (~9.5x) is
	// far larger.
	if _, ok := tr.RecordActual(shape, Actual{ReadRows: 1_000_000}, SourcePacket); !ok {
		t.Fatal("RecordActual should succeed")
	}
	factor, ok := tr.CalibrationFactor(shape)
	if !ok {
		t.Fatal("expected CalibrationFactor to be ok once corroborated")
	}
	if factor != maxCalibrationFactor {
		t.Fatalf("expected the calibration factor clamped to %v, got %v", maxCalibrationFactor, factor)
	}

	// Symmetric clamp: a shape whose actual is far BELOW its prediction
	// clamps to minCalibrationFactor.
	const underShape = "cerb:agg;rw;lwr"
	tr.RecordPredicted(underShape, 1_000_000)
	for i := 0; i < 2; i++ {
		if _, ok := tr.RecordActual(underShape, Actual{ReadRows: 100}, SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}
	factor, ok = tr.CalibrationFactor(underShape)
	if !ok {
		t.Fatal("expected CalibrationFactor to be ok once corroborated")
	}
	if factor != minCalibrationFactor {
		t.Fatalf("expected the calibration factor clamped to %v, got %v", minCalibrationFactor, factor)
	}
}

func TestTracker_EntryTTLExpiry(t *testing.T) {
	tr := NewTracker(testConfig())
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tr.SetNowForTest(func() time.Time { return now })

	const shape = "cerb:agg;rw"
	tr.RecordPredicted(shape, 100_000)
	for i := 0; i < 2; i++ {
		if _, ok := tr.RecordActual(shape, Actual{ReadRows: 90_000}, SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}
	if _, ok := tr.Snapshot(shape); !ok {
		t.Fatal("expected a live entry before the TTL elapses")
	}

	now = now.Add(testConfig().EntryTTL + time.Minute)
	if _, ok := tr.Snapshot(shape); ok {
		t.Fatal("expected the entry to have expired past EntryTTL")
	}
	if _, ok := tr.CalibrationFactor(shape); ok {
		t.Fatal("expected CalibrationFactor to refuse an expired entry")
	}
}

func TestTracker_StatsCountsAlerting(t *testing.T) {
	tr := NewTracker(testConfig())

	tr.RecordPredicted("cerb:agg;rw", 100_000)
	for i := 0; i < 2; i++ {
		if _, ok := tr.RecordActual("cerb:agg;rw", Actual{ReadRows: 90_000}, SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}
	tr.RecordPredicted("cerb:agg;rw;rbf", 10_000)
	for i := 0; i < 2; i++ {
		if _, ok := tr.RecordActual("cerb:agg;rw;rbf", Actual{ReadRows: 100_000}, SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}

	stats := tr.Stats()
	if stats.Entries != 2 {
		t.Fatalf("expected 2 entries, got %d", stats.Entries)
	}
	if stats.Alerting != 1 {
		t.Fatalf("expected exactly 1 alerting shape, got %d", stats.Alerting)
	}
}

func TestTracker_CapacityEviction(t *testing.T) {
	tr := NewTracker(testConfig())
	for i := 0; i < trackerCapacity+10; i++ {
		tr.RecordPredicted(shapeIDForTest(i), 1000)
	}
	stats := tr.Stats()
	if stats.Entries > trackerCapacity {
		t.Fatalf("expected the resident size to stay bounded at %d, got %d", trackerCapacity, stats.Entries)
	}
}

func shapeIDForTest(i int) string {
	// A closed, deterministic id vocabulary — mirrors the real plan-shape id
	// format ("cerb:<root>;<modifier>...") without pulling in chplan/engine.
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := []byte("cerb:test;")
	for i > 0 {
		b = append(b, alphabet[i%len(alphabet)])
		i /= len(alphabet)
	}
	return string(b)
}
