package regression

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every compatibility head runs its differential against ONE reference
// backend image, pinned in that head's docker-compose.yml. The semantic
// execution observations stamp `reference_version` on every record from a
// REFERENCE_VERSION env in compatibility.yml — a third hand copy of the same
// tag (the mirror list in lib/mirror.mjs is the second, and that one is
// pinned against compose by mirror_inventory_test.go). A stale copy here does
// not fail anything: it stamps evidence with a version the run never
// exercised. This pins the workflow copy to the compose file.

// compatReferenceImages maps each head to the image repository its compose
// file pins as the reference backend.
var compatReferenceImages = map[string]string{
	"prometheus": "prom/prometheus",
	"tempo":      "grafana/tempo",
	"loki":       "grafana/loki",
}

const compatWorkflowPath = "../../.github/workflows/compatibility.yml"

// composeReferenceImage returns the `image:` a compose file pins for the
// given repository.
func composeReferenceImage(t *testing.T, path, repository string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*image:\s*(` + regexp.QuoteMeta(repository) + `:[^\s#]+)`)
	m := re.FindStringSubmatch(readFileString(t, path))
	if m == nil {
		t.Fatalf("%s pins no `image: %s:<tag>`", path, repository)
	}
	return m[1]
}

func TestCompatReferenceVersionMirrorsTheComposeImage(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFileString(t, compatWorkflowPath)), &workflow); err != nil {
		t.Fatalf("parse %s: %v", compatWorkflowPath, err)
	}

	for head, repository := range compatReferenceImages {
		compose := "../../compatibility/" + head + "/docker-compose.yml"
		want := composeReferenceImage(t, compose, repository)
		jobID := "semantic-observations-" + head
		job, ok := workflow.Jobs[jobID]
		if !ok {
			t.Errorf("%s has no %q job; the head's observations are not being generated", compatWorkflowPath, jobID)
			continue
		}
		found := false
		for _, step := range job.Steps {
			got, set := step.Env["REFERENCE_VERSION"]
			if !set {
				continue
			}
			found = true
			if strings.TrimSpace(got) != want {
				t.Errorf("%s job %q stamps REFERENCE_VERSION %q, but %s pins the reference as %q; the evidence "+
					"would name a version the run never exercised", compatWorkflowPath, jobID, got, compose, want)
			}
		}
		if !found {
			t.Errorf("%s job %q sets no REFERENCE_VERSION; every observation would carry a null reference_version",
				compatWorkflowPath, jobID)
		}
	}
}
