package regression

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The `cerberus-heavy` self-hosted runner pool carries the required PR jobs
// `lint`, `check-test` and `check-build` (and, through them, the `check`
// rollup). Any other job routed to the pool competes with them for the same
// runners. When those other jobs can fill every runner at once, a single
// push to main blocks every open pull request's merge gate until they drain.
//
// This gate bounds that. For every workflow, it computes the most heavy-pool
// runners the workflow's non-required jobs can hold at the same moment:
// the widest set of jobs no `needs:` chain orders, each weighted by its
// matrix width capped by `strategy.max-parallel`. The sum across workflows
// must leave heavyPoolReservedRunners runners free.
const (
	heavyPoolLabel = "cerberus-heavy"
	// heavyPoolRunners is the replica count of the cerberus-heavy-runner
	// Deployment on the self-hosted runner node (the cirunners manifest
	// `cerberus-heavy-runner.yaml`, `replicas: 3`), which is also the number
	// of runners carrying this label that GET /repos/{repo}/actions/runners
	// lists. The node has no CPU headroom to add replicas.
	heavyPoolRunners = 3
	// heavyPoolReservedRunners is how many heavy runners non-required jobs
	// may never occupy, so a required PR job always finds one.
	heavyPoolReservedRunners = 1
)

// requiredHeavyJobs are the required-status-check jobs the reservation
// protects, by workflow file and job id. They are exempt from the cap, and
// each must still be routed to the pool, so the reservation cannot silently
// protect jobs that have moved elsewhere.
var requiredHeavyJobs = map[string][]string{
	"ci.yml": {"check-build", "check-test", "lint"},
}

type poolWorkflow struct {
	Jobs map[string]poolJob `yaml:"jobs"`
}

type poolJob struct {
	RunsOn   any          `yaml:"runs-on"`
	Needs    any          `yaml:"needs"`
	Strategy poolStrategy `yaml:"strategy"`
}

type poolStrategy struct {
	MaxParallel any `yaml:"max-parallel"`
	Matrix      any `yaml:"matrix"`
}

// poolNode is one job the scheduler may place on the pool, reduced to the
// two facts peak occupancy needs: how many runners it holds at once, and
// which jobs it transitively waits for.
type poolNode struct {
	id    string
	width int
	needs []string
}

func TestNonRequiredJobsLeaveAHeavyRunnerFree(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}

	total := 0
	var breakdown []string
	seenRequired := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		var wf poolWorkflow
		if err := yaml.Unmarshal([]byte(readFileString(t, filepath.Join(workflowsDir, name))), &wf); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		nodes, required, err := heavyPoolNodes(name, wf)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range required {
			seenRequired[name+"/"+id] = true
		}
		if len(nodes) == 0 {
			continue
		}
		peak := peakOccupancy(nodes)
		total += peak
		breakdown = append(breakdown, fmt.Sprintf("%s=%d", name, peak))
	}

	for wf, ids := range requiredHeavyJobs {
		for _, id := range ids {
			if !seenRequired[wf+"/"+id] {
				t.Errorf("required job %s/%s is not routed to %s; the reservation protects a job that is not there",
					wf, id, heavyPoolLabel)
			}
		}
	}
	if total == 0 {
		t.Fatalf("no non-required job routes to %s; the cap would pass vacuously", heavyPoolLabel)
	}
	if limit := heavyPoolRunners - heavyPoolReservedRunners; total > limit {
		t.Errorf("non-required jobs can hold %d of the %d %s runners at once (%s); at most %d may, so a "+
			"required PR job always finds a free runner. Cap the matrix with strategy.max-parallel or "+
			"order the jobs with needs:", total, heavyPoolRunners, heavyPoolLabel,
			strings.Join(breakdown, ", "), limit)
	}
}

// heavyPoolNodes returns the workflow's non-required heavy-pool jobs, with
// needs resolved transitively through every job in the workflow, and the ids
// of the required jobs it found on the pool.
func heavyPoolNodes(workflow string, wf poolWorkflow) ([]poolNode, []string, error) {
	var nodes []poolNode
	var required []string
	for id, job := range wf.Jobs {
		if !slices.Contains(stringList(job.RunsOn), heavyPoolLabel) {
			continue
		}
		if slices.Contains(requiredHeavyJobs[workflow], id) {
			required = append(required, id)
			continue
		}
		width, err := jobWidth(job.Strategy)
		if err != nil {
			return nil, nil, fmt.Errorf("%s/%s: %w", workflow, id, err)
		}
		nodes = append(nodes, poolNode{id: id, width: width, needs: transitiveNeeds(wf, id)})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	return nodes, required, nil
}

// jobWidth is how many runners one job can hold at once: its matrix leg
// count, capped by max-parallel. A matrix whose size is not written in the
// file (an include list, a fromJSON expression) needs an explicit literal
// max-parallel, or its width is unknowable and the job is rejected.
func jobWidth(s poolStrategy) (int, error) {
	maxParallel := 0
	if s.MaxParallel != nil {
		n, err := strconv.Atoi(fmt.Sprint(s.MaxParallel))
		if err != nil || n < 1 {
			return 0, fmt.Errorf("max-parallel %v is not a positive literal", s.MaxParallel)
		}
		maxParallel = n
	}
	legs, known := 1, true
	matrix, isMap := s.Matrix.(map[string]any)
	if s.Matrix != nil && !isMap {
		known = false
	}
	for key, value := range matrix {
		if key == "exclude" {
			known = false
			continue
		}
		list, ok := value.([]any)
		if !ok || key == "include" {
			known = false
			continue
		}
		legs *= len(list)
	}
	switch {
	case !known && maxParallel == 0:
		return 0, fmt.Errorf("matrix size is not literal and no max-parallel bounds it")
	case !known:
		return maxParallel, nil
	case maxParallel > 0 && maxParallel < legs:
		return maxParallel, nil
	default:
		return legs, nil
	}
}

func transitiveNeeds(wf poolWorkflow, id string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(j string) {
		for _, n := range stringList(wf.Jobs[j].Needs) {
			if !seen[n] {
				seen[n] = true
				walk(n)
			}
		}
	}
	walk(id)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// peakOccupancy is the heaviest set of jobs that can run at the same time:
// no member transitively needs another. The node count per workflow is small,
// so every subset is tried.
func peakOccupancy(nodes []poolNode) int {
	best := 0
	for mask := 1; mask < 1<<len(nodes); mask++ {
		weight, ok := 0, true
		for i := range nodes {
			if mask&(1<<i) == 0 {
				continue
			}
			for j := range nodes {
				if mask&(1<<j) != 0 && slices.Contains(nodes[i].needs, nodes[j].id) {
					ok = false
				}
			}
			weight += nodes[i].width
		}
		if ok && weight > best {
			best = weight
		}
	}
	return best
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	default:
		return nil
	}
}

// TestPeakOccupancyModel pins the counting rule itself against hand-computed
// shapes, including the unbounded coverage matrix that filled the pool.
func TestPeakOccupancyModel(t *testing.T) {
	t.Parallel()

	parse := func(src string) poolWorkflow {
		var wf poolWorkflow
		if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
			t.Fatal(err)
		}
		return wf
	}
	heavy := "runs-on: [self-hosted, cerberus-heavy]"
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"unbounded parallel matrices add up", `
jobs:
  plan: {runs-on: ubuntu-latest}
  a: {needs: plan, ` + heavy + `}
  b: {needs: plan, ` + heavy + `, strategy: {matrix: {shard: [1, 2, 3, 4]}}}
  c: {needs: plan, ` + heavy + `, strategy: {matrix: {leg: [2, 3]}}}
`, 7},
		{"max-parallel and a needs chain bound the peak", `
jobs:
  plan: {runs-on: ubuntu-latest}
  a: {needs: plan, ` + heavy + `}
  b: {needs: plan, ` + heavy + `, strategy: {max-parallel: 1, matrix: {shard: [1, 2, 3, 4]}}}
  c: {needs: [plan, a], ` + heavy + `, strategy: {max-parallel: 1, matrix: {leg: [2, 3]}}}
`, 2},
		{"needs is transitive through off-pool jobs", `
jobs:
  a: {` + heavy + `}
  mid: {needs: a, runs-on: ubuntu-latest}
  c: {needs: mid, ` + heavy + `, strategy: {matrix: {x: [1, 2], y: [1, 2]}}}
`, 4},
		{"light-pool jobs do not count", `
jobs:
  a: {runs-on: [self-hosted, cerberus], strategy: {matrix: {x: [1, 2, 3]}}}
  b: {` + heavy + `}
`, 1},
	}
	for _, tc := range cases {
		nodes, _, err := heavyPoolNodes("x.yml", parse(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := peakOccupancy(nodes); got != tc.want {
			t.Errorf("%s: peak %d, want %d", tc.name, got, tc.want)
		}
	}

	if _, _, err := heavyPoolNodes("x.yml", parse(`
jobs:
  a:
    `+heavy+`
    strategy:
      matrix: ${{ fromJSON(needs.p.outputs.m) }}
`)); err == nil {
		t.Errorf("a heavy job with an unsized matrix and no max-parallel was accepted")
	}
}
