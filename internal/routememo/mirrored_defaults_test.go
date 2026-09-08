package routememo

import (
	"testing"

	"github.com/tsouza/cerberus/internal/actuals"
)

// TestMirroredDefaultsMatchActuals pins the three constants this package
// documents as mirroring internal/actuals's own defaults against those
// defaults themselves. Each is a second, independent implementation of one
// calibration decision — routememo is a pure leaf and must not import
// internal/actuals outside a test (.go-arch-lint.yml) — so without this the
// two sides are free to drift while both comments still claim they agree.
// It follows internal/downsampletier's TestResourceCapsMatchDeltaPrefix,
// the pattern already established for exactly this shape (issue #3186).
func TestMirroredDefaultsMatchActuals(t *testing.T) {
	t.Parallel()

	def := actuals.DefaultConfig()
	for _, c := range []struct {
		name       string
		got, want  any
		mirrorDocs string
	}{
		{"magnitudeEMAAlpha", magnitudeEMAAlpha, def.EMAAlpha, "actuals.Config.EMAAlpha"},
		{"MemoEntryTTL", MemoEntryTTL, def.EntryTTL, "actuals.Config.EntryTTL"},
		{"MinCorroboratingFailures", MinCorroboratingFailures, def.MinObservations, "actuals.Config.MinObservations"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v; %s defaults to %v — the two carry one rationale and must stay identical",
				c.name, c.got, c.mirrorDocs, c.want)
		}
	}
}
