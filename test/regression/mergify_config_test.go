package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The merge queue (.github/mergify.yml) checks queued pull requests in
// batches, one batch at a time, on a draft pull request Mergify opens holding
// main plus the batch. Each check is a full CI run on the shared runners, so
// a second batch checked in parallel would double the load on them. The main
// ruleset, whose required checks the queue inherits through injection, still
// requires each pull request's own checks before it merges.

const mergifyConfigPath = ".github/mergify.yml"

// queueLabel is the label that places a pull request in the merge queue.
const queueLabel = "queue"

type mergifyConfig struct {
	QueueRules []struct {
		Name                          string   `yaml:"name"`
		QueueConditions               []string `yaml:"queue_conditions"`
		MergeMethod                   string   `yaml:"merge_method"`
		UpdateMethod                  string   `yaml:"update_method"`
		BatchSize                     *int     `yaml:"batch_size"`
		BranchProtectionInjectionMode string   `yaml:"branch_protection_injection_mode"`
	} `yaml:"queue_rules"`
	MergeQueue struct {
		MaxParallelChecks *int `yaml:"max_parallel_checks"`
	} `yaml:"merge_queue"`
	MergeProtectionsSettings struct {
		AutoMergeConditions yaml.Node `yaml:"auto_merge_conditions"`
	} `yaml:"merge_protections_settings"`
}

// describeSetting renders an optional integer setting for a failure message.
func describeSetting(v *int) string {
	if v == nil {
		return "unset"
	}
	return strconv.Itoa(*v)
}

func loadMergifyConfig(t *testing.T) mergifyConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, mergifyConfigPath))
	if err != nil {
		t.Fatalf("read %s: %v", mergifyConfigPath, err)
	}
	var cfg mergifyConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", mergifyConfigPath, err)
	}
	return cfg
}

// minBatchSize is the smallest batch_size that checks more than one pull
// request per CI run.
const minBatchSize = 2

func TestMergifyQueueBatchesOneAtATime(t *testing.T) {
	cfg := loadMergifyConfig(t)
	if p := cfg.MergeQueue.MaxParallelChecks; p == nil || *p != 1 {
		t.Errorf("merge_queue.max_parallel_checks must be set to 1 (one batch at a time); got %s", describeSetting(p))
	}
	if len(cfg.QueueRules) == 0 {
		t.Fatalf("%s declares no queue_rules", mergifyConfigPath)
	}
	for _, rule := range cfg.QueueRules {
		if rule.BatchSize == nil || *rule.BatchSize < minBatchSize {
			t.Errorf("queue rule %q: batch_size must be at least %d (batching); got %s", rule.Name, minBatchSize, describeSetting(rule.BatchSize))
		}
		if rule.MergeMethod != "squash" {
			t.Errorf("queue rule %q: merge_method = %q, want squash", rule.Name, rule.MergeMethod)
		}
		// main keeps merge commits from "Update branch"; a rebase update would
		// rewrite the pull request's history under its author.
		if rule.UpdateMethod != "merge" {
			t.Errorf("queue rule %q: update_method = %q, want merge", rule.Name, rule.UpdateMethod)
		}
		// The ruleset's required checks reach the queue only through injection.
		if m := rule.BranchProtectionInjectionMode; m != "" && m != "queue" {
			t.Errorf("queue rule %q: branch_protection_injection_mode = %q drops the ruleset's required checks from queueing", rule.Name, m)
		}
		if !slices.Contains(rule.QueueConditions, "base=main") {
			t.Errorf("queue rule %q: queue_conditions must include base=main; got %v", rule.Name, rule.QueueConditions)
		}
	}
}

// The label queues a pull request through auto_merge_conditions: true, which
// queues every pull request that meets the queue conditions. A
// pull_request_rules queue action needs Mergify's Workflow Automation, which
// this repository does not run, so such a rule never fires.
func TestMergifyQueueLabelQueuesThePullRequest(t *testing.T) {
	cfg := loadMergifyConfig(t)
	if n := cfg.MergeProtectionsSettings.AutoMergeConditions; n.Kind != yaml.ScalarNode || n.Value != "true" {
		t.Errorf("%s: merge_protections_settings.auto_merge_conditions must be true, so a labelled pull request is queued", mergifyConfigPath)
	}
	for _, rule := range cfg.QueueRules {
		if !slices.Contains(rule.QueueConditions, "label="+queueLabel) {
			t.Errorf("queue rule %q: queue_conditions must include label=%s, or every pull request is queued", rule.Name, queueLabel)
		}
	}
}

// directMergePattern matches a workflow command that merges a pull request
// itself, around the queue.
var directMergePattern = regexp.MustCompile(`gh\s+pr\s+merge\b`)

func TestNoWorkflowMergesAroundTheQueue(t *testing.T) {
	const dir = ".github/workflows"
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isWorkflowYAML(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(repoRoot, dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			code, _, _ := strings.Cut(line, "#")
			if directMergePattern.MatchString(code) {
				t.Errorf("%s/%s:%d merges a pull request around the merge queue; add the %q label instead", dir, e.Name(), i+1, queueLabel)
			}
		}
	}
}
