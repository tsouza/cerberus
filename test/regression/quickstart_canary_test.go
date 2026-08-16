package regression

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	quickstartWorkflowPath = "../../.github/workflows/quickstart.yml"
	quickstartScriptPath   = "../../.github/scripts/quickstart-canary.mjs"
	quickstartReadmePath   = "../../README.md"
	quickstartSeedPath     = "../../test/e2e/seed/cmd/seed/main.go"
	projectedSHAExpression = "${{ github.sha }}"
	quickstartRunTimeout   = 25
)

type quickstartWorkflowStep struct {
	ID              string         `yaml:"id"`
	Name            string         `yaml:"name"`
	Uses            string         `yaml:"uses"`
	If              string         `yaml:"if"`
	Run             string         `yaml:"run"`
	With            map[string]any `yaml:"with"`
	Env             map[string]any `yaml:"env"`
	ContinueOnError yaml.Node      `yaml:"continue-on-error"`
}

type quickstartWorkflowJob struct {
	Name            string                   `yaml:"name"`
	Needs           yaml.Node                `yaml:"needs"`
	If              string                   `yaml:"if"`
	Permissions     map[string]string        `yaml:"permissions"`
	TimeoutMinutes  int                      `yaml:"timeout-minutes"`
	Strategy        yaml.Node                `yaml:"strategy"`
	ContinueOnError yaml.Node                `yaml:"continue-on-error"`
	Steps           []quickstartWorkflowStep `yaml:"steps"`
}

type quickstartWorkflowDoc struct {
	On          yaml.Node         `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]quickstartWorkflowJob `yaml:"jobs"`
}

func readQuickstartWorkflow(t *testing.T) (quickstartWorkflowDoc, string) {
	t.Helper()

	src, err := os.ReadFile(quickstartWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartWorkflowPath, err)
	}
	var doc quickstartWorkflowDoc
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("parse %s: %v", quickstartWorkflowPath, err)
	}
	return doc, string(src)
}

func quickstartMappingValue(node yaml.Node, key string) yaml.Node {
	if node.Kind != yaml.MappingNode {
		return yaml.Node{}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return *node.Content[i+1]
		}
	}
	return yaml.Node{}
}

func quickstartScalarList(node yaml.Node) []string {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value != "" {
			return []string{node.Value}
		}
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			out = append(out, item.Value)
		}
		return out
	}
	return nil
}

func quickstartValue(values map[string]any, key string) string {
	return strings.TrimSpace(toString(values[key]))
}

func quickstartMode(step quickstartWorkflowStep) string {
	return quickstartValue(step.Env, "MODE")
}

func quickstartStepByMode(t *testing.T, jobID string, job quickstartWorkflowJob, mode string) (int, quickstartWorkflowStep) {
	t.Helper()

	found := -1
	var step quickstartWorkflowStep
	for i, candidate := range job.Steps {
		if quickstartMode(candidate) != mode {
			continue
		}
		if found >= 0 {
			t.Fatalf("%s job %q has more than one MODE=%s step", quickstartWorkflowPath, jobID, mode)
		}
		found, step = i, candidate
	}
	if found < 0 {
		t.Fatalf("%s job %q has no MODE=%s step", quickstartWorkflowPath, jobID, mode)
	}
	return found, step
}

func quickstartStepByName(t *testing.T, jobID string, job quickstartWorkflowJob, fragment string) (int, quickstartWorkflowStep) {
	t.Helper()

	for i, step := range job.Steps {
		if strings.Contains(strings.ToLower(step.Name), strings.ToLower(fragment)) {
			return i, step
		}
	}
	t.Fatalf("%s job %q has no step whose name contains %q", quickstartWorkflowPath, jobID, fragment)
	return -1, quickstartWorkflowStep{}
}

func quickstartFence(t *testing.T) []string {
	t.Helper()

	src, err := os.ReadFile(quickstartReadmePath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartReadmePath, err)
	}
	lines := strings.Split(string(src), "\n")
	section := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Quick start" {
			section = i + 1
			break
		}
	}
	if section < 0 {
		t.Fatalf("%s has no Quick start section", quickstartReadmePath)
	}

	for i := section; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "## ") {
			break
		}
		if line != "```sh" && line != "```bash" {
			continue
		}
		var commands []string
		for _, body := range lines[i+1:] {
			if strings.TrimSpace(body) == "```" {
				if len(commands) == 0 {
					t.Fatalf("%s Quick start shell fence is empty", quickstartReadmePath)
				}
				return commands
			}
			body, _, _ = strings.Cut(body, "#")
			if command := strings.TrimSpace(body); command != "" {
				commands = append(commands, command)
			}
		}
		t.Fatalf("%s Quick start shell fence is not closed", quickstartReadmePath)
	}
	t.Fatalf("%s Quick start section has no shell fence", quickstartReadmePath)
	return nil
}

func TestQuickstartCanaryBindsThePublishedRootCommand(t *testing.T) {
	t.Parallel()

	commands := quickstartFence(t)
	clone := regexp.MustCompile(`^git clone https://github\.com/[^/[:space:]]+/cerberus\.git && cd cerberus$`)
	if len(commands) != 3 || !clone.MatchString(commands[0]) {
		t.Fatalf("%s Quick start must begin with one repository clone-and-cd command; parsed commands: %q",
			quickstartReadmePath, commands)
	}
	if got, want := commands[1], "docker compose up --wait"; got != want {
		t.Fatalf("%s Quick start starts the root stack with %q, want exact command %q",
			quickstartReadmePath, got, want)
	}
	if got, want := commands[2], "open http://localhost:3000"; got != want {
		t.Fatalf("%s Quick start opens %q, want %q", quickstartReadmePath, got, want)
	}

	script, err := os.ReadFile(quickstartScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartScriptPath, err)
	}
	commandLiteral := regexp.MustCompile(`(?s)QUICKSTART_UP_COMMAND\s*=\s*Object\.freeze\(\[\s*["']docker["'],\s*["']compose["'],\s*["']up["'],\s*["']--wait["']\s*,?\s*\]\)`)
	if !commandLiteral.Match(script) {
		t.Fatalf("%s must declare QUICKSTART_UP_COMMAND as the exact argv represented by README's %q command",
			quickstartScriptPath, commands[1])
	}
	wrapperBinding := regexp.MustCompile(`(?s)command:\s*process\.execPath,\s*args:\s*Object\.freeze\(\[\s*join\(repoRoot,\s*REGISTRY_RETRY_WRAPPER\),\s*\.\.\.QUICKSTART_UP_COMMAND\s*,?\s*\]\)`)
	if !wrapperBinding.Match(script) {
		t.Fatalf("%s must pass QUICKSTART_UP_COMMAND unchanged as the registry-retry wrapper's trailing argv",
			quickstartScriptPath)
	}
	modeUpBinding := regexp.MustCompile(`(?s)const invocation = quickstartUpInvocation\(root, timeout\);.*spawnSync\(invocation\.command, invocation\.args, \{\s*cwd: invocation\.cwd`)
	if !modeUpBinding.Match(script) {
		t.Fatalf("%s declares the published command wrapper but MODE=up does not spawn that invocation at its bound root",
			quickstartScriptPath)
	}
}

func TestQuickstartCanaryRunsOnTheProjectedCommit(t *testing.T) {
	t.Parallel()

	doc, _ := readQuickstartWorkflow(t)
	for _, event := range []string{"pull_request", "merge_group", "push"} {
		if !onDeclares(doc.On, event) {
			t.Errorf("%s does not declare the %s trigger", quickstartWorkflowPath, event)
		}
	}
	pushBranches := quickstartScalarList(quickstartMappingValue(quickstartMappingValue(doc.On, "push"), "branches"))
	for _, branch := range []string{"main", maintenanceBranchPattern} {
		if !slices.Contains(pushBranches, branch) {
			t.Errorf("%s push trigger omits %q (branches: %v)", quickstartWorkflowPath, branch, pushBranches)
		}
	}
	if got, want := doc.Concurrency.CancelInProgress, "${{ github.event_name == 'pull_request' }}"; got != want {
		t.Errorf("%s cancel-in-progress is %q, want %q so merge-group evidence is never cancelled",
			quickstartWorkflowPath, got, want)
	}

	for _, jobID := range []string{"select", "run", "quickstart"} {
		job, ok := doc.Jobs[jobID]
		if !ok {
			t.Fatalf("%s has no %q job", quickstartWorkflowPath, jobID)
		}
		if len(job.Steps) == 0 || job.Steps[0].Uses != "actions/checkout@v7" {
			t.Errorf("%s job %q does not begin with actions/checkout@v7", quickstartWorkflowPath, jobID)
			continue
		}
		checkout := job.Steps[0]
		for key, want := range map[string]string{
			"ref":         projectedSHAExpression,
			"fetch-depth": "0",
			"clean":       "true",
		} {
			if got := quickstartValue(checkout.With, key); got != want {
				t.Errorf("%s job %q checkout %s is %q, want %q", quickstartWorkflowPath, jobID, key, got, want)
			}
		}
	}

	selectJob := doc.Jobs["select"]
	_, selectStep := quickstartStepByMode(t, "select", selectJob, "select")
	for key, want := range map[string]string{
		"EVENT_NAME":   "${{ github.event_name }}",
		"HEAD_SHA":     projectedSHAExpression,
		"CHECKOUT_SHA": projectedSHAExpression,
	} {
		if got := quickstartValue(selectStep.Env, key); got != want {
			t.Errorf("%s select step %s is %q, want %q", quickstartWorkflowPath, key, got, want)
		}
	}
	base := quickstartValue(selectStep.Env, "BASE_SHA")
	for _, required := range []string{
		"github.event.pull_request.base.sha",
		"github.event.merge_group.base_sha",
		"github.event.before",
	} {
		if !strings.Contains(base, required) {
			t.Errorf("%s select step BASE_SHA %q omits %q", quickstartWorkflowPath, base, required)
		}
	}

	_, verify := quickstartStepByMode(t, "run", doc.Jobs["run"], "verify-checkout")
	if got := quickstartValue(verify.Env, "EXPECTED_SHA"); got != projectedSHAExpression {
		t.Errorf("%s verify-checkout expects %q, want exact projected SHA %q",
			quickstartWorkflowPath, got, projectedSHAExpression)
	}
}

func TestQuickstartCanaryIsOneBoundedStackWithoutBrowserDepth(t *testing.T) {
	t.Parallel()

	doc, src := readQuickstartWorkflow(t)
	if strings.Contains(strings.ToLower(src), "playwright") || regexp.MustCompile(`(?m)\bnpx\b`).MatchString(src) {
		t.Fatalf("%s includes browser-depth machinery; the required canary must boot one stack and finish quickly",
			quickstartWorkflowPath)
	}
	if strings.Contains(src, "|| true") {
		t.Fatalf("%s masks a shell failure with `|| true`; every setup, diagnostic, and teardown failure must stay red",
			quickstartWorkflowPath)
	}
	for jobID, job := range doc.Jobs {
		if !job.Strategy.IsZero() {
			t.Errorf("%s job %q declares a strategy/matrix; the quickstart canary must boot one stack",
				quickstartWorkflowPath, jobID)
		}
		if !job.ContinueOnError.IsZero() {
			t.Errorf("%s job %q uses continue-on-error, which can mask its verdict", quickstartWorkflowPath, jobID)
		}
		for _, step := range job.Steps {
			if !step.ContinueOnError.IsZero() {
				t.Errorf("%s job %q step %q uses continue-on-error, which can mask its verdict",
					quickstartWorkflowPath, jobID, step.Name)
			}
		}
	}

	runJob := doc.Jobs["run"]
	if got, want := runJob.TimeoutMinutes, quickstartRunTimeout; got != want {
		t.Errorf("%s run timeout is %d minutes, want %d", quickstartWorkflowPath, got, want)
	}
	if got := quickstartScalarList(runJob.Needs); !slices.Equal(got, []string{"select"}) {
		t.Errorf("%s run job needs %v, want [select]", quickstartWorkflowPath, got)
	}
	for _, required := range []string{"needs.select.result == 'success'", "needs.select.outputs.selected == 'true'"} {
		if !strings.Contains(runJob.If, required) {
			t.Errorf("%s run condition %q omits %q", quickstartWorkflowPath, runJob.If, required)
		}
	}

	upIndex, up := quickstartStepByMode(t, "run", runJob, "up")
	if got, want := strings.TrimSpace(up.Run), "node .github/scripts/quickstart-canary.mjs"; got != want {
		t.Errorf("%s MODE=up runs %q, want %q", quickstartWorkflowPath, got, want)
	}
	pullIndex, pull := quickstartStepByName(t, "run", runJob, "pre-pull")
	if got, want := strings.TrimSpace(pull.Run), "node .github/scripts/compose-pull-images.mjs docker-compose.yml"; got != want {
		t.Errorf("%s pre-pull command is %q, want root Compose model command %q",
			quickstartWorkflowPath, got, want)
	}
	if pullIndex >= upIndex {
		t.Errorf("%s pre-pulls Compose images after MODE=up (steps %d and %d)", quickstartWorkflowPath, pullIndex, upIndex)
	}
	if got := strings.Count(src, "MODE: up"); got != 1 {
		t.Errorf("%s contains %d MODE=up steps, want exactly one root-stack boot", quickstartWorkflowPath, got)
	}
	if got, want := doc.Permissions["contents"], "read"; got != want {
		t.Errorf("%s contents permission is %q, want %q", quickstartWorkflowPath, got, want)
	}
	if _, present := doc.Permissions["packages"]; present {
		t.Errorf("%s grants packages permission workflow-wide; only the stack job needs it",
			quickstartWorkflowPath)
	}
	for key, want := range map[string]string{"contents": "read", "packages": "read"} {
		if got := runJob.Permissions[key]; got != want {
			t.Errorf("%s run job %s permission is %q, want %q",
				quickstartWorkflowPath, key, got, want)
		}
	}
	for _, jobID := range []string{"select", "quickstart"} {
		if _, present := doc.Jobs[jobID].Permissions["packages"]; present {
			t.Errorf("%s job %q grants packages permission but never reads packages",
				quickstartWorkflowPath, jobID)
		}
	}
}

func TestQuickstartCanaryProbeOutlivesTheComposeSeederWait(t *testing.T) {
	t.Parallel()

	seed, err := os.ReadFile(quickstartSeedPath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartSeedPath, err)
	}
	script, err := os.ReadFile(quickstartScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartScriptPath, err)
	}
	seedMatch := regexp.MustCompile(`tableWaitTimeout\s*=\s*(\d+)\s*\*\s*time\.Minute`).FindSubmatch(seed)
	probeMatch := regexp.MustCompile(`COMPOSE_SEED_TABLE_WAIT_MS\s*=\s*(\d+)\s*\*\s*60\s*\*\s*1000`).FindSubmatch(script)
	marginMatch := regexp.MustCompile(`PROBE_SEED_READINESS_MARGIN_MS\s*=\s*(\d+)\s*\*\s*60\s*\*\s*1000`).FindSubmatch(script)
	if len(seedMatch) != 2 || len(probeMatch) != 2 || len(marginMatch) != 2 {
		t.Fatalf("seeder wait and quickstart probe margin must remain explicit minute constants")
	}
	seedMinutes, err := strconv.Atoi(string(seedMatch[1]))
	if err != nil {
		t.Fatalf("parse seeder table wait: %v", err)
	}
	probeSeedMinutes, err := strconv.Atoi(string(probeMatch[1]))
	if err != nil {
		t.Fatalf("parse quickstart's seeder wait: %v", err)
	}
	marginMinutes, err := strconv.Atoi(string(marginMatch[1]))
	if err != nil {
		t.Fatalf("parse quickstart's readiness margin: %v", err)
	}
	if probeSeedMinutes != seedMinutes {
		t.Errorf("%s models the seeder wait as %d minutes, but %s waits %d minutes",
			quickstartScriptPath, probeSeedMinutes, quickstartSeedPath, seedMinutes)
	}
	if marginMinutes <= 0 {
		t.Errorf("%s gives the functional probe no positive margin after the seeder wait",
			quickstartScriptPath)
	}
}

func TestQuickstartCanaryProbesThenAlwaysTearsDownAndReports(t *testing.T) {
	t.Parallel()

	doc, _ := readQuickstartWorkflow(t)
	runJob := doc.Jobs["run"]
	upIndex, _ := quickstartStepByMode(t, "run", runJob, "up")
	probeIndex, probe := quickstartStepByMode(t, "run", runJob, "probe")
	for key, want := range map[string]string{
		"CERBERUS_URL": "http://localhost:8080",
		"GRAFANA_URL":  "http://localhost:3000",
	} {
		if got := quickstartValue(probe.Env, key); got != want {
			t.Errorf("%s probe %s is %q, want README root-stack endpoint %q",
				quickstartWorkflowPath, key, got, want)
		}
	}
	script, err := os.ReadFile(quickstartScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", quickstartScriptPath, err)
	}
	for _, endpoint := range []string{
		"/healthz",
		"/readyz",
		"/api/health",
		"/api/datasources/uid/",
		"/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query",
		"/api/datasources/proxy/uid/cerberus-loki/loki/api/v1/query",
		"/api/datasources/proxy/uid/cerberus-tempo/api/search",
		"/api/dashboards/home",
	} {
		if !strings.Contains(string(script), endpoint) {
			t.Errorf("%s MODE=probe does not bind the required %s endpoint", quickstartScriptPath, endpoint)
		}
	}
	grafanaRootProbe := regexp.MustCompile(`id:\s*["']grafana-root["']`)
	if !grafanaRootProbe.Match(script) {
		t.Errorf("%s MODE=probe does not fetch the user-visible Grafana root", quickstartScriptPath)
	}
	diagnosticsIndex, diagnostics := quickstartStepByName(t, "run", runJob, "diagnostics")
	if !strings.Contains(diagnostics.If, "failure()") ||
		!strings.Contains(diagnostics.Run, "docker compose ps") ||
		!strings.Contains(diagnostics.Run, "docker compose logs") {
		t.Errorf("%s diagnostics step must capture Compose state and logs on failure", quickstartWorkflowPath)
	}
	teardownIndex, teardown := quickstartStepByName(t, "run", runJob, "tear down")
	if got, want := strings.TrimSpace(teardown.If), "always()"; got != want {
		t.Errorf("%s teardown condition is %q, want %q", quickstartWorkflowPath, got, want)
	}
	if got, want := strings.TrimSpace(teardown.Run), "docker compose down -v --remove-orphans"; got != want {
		t.Errorf("%s teardown command is %q, want %q", quickstartWorkflowPath, got, want)
	}
	if upIndex >= probeIndex || probeIndex >= diagnosticsIndex || diagnosticsIndex >= teardownIndex {
		t.Errorf("%s step order is up=%d probe=%d diagnostics=%d teardown=%d; teardown must remain last",
			quickstartWorkflowPath, upIndex, probeIndex, diagnosticsIndex, teardownIndex)
	}

	aggregate, ok := doc.Jobs["quickstart"]
	if !ok {
		t.Fatalf("%s has no stable quickstart aggregate job", quickstartWorkflowPath)
	}
	if aggregate.Name != "quickstart" || strings.TrimSpace(aggregate.If) != "always()" {
		t.Errorf("%s aggregate name/condition are %q/%q, want quickstart/always()",
			quickstartWorkflowPath, aggregate.Name, aggregate.If)
	}
	if got := quickstartScalarList(aggregate.Needs); !slices.Equal(got, []string{"select", "run"}) {
		t.Errorf("%s aggregate needs %v, want [select run]", quickstartWorkflowPath, got)
	}
	_, aggregateStep := quickstartStepByMode(t, "quickstart", aggregate, "aggregate")
	for key, want := range map[string]string{
		"SELECT_RESULT": "${{ needs.select.result }}",
		"SELECTED":      "${{ needs.select.outputs.selected }}",
		"RUN_RESULT":    "${{ needs.run.result }}",
	} {
		if got := quickstartValue(aggregateStep.Env, key); got != want {
			t.Errorf("%s aggregate %s is %q, want %q", quickstartWorkflowPath, key, got, want)
		}
	}
}
