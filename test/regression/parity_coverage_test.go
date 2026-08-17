package regression

import (
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// TestPromQLParityCoverageIsComplete (#2187) closes the PromQL corpus: every fixture
// carries either a real parity enrolment or a reviewed, closed-vocabulary
// exemption, never neither and never both.
func TestPromQLParityCoverageIsComplete(t *testing.T) {
	t.Parallel()

	root := repoRootForParity(t)
	dir := filepath.Join(root, "test", "spec", "promql")

	for _, path := range txtarFilesForParity(t, dir) {
		name := filepath.Base(path)
		c, err := spec.Load(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}

		_, enrolled, err := spec.LoadParity(c)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_, exempted, err := spec.LoadParityExempt(c)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}

		switch {
		case enrolled && exempted:
			t.Errorf(
				"%s carries BOTH `parity:` and `parity_exempt:`. A fixture is either enrolled "+
					"against a real reference engine or it declares why it cannot be — the two "+
					"claims contradict each other for the same fixture.",
				name,
			)
		case !enrolled && !exempted:
			t.Errorf(
				"%s carries neither `parity:` nor `parity_exempt:`. That is silently "+
					"indistinguishable from a fixture nobody has looked at — enrol it against a "+
					"reference engine (see parity.go) or declare, with a closed-vocabulary reason, "+
					"why it structurally cannot be (see parity_exempt.go).",
				name,
			)
		}
	}
}
