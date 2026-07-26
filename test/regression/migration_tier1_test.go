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

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
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

// TestMigrationTier1ReferencePrometheusIsWriteOnly pins the reference
// Prometheus to a single sample path. A scrape config would give the reference
// a second, independent source of samples, and two paths cannot land identical
// timestamps — while the migration comparator keys samples by exact timestamp,
// so every scraped sample would read as a divergence.
func TestMigrationTier1ReferencePrometheusIsWriteOnly(t *testing.T) {
	t.Parallel()

	promCfg, err := os.ReadFile(tier1PrometheusPath)
	if err != nil {
		t.Fatalf("read %s: %v", tier1PrometheusPath, err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(promCfg, &parsed); err != nil {
		t.Fatalf("parse %s: %v", tier1PrometheusPath, err)
	}
	if _, found := parsed["scrape_configs"]; found {
		t.Fatalf("%s declares scrape_configs. The reference side is remote-write only: a scrape is a "+
			"second sample path, and the comparator keys samples by exact timestamp.", tier1PrometheusPath)
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
// `name:` header up to the next non-indented, non-blank line.
func justRecipeBody(t *testing.T, justfile, recipe string) string {
	t.Helper()
	lines := strings.Split(justfile, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, recipe+":") {
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
)

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
		strings.TrimPrefix(tier1DeclPath, "../../"),
		tier1ManifestOutput,
	} {
		if !strings.Contains(recipe, want) {
			t.Fatalf("Justfile recipe migration-tier1-seed does not reference %q; body:\n%s", want, recipe)
		}
	}
}
