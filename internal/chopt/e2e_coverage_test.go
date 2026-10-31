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
//
// The lane lists features explicitly under the default enforcing mode, which
// refuses to boot on a feature whose floor its bundled ClickHouse does not
// meet. So the list must hold exactly the registry features that server can
// run — every one at or below its version, none above it. A feature above the
// lane's server is validated against its own builds in test/chserver.
func TestReplicatedE2EEnablesEveryOptimization(t *testing.T) {
	raw, err := os.ReadFile(replicatedE2EValuesPath)
	if err != nil {
		t.Fatalf("read replicated E2E values: %v", err)
	}
	var values struct {
		CHOptimizations string `yaml:"chOptimizations"`
		ClickHouse      struct {
			Bundled struct {
				Image string `yaml:"image"`
			} `yaml:"bundled"`
		} `yaml:"clickhouse"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse replicated E2E values: %v", err)
	}
	image := values.ClickHouse.Bundled.Image
	server, ok := ParseVersion(image[strings.LastIndex(image, ":")+1:])
	if !ok {
		t.Fatalf("replicated E2E bundled image %q carries no ClickHouse version tag", image)
	}

	got := strings.Split(strings.ReplaceAll(values.CHOptimizations, "\n", ""), ",")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	want := make([]string, 0, len(Registry()))
	for _, feature := range Registry() {
		if server.AtLeast(feature.MinVersion) {
			want = append(want, feature.ID)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("replicated E2E optimizations = %v, want every registry feature its %s server can run %v", got, server, want)
	}
}
