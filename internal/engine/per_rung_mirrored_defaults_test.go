package engine

import (
	"testing"

	"github.com/tsouza/cerberus/internal/routememo"
)

// TestPerRungDefaultsMirrorRouteMemo pins the three per-rung learner
// constants that document themselves as mirroring internal/routememo's own
// against those constants directly. The learner is a second, independent
// application of one calibration decision — how much evidence is enough, how
// long it stays trusted, how many keys stay resident — so without this the
// two sides are free to drift while both comments still claim they agree
// (cerberus issue #3186). It follows internal/downsampletier's
// TestResourceCapsMatchDeltaPrefix, the pattern already established here.
func TestPerRungDefaultsMirrorRouteMemo(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name      string
		got, want any
		mirrors   string
	}{
		{"perRungEvidenceMinObservations", perRungEvidenceMinObservations, routememo.MinCorroboratingFailures, "routememo.MinCorroboratingFailures"},
		{"perRungEvidenceTTL", perRungEvidenceTTL, routememo.MemoEntryTTL, "routememo.MemoEntryTTL"},
		{"perRungLearnerCapacity", perRungLearnerCapacity, routememo.MemoMaxEntries, "routememo.MemoMaxEntries"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v; %s is %v — the two carry one rationale and must stay identical",
				c.name, c.got, c.mirrors, c.want)
		}
	}
}
