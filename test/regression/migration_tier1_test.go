package regression

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
	"github.com/tsouza/cerberus/test/e2e/migration/steps"
)

// The Tier-1 migration substrate (Layer 14) is a Docker stack, so the
// assertions that actually exercise it are build-tagged `migration_tier1` and
// need a running cluster. The pins below are the half that does NOT: pure file
// reads over the compose file, its sibling configs, the seeder package, and
// the Justfile, so a change that breaks the substrate's contract fails the
// required `check` lane instead of surfacing hours later in a scheduled lane.
const (
	tier1Dir             = "../../test/e2e/migration/tiers/tier1-dual"
	tier1ComposePath     = tier1Dir + "/docker-compose.dual.yml"
	tier1PrometheusPath  = tier1Dir + "/prometheus.yml"
	tier1CollectorPath   = tier1Dir + "/otel-collector-config.yaml"
	tier1SeedPkgDir      = "../../test/e2e/migration/seed"
	tier1ComposeProject  = "cerberus-migration-tier1"
	tier1CerberusService = "cerberus"
)

// The published-port band tier-1 owns. Every other compose stack in the tree
// sits outside it; reusing one of their ports would silently cross-wire a
// tier-1 run onto a leftover container from another stack.
const (
	tier1PortBandLow  = 27000
	tier1PortBandHigh = 27400
)

// otherComposeFiles are every other compose stack in the tree. Their published
// host ports must not intersect tier-1's.
var otherComposeFiles = []string{
	"../../docker-compose.yml",
	"../../compatibility/prometheus/docker-compose.yml",
	"../../compatibility/loki/docker-compose.yml",
	"../../compatibility/tempo/docker-compose.yml",
}

// composeService is the subset of a compose service definition these pins read.
type composeService struct {
	Image       string            `yaml:"image"`
	PullPolicy  string            `yaml:"pull_policy"`
	Command     []string          `yaml:"command"`
	Environment map[string]string `yaml:"environment"`
	Volumes     []string          `yaml:"volumes"`
	Ports       []string          `yaml:"ports"`
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

func readCompose(t *testing.T, path string) composeFile {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out composeFile
	if err := yaml.Unmarshal(buf, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// hostPorts extracts the host side of every published `"HOST:CONTAINER"` port
// mapping in a compose file.
func hostPorts(t *testing.T, path string) map[int]string {
	t.Helper()
	cf := readCompose(t, path)
	out := map[int]string{}
	for name, svc := range cf.Services {
		for _, p := range svc.Ports {
			host, _, ok := strings.Cut(p, ":")
			if !ok {
				t.Fatalf("%s: service %s publishes %q, which is not a HOST:CONTAINER mapping", path, name, p)
			}
			n, err := strconv.Atoi(host)
			if err != nil {
				t.Fatalf("%s: service %s publishes %q with a non-numeric host port: %v", path, name, p, err)
			}
			out[n] = name
		}
	}
	return out
}

// TestMigrationTier1PortBand pins tier-1's published ports inside its own band
// and disjoint from every other compose stack. A collision does not fail
// loudly: the second stack's container simply fails to bind, or worse, a test
// talks to the wrong backend and reports parity against a stale dataset.
func TestMigrationTier1PortBand(t *testing.T) {
	t.Parallel()

	tier1 := hostPorts(t, tier1ComposePath)
	if len(tier1) == 0 {
		t.Fatalf("%s publishes no host ports; the substrate is unreachable from the test host", tier1ComposePath)
	}
	for port, svc := range tier1 {
		if port < tier1PortBandLow || port > tier1PortBandHigh {
			t.Fatalf("tier-1 service %s publishes host port %d, outside the declared %d-%d band. "+
				"The band is what keeps a tier-1 run from cross-wiring onto another stack's leftovers.",
				svc, port, tier1PortBandLow, tier1PortBandHigh)
		}
	}

	for _, other := range otherComposeFiles {
		for port, svc := range hostPorts(t, other) {
			if tier1Svc, clash := tier1[port]; clash {
				t.Fatalf("host port %d is published by BOTH tier-1 service %s and %s service %s. "+
					"Move the tier-1 mapping to a free port inside the %d-%d band.",
					port, tier1Svc, other, svc, tier1PortBandLow, tier1PortBandHigh)
			}
		}
	}
}

// TestMigrationTier1SharedConfigsResolve pins the single-sourcing of the
// reference Loki / Tempo configs: tier-1 bind-mounts the ONE copy of each in
// the tree rather than holding a second definition that could drift. A mount
// source that stops resolving does not fail the stack loudly — Docker creates
// an empty directory and the reference backend boots on built-in defaults,
// which is a silent parity change.
func TestMigrationTier1SharedConfigsResolve(t *testing.T) {
	t.Parallel()

	wantShared := map[string]string{
		"loki":  "compatibility/loki/loki-config.yaml",
		"tempo": "compatibility/tempo/tempo-config.yaml",
	}

	cf := readCompose(t, tier1ComposePath)
	for svc, wantSuffix := range wantShared {
		def, ok := cf.Services[svc]
		if !ok {
			t.Fatalf("%s has no %q service", tier1ComposePath, svc)
		}
		var mounted bool
		for _, v := range def.Volumes {
			src, _, ok := strings.Cut(v, ":")
			if !ok {
				t.Fatalf("service %s mounts %q, which is not a SOURCE:TARGET bind", svc, v)
			}
			resolved := filepath.Join(tier1Dir, src)
			if _, err := os.Stat(resolved); err != nil {
				t.Fatalf("service %s bind-mounts %q, which does not resolve (%s): %v", svc, src, resolved, err)
			}
			if strings.HasSuffix(filepath.ToSlash(filepath.Clean(resolved)), wantSuffix) {
				mounted = true
			}
		}
		if !mounted {
			t.Fatalf("service %s does not mount %s. The tree holds exactly one reference config per "+
				"signal; a copy here would drift from the compatibility harness's.", svc, wantSuffix)
		}
	}
}

// TestMigrationTier1SchemaAuthority pins the split that makes cerberus's
// readiness probe a live schema-drift detector: the collector's
// clickhouseexporter creates the tables, cerberus never does. Flipping
// cerberus's auto-create on would mint cerberus-named tables and mask a real
// divergence between the exporter's names and cerberus's read-side defaults.
func TestMigrationTier1SchemaAuthority(t *testing.T) {
	t.Parallel()

	cf := readCompose(t, tier1ComposePath)
	if cf.Name != tier1ComposeProject {
		t.Fatalf("compose project name = %q, want %q (the Justfile recipes and any leftover-container "+
			"cleanup key on it)", cf.Name, tier1ComposeProject)
	}
	cerb, ok := cf.Services[tier1CerberusService]
	if !ok {
		t.Fatalf("%s has no %q service", tier1ComposePath, tier1CerberusService)
	}
	if got := cerb.Environment["CERBERUS_AUTO_CREATE_SCHEMA"]; got != "false" {
		t.Fatalf("cerberus CERBERUS_AUTO_CREATE_SCHEMA = %q, want %q. The collector is the sole schema "+
			"authority in this stack; cerberus creating its own tables would mask exporter/read-side "+
			"name drift instead of holding /readyz at 503.", got, "false")
	}

	collector, err := os.ReadFile(tier1CollectorPath)
	if err != nil {
		t.Fatalf("read %s: %v", tier1CollectorPath, err)
	}
	if !regexp.MustCompile(`(?m)^\s*create_schema:\s*true\s*$`).Match(collector) {
		t.Fatalf("%s does not set `create_schema: true`; nothing in the stack would create the OTel "+
			"tables and cerberus would never report ready.", tier1CollectorPath)
	}

	// The seeder writes fixture rows; it must never create a table. A DDL path
	// there would re-mask the same drift from the other side. The scan reads
	// the parsed AST — imports and string literals — so it pins BEHAVIOUR: an
	// error message that honestly mentions table creation is untouched.
	entries, err := os.ReadDir(tier1SeedPkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", tier1SeedPkgDir, err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(tier1SeedPkgDir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "schema/ddl") {
				t.Fatalf("%s imports %s. The collector's clickhouseexporter is the sole schema "+
					"authority; the seeder waits for its tables instead of minting its own.",
					path, imp.Path.Value)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, ddl := range []string{"CREATE TABLE", "CREATE DATABASE", "CREATE VIEW"} {
				if strings.Contains(lit.Value, ddl) {
					t.Fatalf("%s holds a %s statement. The collector's clickhouseexporter is the "+
						"sole schema authority; the seeder waits for its tables instead.", path, ddl)
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatalf("%s holds no Go files; the seeder's write clients are gone", tier1SeedPkgDir)
	}
}

// tier1SelfScrapeJob is MIG-07's one deliberate exception to the reference
// Prometheus being otherwise remote-write only: a real scrape target both the
// reference AND the collector's own prometheusreceiver pull from, so MIG-07
// can prove the collector's scrape path is equivalent. It names Prometheus's
// own process metrics (`up`, `scrape_duration_seconds`,
// `prometheus_http_request_duration_seconds_bucket`, …) — a namespace the
// seeder's fixture never writes a sample under — so it cannot inject a second
// sample path for any fixture series. Mirrors then_scrape_parity.go's own
// scrapeJob constant.
const tier1SelfScrapeJob = "prometheus-self"

// TestMigrationTier1ReferencePrometheusIsWriteOnly pins the reference
// Prometheus to a single sample path for FIXTURE data. A scrape config for
// any target other than tier1SelfScrapeJob would give the reference a second,
// independent source of fixture samples, and two paths cannot land identical
// timestamps — while the migration comparator keys samples by exact
// timestamp, so every such scraped sample would read as a divergence.
func TestMigrationTier1ReferencePrometheusIsWriteOnly(t *testing.T) {
	t.Parallel()

	promCfg, err := os.ReadFile(tier1PrometheusPath)
	if err != nil {
		t.Fatalf("read %s: %v", tier1PrometheusPath, err)
	}
	var parsed struct {
		ScrapeConfigs []struct {
			JobName string `yaml:"job_name"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(promCfg, &parsed); err != nil {
		t.Fatalf("parse %s: %v", tier1PrometheusPath, err)
	}
	for _, sc := range parsed.ScrapeConfigs {
		if sc.JobName != tier1SelfScrapeJob {
			t.Fatalf("%s declares scrape job %q. The reference side is remote-write only for fixture data: "+
				"only the %q self-scrape job is allowed, because any other scrape is a second sample path and "+
				"the comparator keys samples by exact timestamp.", tier1PrometheusPath, sc.JobName, tier1SelfScrapeJob)
		}
	}

	cf := readCompose(t, tier1ComposePath)
	prom, ok := cf.Services["prometheus"]
	if !ok {
		t.Fatalf("%s has no %q service", tier1ComposePath, "prometheus")
	}
	const receiverFlag = "--web.enable-remote-write-receiver"
	var enabled bool
	for _, arg := range prom.Command {
		if strings.TrimSpace(arg) == receiverFlag {
			enabled = true
		}
	}
	if !enabled {
		t.Fatalf("reference prometheus is started without %s, so every seeded sample would be rejected "+
			"and the reference side of the diff would be empty. Command: %v", receiverFlag, prom.Command)
	}
}

// TestMigrationTier1ArchetypeIdentitiesAreDisjoint is the invariant that lets
// every tier-1 archetype share ONE fixture window on ONE live stack.
//
// The seeder writes each archetype into the same reference backends over the
// same interval, so nothing but the names keeps them apart. Reference Tempo's
// /api/search is per-service and its settle gate demands EXACTLY the trace
// count this run pushed, so two archetypes declaring the same service name
// make the second seed fail — which is precisely how the previous mechanism
// broke: three-signal and kube-prometheus-stack both declared "checkout".
//
// The alternative — pushing each archetype into its own earlier window via
// cmd/seed's --window-offset — cannot work for more than one archetype:
// disjoint windows need an offset >= seed.SeedWindow, while reference Tempo's
// ingestion slack rejects offset+SeedWindow >= 1h. No value satisfies both.
// Disjoint identities have no such ceiling, so this pin is what replaces it.
//
// It reads the archetype list out of the Justfile rather than restating it, so
// a ninth archetype is covered the moment it is seeded.
func TestMigrationTier1ArchetypeIdentitiesAreDisjoint(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	archetypes := justVariableList(t, string(buf), "MIGRATION_TIER1_ARCHETYPES")
	if len(archetypes) < 2 {
		t.Fatalf("MIGRATION_TIER1_ARCHETYPES lists %d archetype(s) (%v); with fewer than two "+
			"sharing a stack this invariant would assert nothing", len(archetypes), archetypes)
	}

	// owner maps each declared identity to the archetype that claimed it, so a
	// collision names BOTH sides instead of only the second one.
	owner := map[string]string{}
	for _, a := range archetypes {
		path := filepath.Join("..", "..", "test", "e2e", "migration", "archetypes", a, "seed", "fixture.json")
		decl, err := seed.LoadDeclaration(path)
		if err != nil {
			t.Fatalf("archetype %s is seeded by the tier-1 lane but its data declaration does not load: %v", a, err)
		}
		// Every identity that reaches a shared backend as a queryable name.
		// The kind prefix keeps a service named like a metric from reading as
		// a collision when the two could never be searched the same way.
		identities := []string{
			"log-job/" + decl.LogJob,
			"metric/" + decl.GaugeMetric,
			"metric/" + decl.CounterMetric,
			"metric/" + decl.HistogramMetric,
		}
		for _, svc := range decl.Services {
			identities = append(identities, "service/"+svc)
		}
		for _, id := range identities {
			if prev, taken := owner[id]; taken {
				t.Fatalf("archetypes %s and %s both declare %s; they are seeded into one live stack over one "+
					"window, so the shared name makes each one's queries see the other's data "+
					"(reference Tempo's per-service settle gate fails outright). Rename it in "+
					"test/e2e/migration/archetypes/%s/seed/fixture.json", prev, a, id, a)
			}
			owner[id] = a
		}
	}
}

// justVariableList reads a `NAME := "a b c"` Justfile assignment as a slice.
func justVariableList(t *testing.T, justfile, name string) []string {
	t.Helper()

	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(justfile)
	if m == nil {
		t.Fatalf("Justfile has no %s assignment; the tier-1 seed recipe reads its archetype list from it", name)
	}
	return strings.Fields(m[1])
}

// TestMigrationTier1JustfileRecipes pins the recipe shape the lane is driven
// through: a `-run` recipe that actually passes the build tag (without it the
// tagged assertions compile away to "no test files" and the lane reports green
// having asserted nothing), plus up/down recipes pointed at the compose file,
// with `down -v` so no volume survives into the next run.
func TestMigrationTier1JustfileRecipes(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	body := string(buf)

	const composeRef = "test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml"
	for _, tc := range []struct {
		recipe string
		wants  []string
	}{
		// The up recipe tears the stack down first. A run aborted by a failing
		// assertion never reaches the composite's trailing teardown, so without
		// this the next run seeds a stack that is already seeded: inside the
		// seed window the two fixture generations collide on every sample
		// instant and remote-write rejects the push; past it, ClickHouse
		// silently accumulates both.
		{recipe: "migration-tier1-up", wants: []string{composeRef, "up ", "--wait", "just migration-tier1-down"}},
		{recipe: "migration-tier1-run", wants: []string{"-tags=migration_tier1", "./test/e2e/migration/"}},
		{recipe: "migration-tier1-down", wants: []string{composeRef, "down -v"}},
	} {
		recipeBody := justRecipeBody(t, body, tc.recipe)
		for _, want := range tc.wants {
			if !strings.Contains(recipeBody, want) {
				t.Fatalf("Justfile recipe %s does not contain %q; body:\n%s", tc.recipe, want, recipeBody)
			}
		}
	}

	// The composite must carry the seed step. Without it the assertions run
	// against an empty stack, where "both sides agree" is vacuously true.
	if !strings.Contains(body, tier1Composite) {
		t.Fatalf("Justfile has no `migration-tier1` composite chaining up -> seed -> run -> down; want %q",
			tier1Composite)
	}
}

// tier1Composite is the lane's full lifecycle, in order.
const tier1Composite = "migration-tier1: migration-tier1-up migration-tier1-seed migration-tier1-run migration-tier1-down"

// tier1WorkflowPath is the workflow whose migration-tier1 job executes the
// tagged assertions. It used to be its own migration.yml; the Tier-1 lane
// was folded into migration-e2e.yml as a sibling of migration-tier0 so the
// dual-backend compose stack has exactly one lifecycle per run.
const tier1WorkflowPath = "../../.github/workflows/migration-e2e.yml"

// tier1JobName is the job inside tier1WorkflowPath that drives the Tier-1
// lane.
const tier1JobName = "migration-tier1"

// TestMigrationTier1LaneIsWiredIntoCI pins the two things that keep the
// `migration_tier1`-tagged sources from rotting unobserved.
//
// Those files carry every value assertion the Tier-1 substrate makes, and a
// build tag no lane names is a file no lane compiles: a rename in an untagged
// package breaks them while all eleven required checks stay green, and the docs
// go on citing them as proof. Two independent wirings close that:
//
//   - the required `check` lane type-checks them under the tag, so a break is
//     caught at merge time,
//   - a scheduled workflow actually runs the lane, so the substrate itself is
//     exercised rather than only parsed.
//
// Both are pinned from pure file reads, which is what lets this run inside
// `check` alongside the thing it is guarding.
func TestMigrationTier1LaneIsWiredIntoCI(t *testing.T) {
	t.Parallel()

	justfile, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	testRecipe := justRecipeBody(t, string(justfile), "test")
	for _, want := range []string{"-tags=migration_tier1", "./test/e2e/migration/"} {
		if !strings.Contains(testRecipe, want) {
			t.Fatalf("the `test` recipe does not type-check the migration_tier1 lane (missing %q); "+
				"without it the tagged assertion files compile in no CI job. Body:\n%s", want, testRecipe)
		}
	}

	workflow, err := os.ReadFile(tier1WorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v — the tagged Tier-1 assertions are executed by no workflow", tier1WorkflowPath, err)
	}
	body := string(workflow)

	var parsed struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
			Schedule []struct {
				Cron string `yaml:"cron"`
			} `yaml:"schedule"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(workflow, &parsed); err != nil {
		t.Fatalf("parse %s: %v", tier1WorkflowPath, err)
	}
	onMain := false
	for _, b := range parsed.On.Push.Branches {
		if b == "main" {
			onMain = true
		}
	}
	if !onMain {
		t.Fatalf("%s does not push-trigger on main; the Tier-1 lane docs call push-to-main", tier1WorkflowPath)
	}
	if len(parsed.On.Schedule) == 0 {
		t.Fatalf("%s declares no schedule; the lane the docs call scheduled would run only on dispatch",
			tier1WorkflowPath)
	}
	for _, c := range parsed.On.Schedule {
		if strings.TrimSpace(c.Cron) == "" {
			t.Fatalf("%s declares an empty cron expression", tier1WorkflowPath)
		}
	}
	if !strings.Contains(body, "\n  workflow_dispatch:\n") {
		t.Fatalf("%s declares no workflow_dispatch trigger", tier1WorkflowPath)
	}

	job := workflowJobBody(t, body, tier1JobName)
	if !strings.Contains(job, "needs: migration-setup") {
		t.Fatalf("%s job %q does not declare `needs: migration-setup`", tier1WorkflowPath, tier1JobName)
	}
	for _, want := range []string{
		"just migration-tier1-up",
		"just migration-tier1-seed",
		"-tags=migration_tier1",
		"migration-e2e.mjs",
		"TIER: tier1",
		"just migration-tier1-down",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("%s job %q does not %q, so the Tier-1 lane is incompletely wired. Body:\n%s",
				tier1WorkflowPath, tier1JobName, want, job)
		}
	}
}

// workflowJobBody returns the lines of a workflow job: everything from the
// `  <job>:` header up to (not including) the next line at 2-space indent —
// mirroring justRecipeBody's Justfile-recipe extraction, one indent level in.
func workflowJobBody(t *testing.T, workflow, job string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	header := "  " + job + ":"
	start := -1
	for i, line := range lines {
		if line == header {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("workflow has no %q job", job)
	}
	var out []string
	for _, line := range lines[start:] {
		if len(out) > 0 && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "   ") {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// justRecipeBody returns the lines of a Justfile recipe: everything from its
// `name:` header up to the next non-indented, non-blank line. A recipe with
// parameters (`recipe param="default":`) has a space, not a colon, right
// after its name, so both header shapes are matched.
func justRecipeBody(t *testing.T, justfile, recipe string) string {
	t.Helper()
	lines := strings.Split(justfile, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, recipe+":") || strings.HasPrefix(line, recipe+" ") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("Justfile has no %q recipe", recipe)
	}
	var out []string
	for _, line := range lines[start:] {
		if len(out) > 0 && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// The seeder half of the Tier-1 substrate. These pins are pure file reads over
// the seeder package, the archetype's data declaration and its pinned
// expectations, so a change that breaks the seeder's contract fails the
// required `check` lane instead of surfacing hours later in a scheduled lane.
const (
	tier1SettlePath     = tier1SeedPkgDir + "/settle.go"
	tier1ArchetypeDir   = "../../test/e2e/migration/archetypes/three-signal"
	tier1DeclPath       = tier1ArchetypeDir + "/seed/fixture.json"
	tier1ExpectedPath   = tier1ArchetypeDir + "/expected/tier1.json"
	tier1SeedCmdPath    = "./test/e2e/migration/cmd/seed"
	tier1ManifestOutput = "test/e2e/migration/.out/manifest.json"
	// tier1ArchetypeFixtureDir and tier1ArchetypeFixtureFile are the
	// directory/filename halves of migration-tier1-seed's fixture path, split
	// because the recipe parameterises the archetype segment between them
	// (`archetypes/$a/seed/fixture.json`) rather than hardcoding one
	// archetype — see TestMigrationTier1SeedRecipe.
	tier1ArchetypeFixtureDir  = "test/e2e/migration/archetypes/"
	tier1ArchetypeFixtureFile = "seed/fixture.json"
	// tier1ArchetypesVarName is the Justfile variable migration-tier1-seed's
	// empty-archetype default reads its archetype list from.
	tier1ArchetypesVarName = "MIGRATION_TIER1_ARCHETYPES"
)

// tier1RequiredArchetypes are the archetypes whose fixture data an @tier1
// scenario actually reads (see the `@archetype:` tags in
// test/e2e/migration/features/*.feature): three-signal for every scenario
// except MIG-02/06/07/08, which read kube-prometheus-stack. Both must be
// seeded into the ONE live stack `just migration-tier1` brings up, or
// whichever archetype is missing silently fails its scenarios — exactly the
// gap TestMigrationTier1SeedRecipe exists to keep from recurring.
var tier1RequiredArchetypes = []string{"three-signal", "kube-prometheus-stack"}

// compatSeederSources are the compatibility harnesses' seeders. They gate on
// the same upstream readiness metrics the migration seeder does, and that
// duplication is the one the "do not refactor three programs that each sit
// behind a required check" decision leaves open — so it is closed behaviourally
// here instead.
var compatSeederSources = []string{
	"../../compatibility/loki/cmd/seed/main.go",
	"../../compatibility/tempo/driver/seeder.go",
}

// TestMigrationTier1ReadinessMetricsMatchCompat pins every upstream metric name
// the migration seeder's settle gates key on to a name a compatibility seeder
// also keys on. An upstream rename then fails in ONE place: without this, the
// migration lane would keep polling a metric upstream had removed while the
// compat gates had already moved on, and the failure would read as a timeout
// rather than as the rename it is.
func TestMigrationTier1ReadinessMetricsMatchCompat(t *testing.T) {
	t.Parallel()

	names := metricConstValues(t, tier1SettlePath)
	if len(names) == 0 {
		t.Fatalf("%s declares no readiness-metric constants; the settle gates have lost their signals",
			tier1SettlePath)
	}

	var compat strings.Builder
	for _, path := range compatSeederSources {
		buf, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		compat.Write(buf)
	}
	haystack := compat.String()

	for _, name := range names {
		if !strings.Contains(haystack, name) {
			t.Fatalf("the migration seeder gates on %q, which no compatibility seeder mentions. Either "+
				"upstream renamed it and only one side followed, or a new signal was added without "+
				"telling the harnesses that share it. Sources scanned: %v", name, compatSeederSources)
		}
	}
}

// metricConstValues returns the string values of every `…Metric` constant
// declared in a Go file, read off the AST so a comment mentioning a metric name
// cannot satisfy the pin.
func metricConstValues(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasSuffix(name.Name, "Metric") || i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s = %s: %v", path, name.Name, lit.Value, err)
				}
				out = append(out, unquoted)
			}
		}
	}
	return out
}

// tier1Declaration is the subset of the archetype's data declaration these pins
// read.
type tier1Declaration struct {
	Services         []string  `json:"services"`
	StatusCodes      []string  `json:"http_status_codes"`
	LogFormats       []string  `json:"log_formats"`
	LogJob           string    `json:"log_job"`
	GaugeMetric      string    `json:"gauge_metric"`
	CounterMetric    string    `json:"counter_metric"`
	HistogramMetric  string    `json:"histogram_metric"`
	HistogramBounds  []float64 `json:"histogram_bounds"`
	TracesPerService int       `json:"traces_per_service"`
	SpansPerTrace    []int     `json:"spans_per_trace"`
}

// tier1Expected is the subset of the archetype's pinned expectations these pins
// read.
type tier1Expected struct {
	LogStreams       int `json:"log_streams"`
	LogRecords       int `json:"log_records"`
	PromSeries       int `json:"prom_series"`
	Traces           int `json:"traces"`
	TracesPerService int `json:"traces_per_service"`
	Spans            int `json:"spans"`
	SamplesPerSeries int `json:"samples_per_series"`
	VerifySteps      int `json:"verify_steps"`
	Series           struct {
		Gauge     int `json:"gauge"`
		Sum       int `json:"sum"`
		Histogram int `json:"histogram"`
	} `json:"series"`
	MetricQueries []struct {
		Name   string `json:"name"`
		Query  string `json:"query"`
		Series int    `json:"series"`
	} `json:"metric_queries"`
}

func readTier1Archetype(t *testing.T) (tier1Declaration, tier1Expected) {
	t.Helper()
	var decl tier1Declaration
	var expected tier1Expected
	for _, target := range []struct {
		path string
		into any
	}{{tier1DeclPath, &decl}, {tier1ExpectedPath, &expected}} {
		buf, err := os.ReadFile(target.path)
		if err != nil {
			t.Fatalf("read %s: %v", target.path, err)
		}
		if err := json.Unmarshal(buf, target.into); err != nil {
			t.Fatalf("parse %s: %v", target.path, err)
		}
	}
	return decl, expected
}

// TestMigrationTier1ExpectationsFollowTheDeclaration pins the archetype's
// expected cardinalities to what its data declaration actually implies. Those
// expectations are what the parity oracle is cross-checked against, so a
// hand-edited number there would let a shrunken fixture and a shrunken oracle
// agree with each other and report parity over almost no data.
func TestMigrationTier1ExpectationsFollowTheDeclaration(t *testing.T) {
	t.Parallel()

	decl, expected := readTier1Archetype(t)

	metricSeries := len(decl.Services) * len(decl.StatusCodes)
	logStreams := len(decl.Services) * len(decl.LogFormats)
	traces := len(decl.Services) * decl.TracesPerService
	// A classic-histogram series expands on the reference side into one
	// cumulative bucket per explicit bound, a +Inf bucket, a _count and a _sum.
	histogramPromSeries := metricSeries * (len(decl.HistogramBounds) + 3)
	promSeries := metricSeries*2 + histogramPromSeries

	// The window geometry has two definitions — the Go consts in seed/fixture.go
	// and these pinned counts — and they are bound HERE. Without this, a change
	// to SampleStep or VerifyStep leaves the JSON mis-declaring the fixture and
	// the required lane green, because the only assertion that reads both is
	// behind the `migration_tier1` tag. The window rolls with wall-clock now,
	// but its SHAPE does not, so the counts are exact.
	window := seed.NewWindow(time.Now())

	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"samples per series", expected.SamplesPerSeries, len(window.SampleTimes())},
		{"verify steps", expected.VerifySteps, len(window.VerifySteps())},
		{"gauge series", expected.Series.Gauge, metricSeries},
		{"sum series", expected.Series.Sum, metricSeries},
		{"histogram series", expected.Series.Histogram, metricSeries},
		{"log streams", expected.LogStreams, logStreams},
		{"log records", expected.LogRecords, logStreams * expected.SamplesPerSeries},
		{"traces", expected.Traces, traces},
		{"traces per service", expected.TracesPerService, decl.TracesPerService},
		{"prometheus series", expected.PromSeries, promSeries},
	} {
		if c.got != c.want {
			t.Fatalf("%s declares %s = %d, but %s implies %d",
				tier1ExpectedPath, c.what, c.got, tier1DeclPath, c.want)
		}
	}

	if expected.Spans <= expected.Traces {
		t.Fatalf("%s declares %d spans across %d traces; every trace holds at least %d spans, so the "+
			"span count cannot be that low", tier1ExpectedPath, expected.Spans, expected.Traces,
			decl.SpansPerTrace[0])
	}
	if expected.VerifySteps <= 1 {
		t.Fatalf("%s declares %d verify steps; a single-step window cannot show a divergence that only "+
			"appears part-way through the range", tier1ExpectedPath, expected.VerifySteps)
	}
}

// TestMigrationTier1IncumbentHoldsHistoryBeforeIngestStart pins the ONE
// deliberate asymmetry between the two sides of the fixture, which is the
// entire substance of MIG-23's split-read story: reference Prometheus holds
// history strictly BELOW ClickHouse's ingest-start, and ClickHouse holds none
// of it. Take the asymmetry away and the scenario's pre-boundary contrast
// becomes two empty answers agreeing with each other — a hollow green that
// only a live run would expose, and only if someone read its output closely.
//
// It also pins the geometry the live probe depends on: the newest
// incumbent-only sample sits exactly one SampleStep below ingest-start (so the
// probe lands ON a sample rather than in a gap), and the whole history stays
// strictly below the deepest instant the parity lane can reach (VerifyStart
// minus the widest lookback a corpus range selector may carry), so it can
// never leak into a parity diff.
func TestMigrationTier1IncumbentHoldsHistoryBeforeIngestStart(t *testing.T) {
	t.Parallel()

	decl, err := seed.LoadDeclaration(tier1DeclPath)
	if err != nil {
		t.Fatalf("load %s: %v", tier1DeclPath, err)
	}
	window := seed.NewWindow(time.Now())
	fixture, err := seed.Build(decl, window)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}

	pre := fixture.PromPreIngestSeries()
	if len(pre) != len(fixture.Gauge) {
		t.Fatalf("the fixture leaves the incumbent %d series before ingest-start but carries %d gauge series; "+
			"MIG-23 holds the incumbent's pre-boundary answer to the gauge cardinality", len(pre), len(fixture.Gauge))
	}
	if len(pre) == 0 {
		t.Fatalf("the fixture leaves the incumbent nothing before ClickHouse's ingest-start, so MIG-23's " +
			"pre-boundary contrast is two empty answers agreeing with each other")
	}

	probedInstant := window.SeedStart.Add(-seed.SampleStep)
	deepestLaneReach := window.VerifyStart.Add(-seed.RangeLookbackMargin)
	for _, s := range pre {
		if len(s.Samples) == 0 {
			t.Fatalf("an incumbent-only series carries no sample at all; the incumbent would answer nothing below the boundary")
		}
		for _, sample := range s.Samples {
			if !sample.Time.Before(window.SeedStart) {
				t.Fatalf("an incumbent-only sample lands at %s, at or after ingest-start %s; it would be written to a "+
					"window ClickHouse also holds and prove nothing about the split", sample.Time, window.SeedStart)
			}
			if !sample.Time.Before(deepestLaneReach) {
				t.Fatalf("an incumbent-only sample lands at %s, within reach of the parity lane's deepest lookback %s; "+
					"it would read as a reference-only divergence in every range comparison", sample.Time, deepestLaneReach)
			}
		}
		if newest := s.Samples[len(s.Samples)-1].Time; !newest.Equal(probedInstant) {
			t.Fatalf("the newest incumbent-only sample lands at %s, but MIG-23 probes %s (one sample step below "+
				"ingest-start); the probe would fall into a gap", newest, probedInstant)
		}
	}

	for _, s := range fixture.Gauge {
		for _, sample := range s.Samples {
			if sample.Time.Before(window.SeedStart) {
				t.Fatalf("a ClickHouse-side gauge sample lands at %s, before ingest-start %s; the boundary MIG-23 "+
					"measures off the live table would not exist", sample.Time, window.SeedStart)
			}
		}
	}

	// The manifest is the oracle the live scenario reads; a history the seeder
	// wrote but never published cannot be asserted against.
	m := seed.NewManifest(decl, fixture)
	if m.PreIngestSeries != len(pre) {
		t.Fatalf("the manifest publishes %d pre-ingest series but the fixture renders %d", m.PreIngestSeries, len(pre))
	}
	if want := len(window.PreIngestSampleTimes()); m.PreIngestSamplesPerSeries != want {
		t.Fatalf("the manifest publishes %d pre-ingest samples per series but the window carries %d",
			m.PreIngestSamplesPerSeries, want)
	}
	if !m.PreIngestStart.Equal(window.PreIngestStart()) {
		t.Fatalf("the manifest publishes pre-ingest start %s but the window starts it at %s",
			m.PreIngestStart, window.PreIngestStart())
	}
}

// TestMigrationTier1QueriesNameTheFixture pins the corpus-shape rule that keeps
// the ClickHouse-only schema warm-up rows unreachable from a comparison: every
// declared query names a fixture metric explicitly. A bare `sum(rate(...))`
// with no metric name would sweep in the warm-up series, which the collector
// writes to ClickHouse and to nothing else, so it would read as a permanent
// divergence that no amount of seeding could close.
func TestMigrationTier1QueriesNameTheFixture(t *testing.T) {
	t.Parallel()

	decl, expected := readTier1Archetype(t)
	if len(expected.MetricQueries) == 0 {
		t.Fatalf("%s declares no metric queries; the parity lane would compare nothing", tier1ExpectedPath)
	}

	fixtureMetrics := []string{decl.GaugeMetric, decl.CounterMetric, decl.HistogramMetric}
	seen := map[string]bool{}
	for _, q := range expected.MetricQueries {
		if q.Name == "" {
			t.Fatalf("%s declares a query with no name: %q", tier1ExpectedPath, q.Query)
		}
		if seen[q.Name] {
			t.Fatalf("%s declares two queries named %q", tier1ExpectedPath, q.Name)
		}
		seen[q.Name] = true
		if q.Series <= 0 {
			t.Fatalf("%s declares query %q with %d expected series; a zero-series expectation is "+
				"satisfied by a backend that returned nothing", tier1ExpectedPath, q.Name, q.Series)
		}
		var named bool
		for _, metric := range fixtureMetrics {
			if strings.Contains(q.Query, metric) {
				named = true
			}
		}
		if !named {
			t.Fatalf("%s query %q names no fixture metric (%v). A query that does not name one can "+
				"reach the ClickHouse-only schema warm-up rows, which have no reference-side "+
				"counterpart.", tier1ExpectedPath, q.Query, fixtureMetrics)
		}
	}
}

// TestMigrationTier1SeedRecipe pins the seed step into the lane's lifecycle.
// Without it the assertions would run against an empty stack, where every
// "both sides agree" comparison is vacuously true.
func TestMigrationTier1SeedRecipe(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	body := string(buf)

	recipe := justRecipeBody(t, body, "migration-tier1-seed")
	for _, want := range []string{
		tier1SeedCmdPath,
		tier1ArchetypeFixtureDir,
		tier1ArchetypeFixtureFile,
		tier1ManifestOutput,
	} {
		if !strings.Contains(recipe, want) {
			t.Fatalf("Justfile recipe migration-tier1-seed does not reference %q; body:\n%s", want, recipe)
		}
	}

	// The recipe's empty-archetype default must seed every archetype the
	// @tier1 scenario set needs, not just one — see tier1RequiredArchetypes.
	archetypesLine := tier1JustVarLine(t, body, tier1ArchetypesVarName)
	for _, archetype := range tier1RequiredArchetypes {
		if !strings.Contains(archetypesLine, archetype) {
			t.Fatalf("Justfile's %s does not list archetype %q, so migration-tier1-seed's default would not seed it: %s",
				tier1ArchetypesVarName, archetype, archetypesLine)
		}
	}
}

// tier1JustVarLine returns the single Justfile line declaring a top-level
// `NAME := "..."` variable, failing the test if the variable is not declared
// exactly once.
func tier1JustVarLine(t *testing.T, justfile, name string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(justfile, "\n") {
		if strings.HasPrefix(line, name+" :=") || strings.HasPrefix(line, name+":=") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("Justfile declares %q %d time(s), want exactly 1", name, len(found))
	}
	return found[0]
}

// TestMigrationTier1SampleBudgetIsPinned holds the tier-1 stack's per-query
// sample budget equal to the constant MIG-15 derives its over-budget subquery
// grid from. That scenario asks cerberus for an anchor grid one row past the
// budget and asserts the specific budget rejection; if the compose stack ever
// raised CERBERUS_QUERY_MAX_SAMPLES on its own, the grid would quietly land
// INSIDE the budget and the scenario would go green while asserting nothing
// about a refusal.
func TestMigrationTier1SampleBudgetIsPinned(t *testing.T) {
	t.Parallel()

	cf := readCompose(t, tier1ComposePath)
	cerb, ok := cf.Services[tier1CerberusService]
	if !ok {
		t.Fatalf("%s has no %q service", tier1ComposePath, tier1CerberusService)
	}
	raw, ok := cerb.Environment[tier1SampleBudgetEnv]
	if !ok {
		t.Fatalf("%s's %s service does not pin %s. MIG-15 derives its over-budget subquery grid "+
			"from steps.Tier1QuerySampleBudget; an implicit budget makes that derivation a guess.",
			tier1ComposePath, tier1CerberusService, tier1SampleBudgetEnv)
	}
	got, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s's %s = %q, which is not an integer: %v", tier1ComposePath, tier1SampleBudgetEnv, raw, err)
	}
	if got != steps.Tier1QuerySampleBudget {
		t.Fatalf("the tier-1 stack runs cerberus with %s=%d, but MIG-15 derives its over-budget grid "+
			"from steps.Tier1QuerySampleBudget=%d. Move both together.",
			tier1SampleBudgetEnv, got, steps.Tier1QuerySampleBudget)
	}
}

// tier1SampleBudgetEnv is the per-query sample budget the tier-1 cerberus runs
// under — MIG-15's per-tenant read budget, in that scenario's tenancy model.
const tier1SampleBudgetEnv = "CERBERUS_QUERY_MAX_SAMPLES"

// The build-once seam (Layer 14, D2): the migration lane runs the SAME three
// tier jobs twice against a release commit — once proving the source tree, once
// proving the image goreleaser built — and the only thing that differs is which
// cerberus the stack runs and which binary the scenarios exec. Every constant
// below exists in more than one file by necessity (a compose interpolation
// default, a Justfile variable, a Node const, a Go const, an ldflag), so the
// pins here hold them equal. A drift is silent by construction: the lane goes
// green having tested the wrong build.
const (
	// migrationArtifactScript resolves which cerberus the lane drives.
	migrationArtifactScript = "../../.github/scripts/migration-artifact.mjs"
	// migrationLocalImageVar is the Justfile variable naming the locally built
	// image tag.
	migrationLocalImageVar = "MIGRATION_LOCAL_IMAGE"
	// migrationImageRecipe is the SINGLE point at which the image enters the
	// Docker daemon.
	migrationImageRecipe = "migration-cerberus-image"
	// dockerfileLocalPath builds the image the stacks run when no released
	// artifact is supplied; cerberusMainPath declares the source-build stamp.
	dockerfileLocalPath = "../../Dockerfile.local"
	cerberusMainPath    = "../../cmd/cerberus/main.go"
	// buildFlag forces `docker compose up` to compile the build context even
	// when the image is already present, which is exactly how a released
	// artifact gets replaced by a recompile of whatever tree the runner holds.
	buildFlag = "--build"
)

// composeImageDefaultRe splits a `${VAR:-default}` compose interpolation.
var composeImageDefaultRe = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*):-(.+)\}$`)

// TestMigrationImageIsAcquiredOnceNeverRebuilt pins the seam that lets the
// release lane test the artifact it just built.
//
// `pull_policy: build` plus `up --build` — the shape this replaced — means
// pointing `image:` at a released tag does NOT stop compose recompiling the
// source tree over it. The lane would report "tested the release artifact"
// having tested source, and every gate in the pipeline would stay green.
func TestMigrationImageIsAcquiredOnceNeverRebuilt(t *testing.T) {
	t.Parallel()

	cf := readCompose(t, tier1ComposePath)
	cerb, ok := cf.Services[tier1CerberusService]
	if !ok {
		t.Fatalf("%s has no %q service", tier1ComposePath, tier1CerberusService)
	}

	m := composeImageDefaultRe.FindStringSubmatch(cerb.Image)
	if m == nil {
		t.Fatalf("%s's %s service declares image %q, want a `${VAR:-default}` interpolation. A literal "+
			"tag gives release.yml's artifact lane no way to point the stack at the image goreleaser "+
			"built, so it would prove the source tree under an artifact-shaped job name.",
			tier1ComposePath, tier1CerberusService, cerb.Image)
	}
	imageEnvVar, localTag := m[1], m[2]
	if imageEnvVar != lib.ImageEnv {
		t.Fatalf("the compose image interpolates %s but the harness and the resolver read %s; the "+
			"release lane would set a variable nothing consumes", imageEnvVar, lib.ImageEnv)
	}
	if cerb.PullPolicy != composePullPolicyNever {
		t.Fatalf("%s's %s service declares pull_policy %q, want %q. Anything else lets the up path "+
			"acquire the image itself, which is how a source rebuild silently replaces a released "+
			"artifact. The image is put in the daemon by `just %s` BEFORE up runs.",
			tier1ComposePath, tier1CerberusService, cerb.PullPolicy, composePullPolicyNever, migrationImageRecipe)
	}

	// The same tag lives in three files. Hold them equal, or a `just` run and a
	// compose run would disagree about which image "locally built" means and
	// the stack would fail to resolve one.
	justfile, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	body := string(justfile)
	justTag := justVariableList(t, body, migrationLocalImageVar)
	if len(justTag) != 1 || justTag[0] != localTag {
		t.Fatalf("Justfile's %s is %v but %s defaults to %q; `just %s` would build one tag while "+
			"compose looked for another", migrationLocalImageVar, justTag, tier1ComposePath, localTag,
			migrationImageRecipe)
	}
	resolver, err := os.ReadFile(migrationArtifactScript)
	if err != nil {
		t.Fatalf("read %s: %v", migrationArtifactScript, err)
	}
	if want := "export const LOCAL_IMAGE_TAG = '" + localTag + "'"; !strings.Contains(string(resolver), want) {
		t.Fatalf("%s does not declare %s, so the resolver would export a CERBERUS_IMAGE the compose "+
			"stack does not recognise", migrationArtifactScript, want)
	}

	// The up recipes must acquire the image through the one recipe that knows
	// the build-or-pull decision, and must not carry --build.
	for _, recipe := range []string{"migration-tier1-up", "migration-tier2-up"} {
		recipeBody := justRecipeBody(t, body, recipe)
		if !strings.Contains(recipeBody, "just "+migrationImageRecipe) {
			t.Fatalf("Justfile recipe %s does not invoke `just %s`, so nothing puts the image in the "+
				"daemon and `up` (pull_policy: %s) cannot resolve it. Body:\n%s",
				recipe, migrationImageRecipe, composePullPolicyNever, recipeBody)
		}
		if strings.Contains(recipeBody, buildFlag) {
			t.Fatalf("Justfile recipe %s passes %s to `docker compose up`. That forces a source build "+
				"regardless of pull_policy, so a release run would recompile this tree over the "+
				"released image and still report green. Body:\n%s", recipe, buildFlag, recipeBody)
		}
	}

	// The acquisition recipe itself must be able to do BOTH halves: build the
	// local tag from Dockerfile.local, and pull anything else.
	acquire := justRecipeBody(t, body, migrationImageRecipe)
	for _, want := range []string{"docker build", "Dockerfile.local", "docker pull", lib.ImageEnv} {
		if !strings.Contains(acquire, want) {
			t.Fatalf("Justfile recipe %s does not reference %q; it must build the local tag from "+
				"Dockerfile.local and pull a released one, decided by %s alone. Body:\n%s",
				migrationImageRecipe, want, lib.ImageEnv, acquire)
		}
	}
}

// composePullPolicyNever is the only pull policy compatible with the
// acquire-then-up contract.
const composePullPolicyNever = "never"

// TestMigrationVersionStampsMatchTheirBuilds holds the two stamps the harness
// computes its expectations from equal to the builds that actually produce
// them. lib.ExpectedServerVersion() asserting against a stale LocalImageVersion
// would pass for the wrong reason on every from-source run and then hide a real
// mis-wiring on the release path.
func TestMigrationVersionStampsMatchTheirBuilds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		path, want, why string
	}{
		{
			path: dockerfileLocalPath,
			want: "-X main.Version=" + lib.LocalImageVersion,
			why: "the tier-1/tier-2 compose stacks run this image, and the live GET /info probe in " +
				"tier1_stack_test.go holds the server to lib.LocalImageVersion",
		},
		{
			path: cerberusMainPath,
			want: `var Version = "` + lib.SourceBuildVersion + `"`,
			why: "a from-source `go build` carries no ldflags, and lib.BuildCerberus holds the CLI it " +
				"produced to lib.SourceBuildVersion",
		},
	} {
		buf, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if !strings.Contains(string(buf), tc.want) {
			t.Fatalf("%s does not contain %q — %s. Move the stamp and the constant together.",
				tc.path, tc.want, tc.why)
		}
	}

	// The resolver computes the same source stamp on the Node side.
	resolver, err := os.ReadFile(migrationArtifactScript)
	if err != nil {
		t.Fatalf("read %s: %v", migrationArtifactScript, err)
	}
	if want := "export const SOURCE_BUILD_VERSION = '" + lib.SourceBuildVersion + "'"; !strings.Contains(string(resolver), want) {
		t.Fatalf("%s does not declare %s, so its `--version` probe would expect a different stamp "+
			"than lib.BuildCerberus does and one of the two would fail on a healthy build",
			migrationArtifactScript, want)
	}

	// The two stamps must differ, or neither probe could tell a from-source run
	// from an image run.
	if lib.SourceBuildVersion == lib.LocalImageVersion {
		t.Fatalf("lib.SourceBuildVersion and lib.LocalImageVersion are both %q; the CLI and the "+
			"server probes would agree on every path and prove nothing", lib.SourceBuildVersion)
	}
}

// TestMigrationTiersAcquireTheProbedBinary asserts every tier package gets its
// cerberus through lib.BuildCerberus — the ONE path that probes `--version` and
// holds the answer to what this run is supposed to be testing. A tier that
// `go build`s its own binary, or reads CERBERUS_BIN directly, would run its
// whole scenario set against an unverified executable.
func TestMigrationTiersAcquireTheProbedBinary(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(tiersDir)
	if err != nil {
		t.Fatalf("read %s: %v", tiersDir, err)
	}

	var scanned int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		scanned++
		dir := filepath.Join(tiersDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var probed bool
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") {
				continue
			}
			buf, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", filepath.Join(dir, f.Name()), err)
			}
			if strings.Contains(string(buf), "lib.BuildCerberus(") {
				probed = true
			}
		}
		if !probed {
			t.Fatalf("tier package %s never calls lib.BuildCerberus, so the binary its scenarios exec "+
				"is never held to the version this run drives", dir)
		}
	}

	if scanned != wantTierSuites {
		t.Fatalf("scanned %d tier package(s) under %s, want %d. A loop over vanished directories is "+
			"vacuously green; a new tier must be wired through lib.BuildCerberus and this count raised.",
			scanned, tiersDir, wantTierSuites)
	}
}

// migrationTierJobs are the jobs in migration-e2e.yml that drive a tier.
var migrationTierJobs = []string{"migration-tier0", tier1JobName, "migration-tier2"}

// TestMigrationTierJobsResolveTheArtifact pins the resolver into every tier
// job. A tier that skips it either compiles no CLI at all (release path: no
// binary to hand the scenarios) or, worse, builds one from source while the job
// claims to be driving a released image.
func TestMigrationTierJobsResolveTheArtifact(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(tier1WorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", tier1WorkflowPath, err)
	}
	body := string(workflow)

	for _, job := range migrationTierJobs {
		jobBody := workflowJobBody(t, body, job)
		for _, want := range []string{
			"node .github/scripts/migration-artifact.mjs",
			"CERBERUS_IMAGE_INPUT: ${{ inputs.cerberus_image }}",
			"CERBERUS_EXPECT_VERSION_INPUT: ${{ inputs.expect_version }}",
		} {
			if !strings.Contains(jobBody, want) {
				t.Fatalf("%s job %q does not carry %q, so the release lane's `cerberus_image` input "+
					"would not reach it and the tier would silently test this tree instead. Body:\n%s",
					tier1WorkflowPath, job, want, jobBody)
			}
		}
		// The raw compile the resolver replaced. Leaving one behind would hand
		// the scenarios an unprobed binary on the release path.
		if strings.Contains(jobBody, "go build -o build/cerberus") {
			t.Fatalf("%s job %q still compiles the CLI directly. That binary bypasses the resolver's "+
				"version probe, so a release run would exec a source build while claiming to test the "+
				"released artifact. Body:\n%s", tier1WorkflowPath, job, jobBody)
		}
	}
}
