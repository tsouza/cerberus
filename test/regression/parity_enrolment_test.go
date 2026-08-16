package regression

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

const (
	parityEnrolmentBaselineDir  = "parity-enrolment-baselines"
	parityEnrolmentBaselineFile = "baseline.txt"
	parityEnrolmentUpdateEnv    = "UPDATE_PARITY_ENROLMENT_BASELINE"
	parityEnrolmentFilePerm     = 0o644
)

var parityEnrolmentHeads = []string{"promql", "logql", "traceql"}

// TestParityEnrolmentBaseline pins the exact identity of every TXTAR fixture
// carrying a live-reference parity contract. Counts are insufficient here: a
// removed oracle case and an unrelated new case in the same head would preserve
// the count while silently changing the semantic surface.
func TestParityEnrolmentBaseline(t *testing.T) {
	enrolled := parityEnrolmentRosters(t)
	if os.Getenv(parityEnrolmentUpdateEnv) == "1" {
		writeParityEnrolmentBaselines(t, enrolled)
		return
	}

	baselines := loadParityEnrolmentBaselines(t)
	for _, problem := range parityEnrolmentProblems(enrolled, baselines) {
		t.Error(problem)
	}
}

func parityEnrolmentRosters(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRootForParity(t)
	rosters := make(map[string][]string, len(parityEnrolmentHeads))
	for _, head := range parityEnrolmentHeads {
		dir := filepath.Join(root, "test", "spec", head)
		for _, fixturePath := range txtarFilesForParity(t, dir) {
			fixture, err := spec.Load(fixturePath)
			if err != nil {
				t.Fatalf("load %s: %v", fixturePath, err)
			}
			_, enrolled, err := spec.LoadParity(fixture)
			if err != nil {
				t.Fatalf("load parity contract from %s: %v", fixturePath, err)
			}
			if !enrolled {
				continue
			}
			identity, err := filepath.Rel(dir, fixturePath)
			if err != nil {
				t.Fatalf("resolve parity fixture identity for %s: %v", fixturePath, err)
			}
			rosters[head] = append(rosters[head], filepath.ToSlash(identity))
		}
		slices.Sort(rosters[head])
	}
	return rosters
}

func parityEnrolmentProblems(enrolled, baselines map[string][]string) []string {
	var problems []string
	for _, head := range parityEnrolmentHeads {
		got, want := enrolled[head], baselines[head]
		if slices.Equal(got, want) {
			continue
		}

		missing := rosterDifference(want, got)
		unrecorded := rosterDifference(got, want)
		problems = append(problems, fmt.Sprintf(
			"%s parity enrolment identity baseline drifted.\n"+
				"Missing enrolled fixtures: %s\n"+
				"Unrecorded enrolled fixtures: %s\n"+
				"Restore the lost live-reference contracts, or run `just update-golden parity` "+
				"and review the exact identity diff.",
			head, formatRosterDifference(missing), formatRosterDifference(unrecorded),
		))
	}
	return problems
}

func rosterDifference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, identity := range right {
		rightSet[identity] = struct{}{}
	}
	var difference []string
	for _, identity := range left {
		if _, ok := rightSet[identity]; !ok {
			difference = append(difference, identity)
		}
	}
	return difference
}

func formatRosterDifference(identities []string) string {
	if len(identities) == 0 {
		return "none"
	}
	return "\n  - " + strings.Join(identities, "\n  - ")
}

// TestParityEnrolmentRejectsWithinHeadSubstitution pins the failure mode an
// integer floor cannot see: replacing one enrolled fixture with another in the
// same query head must remain a visible contract change.
func TestParityEnrolmentRejectsWithinHeadSubstitution(t *testing.T) {
	t.Parallel()

	baselines := loadParityEnrolmentBaselines(t)
	enrolled := cloneParityEnrolmentRosters(baselines)
	if len(enrolled["promql"]) == 0 {
		t.Fatal("PromQL parity baseline is empty; substitution control would be vacuous")
	}
	enrolled["promql"] = append(slices.Clone(enrolled["promql"][1:]), "replacement.txtar")
	slices.Sort(enrolled["promql"])

	problems := strings.Join(parityEnrolmentProblems(enrolled, baselines), "\n")
	if !strings.Contains(problems, "Missing enrolled fixtures") {
		t.Fatalf("within-head removal was accepted; problems: %s", problems)
	}
	if !strings.Contains(problems, "replacement.txtar") {
		t.Fatalf("within-head replacement was accepted; problems: %s", problems)
	}
}

func cloneParityEnrolmentRosters(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for head, roster := range in {
		out[head] = slices.Clone(roster)
	}
	return out
}

func loadParityEnrolmentBaselines(t *testing.T) map[string][]string {
	t.Helper()
	root := filepath.Join(repoRootForParity(t), "test", "regression", parityEnrolmentBaselineDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read parity enrolment baseline root: %v", err)
	}
	if len(entries) != len(parityEnrolmentHeads) {
		t.Fatalf("parity enrolment baseline root must contain exactly the head directories %v", parityEnrolmentHeads)
	}
	baselines := make(map[string][]string, len(parityEnrolmentHeads))
	for _, head := range parityEnrolmentHeads {
		baselines[head] = loadParityEnrolmentBaseline(t, filepath.Join(root, head))
	}
	return baselines
}

func loadParityEnrolmentBaseline(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read parity enrolment baseline: %v", err)
	}
	if len(entries) != 1 || entries[0].IsDir() || entries[0].Name() != parityEnrolmentBaselineFile {
		t.Fatalf("parity enrolment baseline directory %s must contain exactly %s", dir, parityEnrolmentBaselineFile)
	}
	contents, err := os.ReadFile(filepath.Join(dir, parityEnrolmentBaselineFile))
	if err != nil {
		t.Fatalf("read parity enrolment baseline: %v", err)
	}
	roster := strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	if err := validateParityEnrolmentRoster(roster); err != nil {
		t.Fatalf("invalid parity enrolment baseline %s: %v", dir, err)
	}
	if string(contents) != renderParityEnrolmentRoster(roster) {
		t.Fatalf("parity enrolment baseline %s is not in canonical form; run `just update-golden parity`", dir)
	}
	return roster
}

func validateParityEnrolmentRoster(roster []string) error {
	if len(roster) == 0 || (len(roster) == 1 && roster[0] == "") {
		return fmt.Errorf("roster must contain at least one fixture identity")
	}
	for _, identity := range roster {
		if identity == "" {
			return fmt.Errorf("roster contains a blank fixture identity")
		}
		if filepath.IsAbs(identity) || filepath.ToSlash(filepath.Clean(identity)) != identity || strings.HasPrefix(identity, "../") {
			return fmt.Errorf("fixture identity %q is not a canonical head-relative path", identity)
		}
		if filepath.Ext(identity) != ".txtar" {
			return fmt.Errorf("fixture identity %q does not name a TXTAR fixture", identity)
		}
	}
	if !slices.IsSorted(roster) {
		return fmt.Errorf("fixture identities are not sorted")
	}
	for index := 1; index < len(roster); index++ {
		if roster[index] == roster[index-1] {
			return fmt.Errorf("fixture identity %q appears more than once", roster[index])
		}
	}
	return nil
}

func renderParityEnrolmentRoster(roster []string) string {
	return strings.Join(roster, "\n") + "\n"
}

func writeParityEnrolmentBaselines(t *testing.T, rosters map[string][]string) {
	t.Helper()
	root := filepath.Join(repoRootForParity(t), "test", "regression", parityEnrolmentBaselineDir)
	for _, head := range parityEnrolmentHeads {
		path := filepath.Join(root, head, parityEnrolmentBaselineFile)
		if err := os.WriteFile(path, []byte(renderParityEnrolmentRoster(rosters[head])), parityEnrolmentFilePerm); err != nil {
			t.Fatalf("write parity enrolment baseline %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}
}
