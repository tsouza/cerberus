package chopt

import (
	"os"
	"slices"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

const replicatedE2EValuesPath = "../../test/e2e/k3s/cerberus-values-bwc-replicated.yaml"

// TestReplicatedE2EEnablesEveryOptimization keeps the fresh Helm deployment
// lane exhaustive. A newly registered optimization must be exercised there;
// otherwise opt-in startup/schema/readiness bugs can ship while every default
// (`auto`) deployment test remains green.
func TestReplicatedE2EEnablesEveryOptimization(t *testing.T) {
	raw, err := os.ReadFile(replicatedE2EValuesPath)
	if err != nil {
		t.Fatalf("read replicated E2E values: %v", err)
	}
	var values struct {
		CHOptimizations string `yaml:"chOptimizations"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse replicated E2E values: %v", err)
	}

	got := strings.Split(strings.ReplaceAll(values.CHOptimizations, "\n", ""), ",")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	want := make([]string, 0, len(Registry()))
	for _, feature := range Registry() {
		want = append(want, feature.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("replicated E2E optimizations = %v, want every registry feature %v", got, want)
	}
}
