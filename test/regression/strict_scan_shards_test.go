package regression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	strictScanWorkflowPath = ".github/workflows/strict-scan.yml"
	strictScanShardJob     = "strict-scan-shard"
	strictScanContextJob   = "strict-scan"
	strictScanLaneID       = "chdb.strict-scan"
)

// strictScanShardIf is the only condition a recipe step of the shard job may
// carry: it names the one shard that runs the step.
var strictScanShardIf = regexp.MustCompile(`^matrix\.shard == '([a-z0-9-]+)'$`)

// strictScanJustRun is a step that runs exactly one Just recipe.
var strictScanJustRun = regexp.MustCompile(`^just ([a-z0-9-]+)$`)

// TestStrictScanShards pins the partition of the strict-scan lane's steps
// across the `strict-scan-shard` matrix (tsouza/cerberus#4022). A recipe step
// runs on a shard only through its `if: matrix.shard == '<shard>'` line, so a
// condition naming a shard the matrix does not declare runs nowhere, a recipe
// step without one runs on every shard, and a shard with no step boots a
// runner for nothing. None of those turns the `strict-scan` context red on its
// own, so this test does. It also pins that the steps run exactly the recipes
// the `chdb.strict-scan` lane registers, each on one shard, and that the
// `strict-scan` context job aggregates the matrix.
func TestStrictScanShards(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot, strictScanWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", strictScanWorkflowPath, err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs    any `yaml:"needs"`
			If       any `yaml:"if"`
			Strategy struct {
				FailFast *bool `yaml:"fail-fast"`
				// A node: a sibling job's matrix is a fromJSON expression.
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", strictScanWorkflowPath, err)
	}
	job, ok := workflow.Jobs[strictScanShardJob]
	if !ok {
		t.Fatalf("%s has no %s job", strictScanWorkflowPath, strictScanShardJob)
	}
	var matrix struct {
		Shard []string `yaml:"shard"`
	}
	if err := job.Strategy.Matrix.Decode(&matrix); err != nil {
		t.Fatalf("%s#%s matrix: %v", strictScanWorkflowPath, strictScanShardJob, err)
	}
	shards := matrix.Shard
	if len(shards) == 0 {
		t.Fatalf("%s#%s declares no matrix.shard values", strictScanWorkflowPath, strictScanShardJob)
	}
	if job.Strategy.FailFast == nil || *job.Strategy.FailFast {
		t.Errorf("%s#%s must set fail-fast: false, or one shard's failure cancels its siblings' verdicts",
			strictScanWorkflowPath, strictScanShardJob)
	}

	stepsPerShard := map[string]int{}
	shardOfRecipe := map[string]string{}
	for i, step := range job.Steps {
		run := strings.TrimSpace(step.Run)
		if !strings.Contains(run, "just ") {
			continue
		}
		where := strictScanWorkflowPath + "#" + strictScanShardJob + " step " + step.Name
		m := strictScanJustRun.FindStringSubmatch(run)
		if m == nil {
			t.Errorf("%s (step %d) runs %q; a shard step runs exactly one `just <recipe>`", where, i+1, run)
			continue
		}
		recipe := m[1]
		c := strictScanShardIf.FindStringSubmatch(strings.TrimSpace(step.If))
		if c == nil {
			t.Errorf("%s (step %d) has condition %q; it must be exactly `matrix.shard == '<shard>'`",
				where, i+1, step.If)
			continue
		}
		shard := c[1]
		if !slices.Contains(shards, shard) {
			t.Errorf("%s (step %d) names shard %q, which matrix.shard %v does not declare; it runs nowhere",
				where, i+1, shard, shards)
			continue
		}
		if previous, dup := shardOfRecipe[recipe]; dup {
			t.Errorf("recipe %s runs on shard %q and again on %q", recipe, previous, shard)
		}
		shardOfRecipe[recipe] = shard
		stepsPerShard[shard]++
	}
	for _, shard := range shards {
		if stepsPerShard[shard] == 0 {
			t.Errorf("shard %q runs no recipe step", shard)
		}
	}

	registered := strictScanLaneRecipes(t)
	ran := make([]string, 0, len(shardOfRecipe))
	for recipe := range shardOfRecipe {
		ran = append(ran, recipe)
	}
	slices.Sort(ran)
	if !slices.Equal(ran, registered) {
		t.Errorf("the shard steps run recipes %v but lane %s registers %v",
			ran, strictScanLaneID, registered)
	}

	aggregate, ok := workflow.Jobs[strictScanContextJob]
	if !ok {
		t.Fatalf("%s has no %s job", strictScanWorkflowPath, strictScanContextJob)
	}
	if cond, _ := aggregate.If.(string); strings.TrimSpace(cond) != "always()" {
		t.Errorf("%s#%s must run on `if: always()`, got %v", strictScanWorkflowPath, strictScanContextJob, aggregate.If)
	}
	needs, _ := aggregate.Needs.([]any)
	if !slices.Contains(needs, any(strictScanShardJob)) || !slices.Contains(needs, any("changes")) {
		t.Errorf("%s#%s needs %v; it must need both changes and %s",
			strictScanWorkflowPath, strictScanContextJob, aggregate.Needs, strictScanShardJob)
	}
	var verdict bool
	for _, step := range aggregate.Steps {
		if strings.Contains(step.Run, "strict-scan-aggregate.mjs") && !strings.Contains(step.Run, "run-node-tests") {
			verdict = true
		}
	}
	if !verdict {
		t.Errorf("%s#%s does not run .github/scripts/strict-scan-aggregate.mjs", strictScanWorkflowPath, strictScanContextJob)
	}
}

// strictScanLaneRecipes is the sorted recipe list of the chdb.strict-scan lane
// in .github/ci-lanes.json.
func strictScanLaneRecipes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".github/ci-lanes.json"))
	if err != nil {
		t.Fatalf("read ci-lanes.json: %v", err)
	}
	var registry struct {
		Lanes []struct {
			ID      string   `json:"id"`
			Recipes []string `json:"recipes"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatalf("parse ci-lanes.json: %v", err)
	}
	for _, lane := range registry.Lanes {
		if lane.ID == strictScanLaneID {
			recipes := slices.Clone(lane.Recipes)
			slices.Sort(recipes)
			return recipes
		}
	}
	t.Fatalf("ci-lanes.json has no lane %s", strictScanLaneID)
	return nil
}
