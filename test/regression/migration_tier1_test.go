package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
		{recipe: "migration-tier1-up", wants: []string{composeRef, "up ", "--wait"}},
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

	if !strings.Contains(body, "migration-tier1: migration-tier1-up migration-tier1-run migration-tier1-down") {
		t.Fatal("Justfile has no `migration-tier1` composite chaining up -> run -> down")
	}
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
