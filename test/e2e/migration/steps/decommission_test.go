package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// TestStagedOrderMatchesTheCommittedRunbook runs MIG-25's staged-teardown-order
// assertion against the REAL runbook, offline. The scenario itself only reaches
// this comparison on the tier-1 lane, behind a live compose stack; running it
// here means a diff that reorders the harness's stages, or reworks the
// runbook's sentence, fails in `just test` rather than an hour later.
func TestStagedOrderMatchesTheCommittedRunbook(t *testing.T) {
	t.Parallel()

	if err := stagedOrderMatchesRunbook(committedRunbook(t)); err != nil {
		t.Fatal(err)
	}
}

// TestStagedOrderRefusesARunbookStatingAnotherOrder proves the comparison has
// discriminating power in the direction that matters: a runbook that documents
// the ingest leg being torn down before the read path must fail, not be read as
// agreement. Without this, the assertion could be satisfied by any document
// that merely mentions the three stages.
func TestStagedOrderRefusesARunbookStatingAnotherOrder(t *testing.T) {
	t.Parallel()

	doc := committedRunbook(t)
	row, err := runbookStageRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	reversed := strings.NewReplacer(
		decommissionStagePhrases["read-path"], decommissionStagePhrases["ingest-write"],
		decommissionStagePhrases["ingest-write"], decommissionStagePhrases["read-path"],
	).Replace(row)
	if err := stagedOrderMatchesRunbook(strings.Replace(doc, row, reversed, 1)); err == nil {
		t.Fatal("a runbook stating the ingest leg is torn down first passed the staged-order comparison")
	}
}

// TestStagedOrderRefusesARunbookThatStatesNoOrder proves the same for a runbook
// that stopped documenting the order at all: with nothing to hold the harness's
// stage list to, the assertion fails rather than passing on its own literals —
// which is exactly what it did before it read the runbook.
func TestStagedOrderRefusesARunbookThatStatesNoOrder(t *testing.T) {
	t.Parallel()

	doc := committedRunbook(t)
	row, err := runbookStageRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := stagedOrderMatchesRunbook(strings.Replace(doc, row, "", 1)); err == nil {
		t.Fatal("a runbook that documents no staged teardown order passed the comparison")
	}
}

// committedRunbook reads the runbook the assertion is anchored to.
func committedRunbook(t *testing.T) string {
	t.Helper()

	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, decommissionRunbook))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
