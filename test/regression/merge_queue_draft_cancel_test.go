package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Mergify validates a queued pull request through a draft pull request of its
// own on a `mergify/merge-queue/<id>` branch. That draft's runs arrive as
// `pull_request` events, not `merge_group`, and it receives several events in
// a few seconds. A workflow whose concurrency cancels in-progress
// `pull_request` runs therefore cancels its own queue runs, and Mergify reads
// a cancelled required check as interrupted and dequeues the entry:
//
//	❌ Left the queue · checks interrupted · draft #4068
//	The merge conditions cannot be satisfied: the checks were interrupted
//	before producing a result ("checks": ["pr-body"])
//
// A later run of the same check passed, but the entry had already left the
// queue, and nothing re-adds it.
//
// This test pins the guard: in every concurrency block (workflow- or
// job-level) of a workflow a draft's events reach (`pull_request` or
// `pull_request_target`), each top-level `||` branch of `cancel-in-progress`
// either carries `!startsWith(github.head_ref, '<draft prefix>')` as a
// top-level conjunct or requires an event a draft never sends
// (`github.event_name == 'push'`, `'schedule'`, ...).

const (
	mergeQueueDraftLib = "../../.github/scripts/lib/merge-queue-draft.mjs"
	workflowsGlob      = "../../.github/workflows/*.yml"
)

// mergeQueuePrefixRE reads the draft branch prefix from the one place the
// scripts define it, so the workflows and the scripts cannot disagree.
var mergeQueuePrefixRE = regexp.MustCompile(`export const MERGE_QUEUE_BRANCH_PREFIX = '([^']+)';`)

// eventPinRE matches a conjunct that requires one specific event.
var eventPinRE = regexp.MustCompile(`^github\.event_name\s*==\s*'([a-z_]+)'$`)

// draftEvents are the events a merge-queue draft pull request sends.
var draftEvents = map[string]bool{"pull_request": true, "pull_request_target": true}

type cancelConcurrency struct {
	CancelInProgress any `yaml:"cancel-in-progress"`
}

type cancelWorkflow struct {
	On          yaml.Node          `yaml:"on"`
	Concurrency *cancelConcurrency `yaml:"concurrency"`
	Jobs        map[string]struct {
		Concurrency *cancelConcurrency `yaml:"concurrency"`
	} `yaml:"jobs"`
}

// triggersOn reports whether a workflow's `on:` (string, list or map form)
// names the event.
func triggersOn(on yaml.Node, event string) bool {
	switch on.Kind {
	case yaml.ScalarNode:
		return on.Value == event
	case yaml.SequenceNode:
		for _, n := range on.Content {
			if n.Value == event {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(on.Content); i += 2 {
			if on.Content[i].Value == event {
				return true
			}
		}
	}
	return false
}

// topLevelDisjuncts splits an expression on its top-level `||` operators
// (parentheses respected), stripping `${{ … }}` and one pair of enclosing
// parentheses per branch.
func topLevelDisjuncts(expr string) []string {
	expr = strings.TrimSpace(expr)
	expr = strings.TrimSuffix(strings.TrimPrefix(expr, "${{"), "}}")
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 && i+1 < len(expr) && expr[i+1] == '|' {
				out = append(out, expr[start:i])
				i++
				start = i + 1
			}
		}
	}
	out = append(out, expr[start:])
	for i, b := range out {
		out[i] = strings.TrimSpace(unwrapEnclosingParens(strings.TrimSpace(b)))
	}
	return out
}

// branchExcludesDrafts reports whether one `||` branch is false on every
// merge-queue draft run: it carries the guard, or it requires an event a
// draft never sends.
func branchExcludesDrafts(branch, guard string) bool {
	for _, conjunct := range workflowIfConjuncts(branch) {
		if conjunct == guard {
			return true
		}
		if m := eventPinRE.FindStringSubmatch(conjunct); m != nil && !draftEvents[m[1]] {
			return true
		}
	}
	return false
}

// cancelsDraftRuns reports why a concurrency block can cancel a merge-queue
// draft's run, or "" when it cannot.
func cancelsDraftRuns(c *cancelConcurrency, guard string) string {
	if c == nil || c.CancelInProgress == nil {
		return ""
	}
	switch v := c.CancelInProgress.(type) {
	case bool:
		if v {
			return "cancel-in-progress is literally true"
		}
		return ""
	case string:
		for _, branch := range topLevelDisjuncts(v) {
			if !branchExcludesDrafts(branch, guard) {
				return "cancel-in-progress `" + v + "` has a branch, `" + branch +
					"`, that can be true on a draft's event and has no top-level `" + guard + "` conjunct"
			}
		}
		return ""
	default:
		return "cancel-in-progress has an unreadable value"
	}
}

func TestPullRequestWorkflowsNeverCancelMergeQueueDraftRuns(t *testing.T) {
	lib, err := os.ReadFile(mergeQueueDraftLib)
	if err != nil {
		t.Fatal(err)
	}
	m := mergeQueuePrefixRE.FindSubmatch(lib)
	if m == nil {
		t.Fatalf("%s no longer exports MERGE_QUEUE_BRANCH_PREFIX in the form this guard reads", mergeQueueDraftLib)
	}
	guard := "!startsWith(github.head_ref, '" + string(m[1]) + "')"

	paths, err := filepath.Glob(workflowsGlob)
	if err != nil {
		t.Fatal(err)
	}
	draftWorkflows := 0
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var wf cancelWorkflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !triggersOn(wf.On, "pull_request") && !triggersOn(wf.On, "pull_request_target") {
			continue
		}
		draftWorkflows++
		name := filepath.Base(p)
		if why := cancelsDraftRuns(wf.Concurrency, guard); why != "" {
			t.Errorf("%s: workflow concurrency: %s", name, why)
		}
		for jobID, job := range wf.Jobs {
			if why := cancelsDraftRuns(job.Concurrency, guard); why != "" {
				t.Errorf("%s: job %s concurrency: %s", name, jobID, why)
			}
		}
	}
	if draftWorkflows == 0 {
		t.Fatal("no workflow triggers on a draft's event; the guard selects nothing")
	}
}

func TestCancelsDraftRunsReadsTheExpression(t *testing.T) {
	const guard = "!startsWith(github.head_ref, 'mergify/merge-queue/')"
	cases := []struct {
		name   string
		value  any
		cancel bool
	}{
		{"absent", nil, false},
		{"literal false", false, false},
		{"literal true", true, true},
		{"pull_request only", "${{ github.event_name == 'pull_request' }}", true},
		{"guarded", "${{ github.event_name == 'pull_request' && " + guard + " }}", false},
		{"guard under an or", "${{ github.event_name == 'pull_request' || " + guard + " }}", true},
		{"guard for another prefix", "${{ !startsWith(github.head_ref, 'other/') }}", true},
		{"push or schedule only", "${{ (github.event_name == 'push' && github.ref == 'refs/heads/main') || (github.event_name == 'schedule') }}", false},
		{"push or an unguarded pull_request", "${{ (github.event_name == 'push') || (github.event_name == 'pull_request') }}", true},
		{"pull_request_target pin", "${{ github.event_name == 'pull_request_target' }}", true},
		{"merge_group excluded only", "${{ github.event_name != 'merge_group' }}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c *cancelConcurrency
			if tc.value != nil {
				c = &cancelConcurrency{CancelInProgress: tc.value}
			}
			if got := cancelsDraftRuns(c, guard) != ""; got != tc.cancel {
				t.Fatalf("cancelsDraftRuns(%v) cancels = %v, want %v", tc.value, got, tc.cancel)
			}
		})
	}
}
