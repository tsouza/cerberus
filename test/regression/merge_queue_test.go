package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A merge queue validates a PR against the trunk it would actually create: it
// builds a projected commit (the PR replayed on top of `main` plus every entry
// ahead of it) and dispatches the `merge_group` event against it. Branch
// protection then waits for the SAME required contexts on that projected
// commit. Two properties have to hold for that to terminate, and neither is
// visible in a workflow file read on its own:
//
//   - Every required context must POST on `merge_group`. A workflow without the
//     trigger produces no check-run there, the entry's status never resolves,
//     and the queue stalls with the PR neither merged nor rejected — the
//     failure mode is silence, not a red X.
//   - No merge-group run may be CANCELLED by a concurrency group. GitHub reads
//     a cancelled check-run as a failed one and dequeues the entry, so a
//     `cancel-in-progress` that is true under `merge_group` turns the queue's
//     own batching into a source of spurious dequeues.
//
// These pins hold both properties. They are cheap to satisfy and expensive to
// rediscover: a stalled queue reports nothing at all.

// mergeGroupTrigger is the workflow event GitHub dispatches against a queue's
// projected trunk.
const mergeGroupTrigger = "merge_group"

// queueExternalContexts are required contexts posted by a GitHub app rather
// than by a workflow in this repository, mapped to the producer.
//
// `CodeQL` comes from code-scanning DEFAULT setup, which builds its own
// analysis workflow server-side and dispatches it on `push` and
// `pull_request` only — there is no file here to add a trigger to, and no
// setting that adds `merge_group`. It is therefore the one required context a
// merge queue cannot satisfy: an entry waits for a `CodeQL` check-run that the
// app never posts. Resolving it is a repository-settings decision (drop the
// context from the required set, or migrate to ADVANCED setup — disable
// default setup and land a `codeql.yml` carrying the trigger), not a code
// change, so this pin records the constraint instead of asserting past it.
// Tracked in #1558.
//
// The map is not a tolerance list: the test below asserts in BOTH directions.
// An entry here must have no workflow owner, so the moment `CodeQL` becomes a
// workflow in this repo the entry has to be deleted, and deleting it puts the
// context straight back under the `merge_group` requirement.
var queueExternalContexts = map[string]string{
	"CodeQL": "code-scanning default setup (the github-advanced-security app)",
}

// onDeclares reports whether a workflow's `on:` block names an event. All three
// legal shapes are handled — a mapping (`on: {push: …}`), a sequence
// (`on: [push, pull_request]`) and a bare scalar (`on: push`) — because a
// mapping-only reader would silently answer "no" for the other two.
func onDeclares(on yaml.Node, event string) bool {
	switch on.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if on.Content[i].Value == event {
				return true
			}
		}
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if item.Value == event {
				return true
			}
		}
	case yaml.ScalarNode:
		return on.Value == event
	}
	return false
}

func TestEveryRequiredContextPostsOnTheMergeGroup(t *testing.T) {
	t.Parallel()

	owners := workflowCheckOwners(t)

	for _, ctx := range branchProtectionContexts {
		owner, ok := owners.lookup(ctx)

		if producer, external := queueExternalContexts[ctx]; external {
			if ok {
				t.Errorf("required context %q is recorded as posted by %s, but %s declares a job "+
					"with that name. Delete its queueExternalContexts entry — a context this repo "+
					"owns has to carry the `%s:` trigger like every other required lane.",
					ctx, producer, owner.file, mergeGroupTrigger)
			}
			continue
		}

		if !ok {
			t.Errorf("required context %q is posted by no job in %s and is not recorded in "+
				"queueExternalContexts. A merge queue waits for every required context on the "+
				"projected trunk, so an unattributed one stalls the queue in silence.", ctx, workflowsDir)
			continue
		}
		if !owner.mergeGroup {
			t.Errorf("required context %q is owned by %s, which does not declare the `%s:` trigger. "+
				"Branch protection waits for this context on a merge queue's projected commit, and "+
				"no run would ever post it there: the entry never resolves and the queue stalls.",
				ctx, owner.file, mergeGroupTrigger)
		}
	}
}

// queueSafeCancelInProgress matches the one expression form vetted as false
// under `merge_group`: a comparison of the event name against a single other
// event. Any other shape has to be reasoned about before it is added here —
// the literal `false` is handled separately, and an unrecognised expression
// fails rather than being assumed harmless.
var queueSafeCancelInProgress = regexp.MustCompile(
	`^\$\{\{\s*github\.event_name\s*==\s*'([a-z_]+)'\s*\}\}$`,
)

// cancelInProgressIsQueueSafe reports whether a `cancel-in-progress:` value can
// never be true on a `merge_group` run.
func cancelInProgressIsQueueSafe(value string) bool {
	if value == "false" {
		return true
	}
	m := queueSafeCancelInProgress.FindStringSubmatch(value)
	return m != nil && m[1] != mergeGroupTrigger
}

// concurrencySite is one `concurrency:` block and where it was found.
type concurrencySite struct {
	where            string
	cancelInProgress yaml.Node
}

// concurrencySites returns every concurrency block in a workflow — the
// workflow-level one plus any job-level one. Both cancel a run, so both matter.
func concurrencySites(path string, doc mergeQueueWorkflowDoc) []concurrencySite {
	sites := make([]concurrencySite, 0, 1+len(doc.Jobs))
	if doc.Concurrency.Kind != 0 {
		sites = append(sites, concurrencySite{
			where:            path,
			cancelInProgress: cancelInProgressNode(doc.Concurrency),
		})
	}
	for id, jb := range doc.Jobs {
		if jb.Concurrency.Kind == 0 {
			continue
		}
		sites = append(sites, concurrencySite{
			where:            path + " job " + id,
			cancelInProgress: cancelInProgressNode(jb.Concurrency),
		})
	}
	return sites
}

// cancelInProgressNode pulls the `cancel-in-progress:` value out of a
// concurrency block. A block that omits the key leaves a zero node, which the
// caller reads as GitHub's default of false.
func cancelInProgressNode(concurrency yaml.Node) yaml.Node {
	if concurrency.Kind != yaml.MappingNode {
		return yaml.Node{}
	}
	for i := 0; i+1 < len(concurrency.Content); i += 2 {
		if concurrency.Content[i].Value == "cancel-in-progress" {
			return *concurrency.Content[i+1]
		}
	}
	return yaml.Node{}
}

// mergeQueueWorkflowDoc is the slice of a workflow this file reads.
type mergeQueueWorkflowDoc struct {
	On          yaml.Node `yaml:"on"`
	Concurrency yaml.Node `yaml:"concurrency"`
	Jobs        map[string]struct {
		Concurrency yaml.Node `yaml:"concurrency"`
	} `yaml:"jobs"`
}

func TestMergeGroupRunsAreNeverCancelledByConcurrency(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}

	queued := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yml" {
			continue
		}
		path := filepath.Join(workflowsDir, e.Name())

		var doc mergeQueueWorkflowDoc
		if err := yaml.Unmarshal([]byte(readFileString(t, path)), &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if !onDeclares(doc.On, mergeGroupTrigger) {
			continue
		}
		queued++

		for _, site := range concurrencySites(path, doc) {
			if site.cancelInProgress.Kind == 0 {
				continue // absent: GitHub defaults to false
			}
			value := strings.TrimSpace(site.cancelInProgress.Value)
			if cancelInProgressIsQueueSafe(value) {
				continue
			}
			t.Errorf("%s runs on `%s:` and its concurrency block sets "+
				"`cancel-in-progress: %s`, which is not provably false there. GitHub reads a "+
				"cancelled check-run as a failed one, so a cancelled merge-group run DEQUEUES the "+
				"pull request it was validating — the queue would knock out green PRs on its own "+
				"batching. Use `false`, or an expression this test recognises as false under `%s`.",
				site.where, mergeGroupTrigger, value, mergeGroupTrigger)
		}
	}

	if queued == 0 {
		t.Fatalf("no workflow under %s declares the `%s:` trigger, so no required context can "+
			"report on a queue's projected trunk", workflowsDir, mergeGroupTrigger)
	}
}
