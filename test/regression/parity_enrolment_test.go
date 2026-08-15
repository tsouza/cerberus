package regression

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

const parityEnrolmentCheckpointDir = "parity-enrolment-floors"

// TestParityEnrolmentFloor is the ratchet.
//
// Enrolment is incremental — a fixture is enrolled by hand when its seed
// carries what the reference engine needs — so this cannot assert that
// every fixture is enrolled. What it CAN do is make the count monotonic:
// enrolment may only ever grow, and raising the floor requires adding a
// checkpoint file that is visible in review.
//
// Checkpoints are append-only instead of one shared constant. Concurrent
// parity PRs therefore add independent files rather than conflicting on a
// hot line. The effective floor is the largest valid checkpoint.
//
// That is deliberately weaker than the all-or-nothing enrolment a closed
// corpus would allow, and it is the honest trade: PromQL fixtures include
// metadata and exemplar shapes that ask no PromQL expression at all, so
// "every fixture must enrol" is unattainable, and forcing it would require
// a per-fixture exemption vocabulary — precisely the allow-list shape
// invariant 7 forbids.
func TestParityEnrolmentFloor(t *testing.T) {
	t.Parallel()

	parityEnrolmentFloor := loadParityEnrolmentFloor(t)
	enrolled := 0
	for _, dir := range parityFixtureDirs(t) {
		for _, path := range txtarFilesForParity(t, dir) {
			c, err := spec.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if _, ok, err := spec.LoadParity(c); err == nil && ok {
				enrolled++
			}
		}
	}

	if enrolled < parityEnrolmentFloor {
		t.Errorf(
			"parity enrolment fell to %d, below the committed floor of %d.\n"+
				"A fixture lost its `-- parity --` section, which silently returns it to being "+
				"checked only against cerberus's own regenerated output. If the un-enrolment is "+
				"intentional, remove or replace the highest checkpoint and say why in the commit message.",
			enrolled, parityEnrolmentFloor,
		)
	}
	if enrolled > parityEnrolmentFloor {
		t.Errorf(
			"parity enrolment rose to %d, above the committed floor of %d.\n"+
				"Add a uniquely named .txt checkpoint containing %d under test/regression/%s "+
				"so the gain cannot be silently lost. Do not edit an existing checkpoint: "+
				"append-only checkpoints let concurrent parity PRs merge without a hot-line conflict.",
			enrolled, parityEnrolmentFloor, enrolled, parityEnrolmentCheckpointDir,
		)
	}
}

func loadParityEnrolmentFloor(t *testing.T) int {
	t.Helper()
	dir := filepath.Join(repoRootForParity(t), "test", "regression", parityEnrolmentCheckpointDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read parity enrolment checkpoints: %v", err)
	}

	floor := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".txt" {
			t.Errorf("unexpected parity enrolment checkpoint entry %q; expected a .txt file", entry.Name())
			continue
		}
		contents, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Errorf("read parity enrolment checkpoint %s: %v", entry.Name(), err)
			continue
		}
		checkpoint, err := strconv.Atoi(strings.TrimSpace(string(contents)))
		if err != nil || checkpoint <= 0 {
			t.Errorf("parity enrolment checkpoint %s must contain one positive decimal integer", entry.Name())
			continue
		}
		if checkpoint > floor {
			floor = checkpoint
		}
	}
	if floor == 0 {
		t.Fatal("parity enrolment checkpoint directory contains no valid floor")
	}
	return floor
}
