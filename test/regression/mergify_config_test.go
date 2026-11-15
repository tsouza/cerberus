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
		BatchMaxFailureResolution     *int     `yaml:"batch_max_failure_resolution_attempts"`
		BranchProtectionInjectionMode string   `yaml:"branch_protection_injection_mode"`
	} `yaml:"queue_rules"`
	MergeQueue struct {
		MaxParallelChecks *int   `yaml:"max_parallel_checks"`
		DequeuedLabel     string `yaml:"dequeued_label"`
		QueuedLabel       string `yaml:"queued_label"`
	} `yaml:"merge_queue"`
	CommandsRestrictions map[string]struct {
		Conditions yaml.Node `yaml:"conditions"`
	} `yaml:"commands_restrictions"`
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

// batchFailureResolutionAttempts is how many split-and-recheck rounds a failed
// batch gets before its failing pull requests leave the queue. Each round is
// another full CI run, so one spurious red costs the whole batch at most one
// extra run.
const batchFailureResolutionAttempts = 1

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
		if a := rule.BatchMaxFailureResolution; a == nil || *a != batchFailureResolutionAttempts {
			t.Errorf("queue rule %q: batch_max_failure_resolution_attempts must be set to %d; got %s", rule.Name, batchFailureResolutionAttempts, describeSetting(a))
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

// requeueWorkflowPath runs the requeue scripts.
const requeueWorkflowPath = ".github/workflows/merge-queue-requeue.yml"

// requeueDequeuedJob is the job in requeueWorkflowPath that runs
// requeueDequeuedScript.
const requeueDequeuedJob = "requeue-dequeued"

// requeueDequeuedScript posts `@mergifyio queue` for an entry dequeued at an
// unchanged head; the restriction below has to let its account run it.
const requeueDequeuedScript = ".github/scripts/merge-queue-requeue-dequeued.mjs"

// requeueScriptConst reads one exported string constant out of the script.
func requeueScriptConst(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^export const ` + name + ` = '([^']+)';$`).
		FindStringSubmatch(readFileString(t, filepath.Join(repoRoot, requeueDequeuedScript)))
	if m == nil {
		t.Fatalf("%s no longer exports %s as a string literal", requeueDequeuedScript, name)
	}
	return m[1]
}

// TestMergifyAcceptsTheRequeueCommandFromTheWorkflow pins #4069's command path.
// Mergify keeps an entry it dequeued at an unchanged head out of the queue until
// it is sent the queue command, and by default runs that command only for a
// sender with write permission. GitHub reports `none` for the workflow token's
// account, so without a restriction naming it the command the script posts is
// refused and an interrupted entry stays dequeued.
func TestMergifyAcceptsTheRequeueCommandFromTheWorkflow(t *testing.T) {
	cfg := loadMergifyConfig(t)
	command := requeueScriptConst(t, "REQUEUE_COMMAND")
	verb := strings.TrimSpace(strings.TrimPrefix(command, "@mergifyio"))
	author := requeueScriptConst(t, "REQUEUE_AUTHOR")

	restriction, ok := cfg.CommandsRestrictions[verb]
	if !ok {
		t.Fatalf("%s: commands_restrictions has no %q entry, so `%s` from %s is refused", mergifyConfigPath, verb, command, author)
	}
	var conditions []string
	var collect func(n *yaml.Node)
	collect = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			conditions = append(conditions, strings.Join(strings.Fields(n.Value), " "))
		}
		for _, c := range n.Content {
			collect(c)
		}
	}
	collect(&restriction.Conditions)
	if !slices.Contains(conditions, "sender = "+author) {
		t.Errorf("%s: commands_restrictions.%s does not allow `sender = %s`; got %v", mergifyConfigPath, verb, author, conditions)
	}
	if !slices.Contains(conditions, "sender-permission >= write") {
		t.Errorf("%s: commands_restrictions.%s dropped `sender-permission >= write`; people with write access "+
			"could no longer requeue by hand. Got %v", mergifyConfigPath, verb, conditions)
	}

	for variable, got := range map[string]string{
		"DEQUEUED_LABEL": cfg.MergeQueue.DequeuedLabel,
		"QUEUED_LABEL":   cfg.MergeQueue.QueuedLabel,
	} {
		if want := requeueScriptConst(t, variable); got != "" && got != want {
			t.Errorf("%s: merge_queue sets the label %s names to %q, but %s triggers on %q",
				mergifyConfigPath, variable, got, requeueDequeuedScript, want)
		}
	}

	// The job has to receive every label event the script maps to a path: the
	// event type in `on:` and the action/label pair in the job's `if:`.
	var workflow struct {
		On struct {
			PullRequestTarget struct {
				Types []string `yaml:"types"`
			} `yaml:"pull_request_target"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFileString(t, filepath.Join(repoRoot, requeueWorkflowPath))), &workflow); err != nil {
		t.Fatalf("parse %s: %v", requeueWorkflowPath, err)
	}
	job, ok := workflow.Jobs[requeueDequeuedJob]
	if !ok {
		t.Fatalf("%s has no %s job", requeueWorkflowPath, requeueDequeuedJob)
	}
	for _, event := range []struct{ action, labelConst string }{
		{"labeled", "DEQUEUED_LABEL"},
		{"unlabeled", "QUEUED_LABEL"},
		{"labeled", "QUEUE_LABEL"},
	} {
		label := queueLabel
		if event.labelConst != "QUEUE_LABEL" {
			label = requeueScriptConst(t, event.labelConst)
		}
		if !slices.Contains(workflow.On.PullRequestTarget.Types, event.action) {
			t.Errorf("%s: pull_request_target types %v lack %q, so the %s:%s event never runs the requeue",
				requeueWorkflowPath, workflow.On.PullRequestTarget.Types, event.action, event.action, label)
		}
		pair := "github.event.action == '" + event.action + "'"
		name := "github.event.label.name == '" + label + "'"
		if !strings.Contains(job.If, pair) || !strings.Contains(job.If, name) {
			t.Errorf("%s: the %s job's `if:` does not admit %s:%s, which %s acts on; got %q",
				requeueWorkflowPath, requeueDequeuedJob, event.action, label, requeueDequeuedScript, job.If)
		}
	}
}
