package regression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const ciLaneRegistryPath = "../../.github/ci-lanes.json"

type ciLaneRegistry struct {
	Lanes []struct {
		ID    string `json:"id"`
		Owner struct {
			Workflow     string   `json:"workflow"`
			Jobs         []string `json:"jobs"`
			ProducerJobs []string `json:"producer_jobs"`
			ContextJob   string   `json:"context_job"`
		} `json:"owner"`
		Context struct {
			Match     string `json:"match"`
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
		} `json:"context"`
		ReleasePosture string `json:"release_posture"`
	} `json:"lanes"`
	NonLaneWorkflows []struct {
		Workflow string `json:"workflow"`
	} `json:"non_lane_workflows"`
}

type ciLaneWorkflowStep struct {
	Name            string    `yaml:"name"`
	If              yaml.Node `yaml:"if"`
	Run             string    `yaml:"run"`
	Shell           yaml.Node `yaml:"shell"`
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
}

type ciLaneWorkflowDefaults struct {
	Run struct {
		Shell yaml.Node `yaml:"shell"`
	} `yaml:"run"`
}

type ciLaneWorkflowJob struct {
	Name            string                 `yaml:"name"`
	Needs           yaml.Node              `yaml:"needs"`
	If              yaml.Node              `yaml:"if"`
	ContinueOnError yaml.Node              `yaml:"continue-on-error"`
	Defaults        ciLaneWorkflowDefaults `yaml:"defaults"`
	Steps           []ciLaneWorkflowStep   `yaml:"steps"`
	Strategy        struct {
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
}

type ciLaneWorkflow struct {
	Defaults ciLaneWorkflowDefaults       `yaml:"defaults"`
	Jobs     map[string]ciLaneWorkflowJob `yaml:"jobs"`
}

type ciLaneContextPattern struct {
	Match   string
	Name    string
	LaneIDs []string
}

type ciLaneContextClaim struct {
	Workflow string
	JobID    string
	Match    string
	Name     string
	LaneIDs  []string
}

// TestCILaneRegistry binds the machine-readable registry to the workflow graph
// it describes. ci-lane-contract.mjs validates the JSON contract itself; this
// regression test validates facts that require parsing Actions YAML: total job
// ownership, the exact check names GitHub emits, and the dependency edges that
// keep an aggregate from reporting green without its producers.
func TestCILaneRegistry(t *testing.T) {
	registry := readCILaneRegistry(t)
	workflows := readCILaneWorkflows(t)
	coveredJobs := map[string]map[string][]string{}
	nonLaneWorkflows := map[string]bool{}
	contextPatterns := map[string]map[string]*ciLaneContextPattern{}
	emittedByContextJob := map[string][]string{}

	for _, classification := range registry.NonLaneWorkflows {
		workflow := classification.Workflow
		if nonLaneWorkflows[workflow] {
			t.Errorf("%s classifies workflow %q as non-lane more than once", ciLaneRegistryPath, workflow)
		}
		nonLaneWorkflows[workflow] = true
		if _, ok := workflows[workflow]; !ok {
			t.Errorf("%s classifies missing workflow %q as non-lane", ciLaneRegistryPath, workflow)
		}
	}

	protectedContexts := map[string]*ciLaneContextClaim{}
	gatingJobs := map[string]map[string]bool{}
	releaseRequiredContexts := map[string]bool{}

	for _, lane := range registry.Lanes {
		workflow, ok := workflows[lane.Owner.Workflow]
		if !ok {
			t.Errorf("registry lane %q owns missing workflow %q", lane.ID, lane.Owner.Workflow)
			continue
		}
		if nonLaneWorkflows[lane.Owner.Workflow] {
			t.Errorf("registry lane %q owns %q, which is also classified as a whole non-lane workflow",
				lane.ID, lane.Owner.Workflow)
		}

		if coveredJobs[lane.Owner.Workflow] == nil {
			coveredJobs[lane.Owner.Workflow] = map[string][]string{}
		}
		for _, jobID := range lane.Owner.Jobs {
			coveredJobs[lane.Owner.Workflow][jobID] = append(
				coveredJobs[lane.Owner.Workflow][jobID], lane.ID,
			)
			if _, exists := workflow.Jobs[jobID]; !exists {
				t.Errorf("registry lane %q owns missing job %s#%s",
					lane.ID, lane.Owner.Workflow, jobID)
			}
			if lane.Context.Protected || lane.ReleasePosture == "required" {
				identity := ciLaneJobIdentity(lane.Owner.Workflow, jobID)
				if gatingJobs[identity] == nil {
					gatingJobs[identity] = map[string]bool{}
				}
				gatingJobs[identity][lane.ID] = true
			}
		}

		contextJob, exists := workflow.Jobs[lane.Owner.ContextJob]
		if !slices.Contains(lane.Owner.Jobs, lane.Owner.ContextJob) {
			t.Errorf("registry lane %q context job %q does not appear in owner.jobs",
				lane.ID, lane.Owner.ContextJob)
		}
		if !exists {
			t.Errorf("registry lane %q names missing context job %s#%s",
				lane.ID, lane.Owner.Workflow, lane.Owner.ContextJob)
			continue
		}

		contextIdentity := ciLaneJobIdentity(lane.Owner.Workflow, lane.Owner.ContextJob)
		emitted, known := emittedByContextJob[contextIdentity]
		if !known {
			emitted = ciLaneEmittedContextNames(
				t, lane.Owner.Workflow, lane.Owner.ContextJob, contextJob,
			)
			emittedByContextJob[contextIdentity] = emitted
		}
		assertCILaneContextName(t, lane.ID, lane.Owner.Workflow, lane.Owner.ContextJob,
			emitted, lane.Context.Match, lane.Context.Name)

		if contextPatterns[contextIdentity] == nil {
			contextPatterns[contextIdentity] = map[string]*ciLaneContextPattern{}
		}
		patternKey := ciLanePatternKey(lane.Context.Match, lane.Context.Name)
		pattern, duplicatePattern := contextPatterns[contextIdentity][patternKey]
		if !duplicatePattern {
			pattern = &ciLaneContextPattern{Match: lane.Context.Match, Name: lane.Context.Name}
			contextPatterns[contextIdentity][patternKey] = pattern
		}
		pattern.LaneIDs = append(pattern.LaneIDs, lane.ID)

		// `owner.jobs` is an inventory, not an aggregate declaration: selectors,
		// setup jobs, and adjacent evidence may belong to a lane without feeding
		// its context. producer_jobs is the explicit fail-closed claim. A nonempty
		// list means the context must run even after a producer fails and directly
		// observe every named producer.
		if len(lane.Owner.ProducerJobs) > 0 {
			if !ciLaneIsAlwaysCondition(contextJob.If) {
				t.Errorf("aggregate registry lane %q context job %s#%s does not use always(); "+
					"a failed or skipped producer could prevent the aggregate context from reporting",
					lane.ID, lane.Owner.Workflow, lane.Owner.ContextJob)
			}
			needs := ciLaneNeeds(t, lane.Owner.Workflow, lane.Owner.ContextJob, contextJob.Needs)
			for _, producer := range lane.Owner.ProducerJobs {
				if !slices.Contains(lane.Owner.Jobs, producer) {
					t.Errorf("aggregate registry lane %q producer %q does not appear in owner.jobs",
						lane.ID, producer)
				}
				if producer == lane.Owner.ContextJob {
					t.Errorf("aggregate registry lane %q names context job %q as its own producer",
						lane.ID, producer)
					continue
				}
				if slices.Contains(needs, producer) {
					continue
				}
				t.Errorf("aggregate registry lane %q context job %s#%s does not depend on producer %q; "+
					"the context can report without observing that producer",
					lane.ID, lane.Owner.Workflow, lane.Owner.ContextJob, producer)
			}
		}

		if lane.Context.Protected {
			if lane.Context.Match != "exact" {
				t.Errorf("protected registry lane %q uses %q context matching; branch protection names are exact",
					lane.ID, lane.Context.Match)
			}
			claim, duplicate := protectedContexts[lane.Context.Name]
			if duplicate && (claim.Workflow != lane.Owner.Workflow ||
				claim.JobID != lane.Owner.ContextJob ||
				claim.Match != lane.Context.Match || claim.Name != lane.Context.Name) {
				t.Errorf("protected context %q is claimed by distinct registry producers: "+
					"lanes %v use %s#%s, while lane %q uses %s#%s",
					lane.Context.Name, claim.LaneIDs, claim.Workflow, claim.JobID,
					lane.ID, lane.Owner.Workflow, lane.Owner.ContextJob)
			} else if duplicate {
				claim.LaneIDs = append(claim.LaneIDs, lane.ID)
			} else {
				protectedContexts[lane.Context.Name] = &ciLaneContextClaim{
					Workflow: lane.Owner.Workflow,
					JobID:    lane.Owner.ContextJob,
					Match:    lane.Context.Match,
					Name:     lane.Context.Name,
					LaneIDs:  []string{lane.ID},
				}
			}
		}
		if lane.ReleasePosture == "required" {
			if lane.Context.Match != "exact" {
				t.Errorf("release-required registry lane %q uses %q context matching; "+
					"RELEASE_REQUIRED_CHECKS waits for exact check-run names",
					lane.ID, lane.Context.Match)
			}
			releaseRequiredContexts[lane.Context.Name] = true
		}
	}

	assertCILaneContextPatternTotality(t, contextPatterns, emittedByContextJob)
	assertProtectedCILaneContextOwners(t, workflows, protectedContexts)

	for workflowPath, workflow := range workflows {
		if nonLaneWorkflows[workflowPath] {
			continue
		}
		for jobID := range workflow.Jobs {
			if len(coveredJobs[workflowPath][jobID]) == 0 {
				t.Errorf("workflow job %s#%s is covered by neither a registry lane nor a whole-workflow classification",
					workflowPath, jobID)
			}
		}
	}

	wantProtected := slices.Clone(branchProtectionContexts)
	gotProtected := make([]string, 0, len(protectedContexts))
	for context := range protectedContexts {
		gotProtected = append(gotProtected, context)
	}
	slices.Sort(wantProtected)
	slices.Sort(gotProtected)
	if !slices.Equal(gotProtected, wantProtected) {
		t.Errorf("protected registry contexts differ from branchProtectionContexts:\n  registry only: %v\n  branch protection only: %v",
			ciLaneSetDifference(gotProtected, wantProtected),
			ciLaneSetDifference(wantProtected, gotProtected))
	}

	assertCILaneReleaseRequiredContexts(t, releaseRequiredContexts)

	for identity, laneIDs := range gatingJobs {
		workflowPath, jobID, _ := strings.Cut(identity, "#")
		job := workflows[workflowPath].Jobs[jobID]
		if !job.ContinueOnError.IsZero() {
			t.Errorf("protected or release-required registry lanes %v job %s declares continue-on-error; "+
				"failed evidence could be reported as a successful gating context",
				ciLaneSortedKeys(laneIDs), identity)
		}
		for stepIndex, step := range job.Steps {
			if step.ContinueOnError.IsZero() {
				continue
			}
			stepName := step.Name
			if stepName == "" {
				stepName = "unnamed"
			}
			t.Errorf("protected or release-required registry lanes %v job %s step %d (%s) declares "+
				"continue-on-error; "+
				"failed evidence could be reported as a successful protected context",
				ciLaneSortedKeys(laneIDs), identity, stepIndex+1, stepName)
		}
	}

	assertCILaneContractIsGated(t, workflows)
}

func readCILaneRegistry(t *testing.T) ciLaneRegistry {
	t.Helper()

	data, err := os.ReadFile(ciLaneRegistryPath)
	if err != nil {
		t.Fatalf("read %s: %v", ciLaneRegistryPath, err)
	}
	var registry ciLaneRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatalf("parse %s: %v", ciLaneRegistryPath, err)
	}
	if len(registry.Lanes) == 0 {
		t.Fatalf("%s contains no lanes, so workflow coverage would be vacuous", ciLaneRegistryPath)
	}
	return registry
}

func readCILaneWorkflows(t *testing.T) map[string]ciLaneWorkflow {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	workflows := map[string]ciLaneWorkflow{}
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		testPath := filepath.Join(workflowsDir, entry.Name())
		registryPath := filepath.ToSlash(filepath.Join(".github/workflows", entry.Name()))
		var workflow ciLaneWorkflow
		if err := yaml.Unmarshal([]byte(readFileString(t, testPath)), &workflow); err != nil {
			t.Fatalf("parse %s: %v", testPath, err)
		}
		if len(workflow.Jobs) == 0 {
			t.Fatalf("%s declares no jobs", testPath)
		}
		workflows[registryPath] = workflow
	}
	if len(workflows) == 0 {
		t.Fatalf("parsed no workflows under %s", workflowsDir)
	}
	return workflows
}

func assertCILaneContextName(
	t *testing.T,
	laneID, workflowPath, jobID string,
	emitted []string,
	match, want string,
) {
	t.Helper()

	found := false
	for _, name := range emitted {
		found = ciLanePatternMatches(match, want, name)
		if found {
			break
		}
	}
	if match != "exact" && match != "prefix" {
		t.Errorf("registry lane %q has unknown context match mode %q", laneID, match)
		return
	}
	if !found {
		t.Errorf("registry lane %q context %s %q is not emitted by %s#%s; emitted names: %v",
			laneID, match, want, workflowPath, jobID, emitted)
	}
}

func ciLaneJobIdentity(workflowPath, jobID string) string {
	return workflowPath + "#" + jobID
}

func ciLaneScalarValue(node yaml.Node) string {
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func ciLaneIsAlwaysCondition(node yaml.Node) bool {
	value := strings.TrimSpace(ciLaneScalarValue(node))
	return value == "always()" || value == "${{ always() }}"
}

func ciLaneEmittedContextNames(
	t *testing.T,
	workflowPath, jobID string,
	job ciLaneWorkflowJob,
) []string {
	t.Helper()

	name := job.Name
	if name == "" {
		name = jobID
	}
	combos := []map[string]string{{}}
	if strings.Contains(name, "${{") {
		combos = matrixCombos(t, filepath.Join(repoRoot, workflowPath), jobID, job.Strategy.Matrix)
	}
	emittedSet := map[string]bool{}
	for _, combo := range combos {
		emittedSet[expandJobName(t, workflowPath, jobID, name, combo)] = true
	}
	return ciLaneSortedKeys(emittedSet)
}

func ciLanePatternKey(match, name string) string {
	return match + "\x00" + name
}

func ciLanePatternMatches(match, pattern, emitted string) bool {
	switch match {
	case "exact":
		return emitted == pattern
	case "prefix":
		return strings.HasPrefix(emitted, pattern)
	default:
		return false
	}
}

func assertCILaneContextPatternTotality(
	t *testing.T,
	patternsByJob map[string]map[string]*ciLaneContextPattern,
	emittedByJob map[string][]string,
) {
	t.Helper()

	for identity, emittedNames := range emittedByJob {
		patterns := patternsByJob[identity]
		for _, emitted := range emittedNames {
			matching := []string{}
			for _, pattern := range patterns {
				if !ciLanePatternMatches(pattern.Match, pattern.Name, emitted) {
					continue
				}
				matching = append(matching, pattern.Match+" "+pattern.Name+" (lanes "+
					strings.Join(pattern.LaneIDs, ", ")+")")
			}
			slices.Sort(matching)
			switch len(matching) {
			case 0:
				t.Errorf("registered context job %s emits %q, but no registry context pattern covers it",
					identity, emitted)
			case 1:
				// Exactly one distinct pattern owns the emitted name. Multiple
				// logical lanes may share that same pattern and producer.
			default:
				t.Errorf("registered context job %s emits %q, which is ambiguously covered by "+
					"distinct registry context patterns: %v", identity, emitted, matching)
			}
		}
	}
}

func assertProtectedCILaneContextOwners(
	t *testing.T,
	workflows map[string]ciLaneWorkflow,
	protectedContexts map[string]*ciLaneContextClaim,
) {
	t.Helper()

	actualOwners := map[string]map[string]bool{}
	for workflowPath, workflow := range workflows {
		for jobID, job := range workflow.Jobs {
			identity := ciLaneJobIdentity(workflowPath, jobID)
			for _, emitted := range ciLaneEmittedContextNames(t, workflowPath, jobID, job) {
				if _, protected := protectedContexts[emitted]; !protected {
					continue
				}
				if actualOwners[emitted] == nil {
					actualOwners[emitted] = map[string]bool{}
				}
				actualOwners[emitted][identity] = true
			}
		}
	}

	for context, claim := range protectedContexts {
		owners := ciLaneSortedKeys(actualOwners[context])
		if len(owners) != 1 {
			t.Errorf("protected exact context %q is emitted by %d workflow jobs, want exactly one: %v",
				context, len(owners), owners)
			continue
		}
		wantOwner := ciLaneJobIdentity(claim.Workflow, claim.JobID)
		if owners[0] != wantOwner {
			t.Errorf("protected exact context %q is registered to %s but emitted by %s",
				context, wantOwner, owners[0])
		}
	}
}

func assertCILaneReleaseRequiredContexts(t *testing.T, registryContexts map[string]bool) {
	t.Helper()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	want := ciLaneStringSet(requiredChecksFromPreflight(t, job))
	got := ciLaneSortedKeys(registryContexts)
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("release-required registry contexts differ from RELEASE_REQUIRED_CHECKS in %s job %q:"+
		"\n  registry only: %v\n  release preflight only: %v",
		releaseWorkflowPath, preflightJob,
		ciLaneSetDifference(got, want), ciLaneSetDifference(want, got))
}

func ciLaneStringSet(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	return ciLaneSortedKeys(set)
}

func ciLaneSortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	slices.Sort(keys)
	return keys
}

func ciLaneNeeds(t *testing.T, workflowPath, jobID string, node yaml.Node) []string {
	t.Helper()

	switch node.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		return []string{node.Value}
	case yaml.SequenceNode:
		needs := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				t.Fatalf("%s#%s needs contains a non-scalar entry", workflowPath, jobID)
			}
			needs = append(needs, item.Value)
		}
		return needs
	default:
		t.Fatalf("%s#%s needs has unsupported YAML shape %d", workflowPath, jobID, node.Kind)
		return nil
	}
}

func ciLaneSetDifference(left, right []string) []string {
	inRight := map[string]bool{}
	for _, value := range right {
		inRight[value] = true
	}
	difference := []string{}
	for _, value := range left {
		if !inRight[value] {
			difference = append(difference, value)
		}
	}
	return difference
}

func assertCILaneContractIsGated(t *testing.T, workflows map[string]ciLaneWorkflow) {
	t.Helper()

	const ciWorkflowPath = ".github/workflows/ci.yml"
	workflow, ok := workflows[ciWorkflowPath]
	if !ok {
		t.Fatalf("missing %s", ciWorkflowPath)
	}
	job, ok := workflow.Jobs[forbidSkipJobName]
	if !ok {
		t.Fatalf("%s has no protected %q job", ciWorkflowPath, forbidSkipJobName)
	}

	if !job.ContinueOnError.IsZero() {
		t.Errorf("%s job %q declares continue-on-error, so the registry contract is not a gate",
			ciWorkflowPath, forbidSkipJobName)
	}
	if !workflow.Defaults.Run.Shell.IsZero() || !job.Defaults.Run.Shell.IsZero() {
		t.Errorf("%s job %q inherits a workflow/job shell override; the registry contract requires "+
			"GitHub's default fail-fast shell", ciWorkflowPath, forbidSkipJobName)
	}

	foundCommands := false
	foundGate := false
	for stepIndex, step := range job.Steps {
		if !slices.Equal(ciLaneRunLines(step.Run), ciLaneContractCommands) {
			continue
		}
		foundCommands = true
		stepName := step.Name
		if stepName == "" {
			stepName = "unnamed"
		}
		valid := true
		if !step.If.IsZero() {
			t.Errorf("%s job %q step %d (%s) conditionally runs the lane contract; the gate step's "+
				"if field must be absent", ciWorkflowPath, forbidSkipJobName, stepIndex+1, stepName)
			valid = false
		}
		if !step.ContinueOnError.IsZero() {
			t.Errorf("%s job %q step %d (%s) declares continue-on-error; a contract failure could "+
				"report green", ciWorkflowPath, forbidSkipJobName, stepIndex+1, stepName)
			valid = false
		}
		if !step.Shell.IsZero() {
			t.Errorf("%s job %q step %d (%s) overrides shell; the default GitHub bash wrapper's "+
				"fail-fast semantics are part of the lane-contract gate", ciWorkflowPath,
				forbidSkipJobName, stepIndex+1, stepName)
			valid = false
		}
		if valid {
			foundGate = true
		}
	}
	if !foundCommands {
		t.Errorf("%s job %q has no single step whose exact two-line run body is %q; comments, "+
			"reordering, extra commands, or failure-masking suffixes do not satisfy the gate",
			ciWorkflowPath, forbidSkipJobName, ciLaneContractCommands)
	} else if !foundGate {
		t.Errorf("%s job %q runs the lane-contract commands, but no matching step is unconditional, "+
			"fail-closed, and on the default shell", ciWorkflowPath, forbidSkipJobName)
	}
}

var ciLaneContractCommands = []string{
	"node --test .github/scripts/ci-lane-contract.test.mjs",
	"MODE=registry node .github/scripts/ci-lane-contract.mjs",
}

func ciLaneRunLines(run string) []string {
	run = strings.TrimSpace(run)
	if run == "" {
		return nil
	}
	lines := strings.Split(run, "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return lines
}

func ciLaneContractStepIsGate(step ciLaneWorkflowStep) bool {
	return step.If.IsZero() &&
		step.ContinueOnError.IsZero() &&
		step.Shell.IsZero() &&
		slices.Equal(ciLaneRunLines(step.Run), ciLaneContractCommands)
}

func TestCILaneContractStepRequiresFailClosedCommands(t *testing.T) {
	t.Parallel()

	validRun := strings.Join(ciLaneContractCommands, "\n")
	scalar := func(value string) yaml.Node {
		return yaml.Node{Kind: yaml.ScalarNode, Value: value}
	}
	tests := []struct {
		name string
		step ciLaneWorkflowStep
		want bool
	}{
		{name: "exact", step: ciLaneWorkflowStep{Run: validRun}, want: true},
		{name: "comment only", step: ciLaneWorkflowStep{Run: "# " +
			strings.Join(ciLaneContractCommands, "\n# ")}},
		{name: "or true", step: ciLaneWorkflowStep{Run: validRun + " || true"}},
		{name: "conditional", step: ciLaneWorkflowStep{Run: validRun, If: scalar("success()")}},
		{name: "missing command", step: ciLaneWorkflowStep{Run: ciLaneContractCommands[0]}},
		{name: "reordered", step: ciLaneWorkflowStep{Run: strings.Join([]string{
			ciLaneContractCommands[1], ciLaneContractCommands[0],
		}, "\n")}},
		{name: "set plus e", step: ciLaneWorkflowStep{Run: "set +e\n" + validRun}},
		{name: "continue on error", step: ciLaneWorkflowStep{
			Run: validRun, ContinueOnError: scalar("true"),
		}},
		{name: "shell override", step: ciLaneWorkflowStep{
			Run: validRun, Shell: scalar("bash {0}"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ciLaneContractStepIsGate(test.step); got != test.want {
				t.Errorf("ciLaneContractStepIsGate() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCILaneAggregateRequiresUnconditionalAlways(t *testing.T) {
	t.Parallel()

	scalar := func(value string) yaml.Node {
		return yaml.Node{Kind: yaml.ScalarNode, Value: value}
	}
	for name, test := range map[string]struct {
		node yaml.Node
		want bool
	}{
		"plain":              {node: scalar("always()"), want: true},
		"expression wrapper": {node: scalar("${{ always() }}"), want: true},
		"absent":             {},
		"false and always":   {node: scalar("false && always()")},
		"always and false":   {node: scalar("always() && false")},
		"success":            {node: scalar("success()")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ciLaneIsAlwaysCondition(test.node); got != test.want {
				t.Errorf("ciLaneIsAlwaysCondition() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCILaneWorkflowYAMLExtensions(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"ci.yml":        true,
		"security.yaml": true,
		"UPPER.YAML":    true,
		"README.md":     false,
		"ci.yml.bak":    false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isWorkflowYAML(name); got != want {
				t.Errorf("isWorkflowYAML(%q) = %v, want %v", name, got, want)
			}
		})
	}
}
