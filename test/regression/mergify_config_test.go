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

// The merge queue (.github/mergify.yml) checks one pull request at a time, on
// the pull request itself. Mergify computes "in place" from three settings:
// max_parallel_checks 1, batch_size 1 on every queue rule, and no
// merge_conditions beyond the queue conditions. Any other shape makes Mergify
// test on a draft pull request it writes itself, and the description- and
// commit-scanning gates (pr-body, forbid-deferral, commitlint) would then
// judge that draft instead of the pull request being merged.

const mergifyConfigPath = ".github/mergify.yml"

// queueLabel is the label that places a pull request in the merge queue.
const queueLabel = "queue"

type mergifyConfig struct {
	QueueRules []struct {
		Name                          string   `yaml:"name"`
		QueueConditions               []string `yaml:"queue_conditions"`
		MergeConditions               []string `yaml:"merge_conditions"`
		MergeMethod                   string   `yaml:"merge_method"`
		UpdateMethod                  string   `yaml:"update_method"`
		BatchSize                     *int     `yaml:"batch_size"`
		BranchProtectionInjectionMode string   `yaml:"branch_protection_injection_mode"`
	} `yaml:"queue_rules"`
	MergeQueue struct {
		MaxParallelChecks *int `yaml:"max_parallel_checks"`
	} `yaml:"merge_queue"`
	PullRequestRules []struct {
		Name       string               `yaml:"name"`
		Conditions []string             `yaml:"conditions"`
		Actions    map[string]yaml.Node `yaml:"actions"`
	} `yaml:"pull_request_rules"`
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

func TestMergifyQueueChecksInPlace(t *testing.T) {
	cfg := loadMergifyConfig(t)
	if p := cfg.MergeQueue.MaxParallelChecks; p == nil || *p != 1 {
		t.Errorf("merge_queue.max_parallel_checks must be set to 1 (in-place checks); got %s", describeSetting(p))
	}
	if len(cfg.QueueRules) == 0 {
		t.Fatalf("%s declares no queue_rules", mergifyConfigPath)
	}
	for _, rule := range cfg.QueueRules {
		if rule.BatchSize == nil || *rule.BatchSize != 1 {
			t.Errorf("queue rule %q: batch_size must be set to 1 (in-place checks); got %s", rule.Name, describeSetting(rule.BatchSize))
		}
		if len(rule.MergeConditions) > 0 && !slices.Equal(rule.MergeConditions, rule.QueueConditions) {
			t.Errorf("queue rule %q: merge_conditions beyond queue_conditions turn off in-place checks", rule.Name)
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
		if !slices.Contains(rule.QueueConditions, "base=main") || !slices.Contains(rule.QueueConditions, "label="+queueLabel) {
			t.Errorf("queue rule %q: queue_conditions must include base=main and label=%s; got %v", rule.Name, queueLabel, rule.QueueConditions)
		}
	}
}

func TestMergifyQueueLabelQueuesThePullRequest(t *testing.T) {
	cfg := loadMergifyConfig(t)
	for _, rule := range cfg.PullRequestRules {
		if _, ok := rule.Actions["queue"]; ok && slices.Contains(rule.Conditions, "label="+queueLabel) {
			return
		}
	}
	t.Errorf("%s has no pull_request_rule that queues a pull request labelled %s", mergifyConfigPath, queueLabel)
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
