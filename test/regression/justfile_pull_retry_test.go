package regression

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The v1.13.0 release run died on this line, mid-publish:
//
//	Head "https://registry-1.docker.io/v2/prom/prometheus/manifests/v3.11.3":
//	context deadline exceeded (Client.Timeout exceeded while awaiting headers)
//	error: recipe `migration-tier1-up` failed on line 1306 with exit code 1
//
// `docker compose up` pulls the images it is missing but has no retry of its
// own, so one transient Docker Hub timeout took down the Tier-1 leg, which
// failed `migration-e2e`, which skipped `publish` — a green build stopped one
// step short of shipping because a registry HEAD request was slow once.
//
// The Justfile already had `_pull-retry` (5 attempts, linear backoff) for the
// k3d lanes; the compose lanes simply never went through it. These tests pin
// that every image acquisition in the Justfile is retried, so the next lane
// added can't quietly reintroduce a single-attempt pull.

// justRecipes splits a Justfile into recipe name -> body. A recipe header sits
// at column 0 and ends in `:`; its body is the indented block beneath it.
func justRecipes(t *testing.T, src string) map[string]string {
	t.Helper()

	headerRE := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\s*(?:[+*]?[A-Za-z0-9_]+\s*)*:`)

	recipes := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			recipes[name] = body.String()
		}
		body.Reset()
	}

	for _, line := range strings.Split(src, "\n") {
		switch {
		case strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t"):
			if name != "" {
				body.WriteString(line)
				body.WriteString("\n")
			}
		case strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#"):
			// Blank lines and comments do not end a recipe body in just, but
			// they carry no commands either — skip without flushing.
		case strings.Contains(line, ":="):
			// A variable assignment, not a recipe header — both end in a colon
			// as far as the header pattern is concerned.
			flush()
			name = ""
		default:
			flush()
			if m := headerRE.FindStringSubmatch(line); m != nil {
				name = m[1]
			} else {
				name = ""
			}
		}
	}
	flush()

	if len(recipes) == 0 {
		t.Fatal("parsed zero recipes from the Justfile — the parser, not the Justfile, is wrong")
	}
	return recipes
}

// composeUpUnits yields every unit of execution in the tree that can bring a
// compose stack up, keyed by a label naming it: each Justfile recipe on its own,
// each workflow JOB on its own, and each shell script whole. The unit is what a
// pre-pull has to appear in, and it is the unit that shares a daemon — `just`
// runs one recipe, a script runs top to bottom in one shell, and a job's steps
// share one runner. Two jobs do not: `compose-smoke-shard`'s pre-pull leaves
// nothing in `compose-smoke-shard-info`'s daemon, so a file-wide scope would
// pass a workflow where only one of its two shards was covered.
func composeUpUnits(t *testing.T) map[string]string {
	t.Helper()

	units := map[string]string{}
	for _, file := range buildScanFiles(t) {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		switch ext := filepath.Ext(file); {
		case filepath.Base(file) == "Justfile":
			for name, body := range justRecipes(t, string(src)) {
				units[file+":"+name] = body
			}
		case ext == ".yml" || ext == ".yaml":
			for name, body := range workflowJobs(string(src)) {
				units[file+":"+name] = body
			}
		default:
			units[file] = string(src)
		}
	}
	return units
}

// workflowJobs splits a workflow into its jobs. A job key sits at exactly two
// spaces of indentation under the top-level `jobs:` mapping; whatever precedes
// the first one (triggers, top-level env) is returned as a unit of its own so a
// command there is still judged. A composite action declares no `jobs:` at all
// and comes back whole, under that same preamble key.
func workflowJobs(src string) map[string]string {
	jobKeyRE := regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)

	jobs := map[string]string{}
	name := "(preamble)"
	inJobs := false
	var body strings.Builder
	flush := func() {
		jobs[name] = body.String()
		body.Reset()
	}

	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "jobs:") {
			inJobs = true
		}
		if m := jobKeyRE.FindStringSubmatch(line); inJobs && m != nil {
			flush()
			name = m[1]
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return jobs
}

// composePrePullMarkers are the two spellings of "this stack's images were
// already acquired over the authenticated path": the Justfile's wrapper recipe,
// and the module it delegates to, which a shell script or a workflow step calls
// directly.
var composePrePullMarkers = []string{composePrePullRecipeName, composePullModule}

// `docker compose ... up`, on one logical line so the flags and `\`
// continuations that sit between the two words don't hide it.
var composeUpCommand = regexp.MustCompile(`\bdocker\s+compose\b.*\bup\b`)

// minComposeUpSites is the floor for the class scan, counted in `up` COMMANDS
// rather than in units — several units issue two. The real count is 10 (2
// Justfile recipes, 3 compatibility harnesses issuing 4, e2e.yml's two shards,
// bench/histogram's 2); the floor sits below it so an ordinary lane removal
// doesn't fail the guard, while a refactor that leaves it scanning for a shape
// nothing matches does.
const minComposeUpSites = 5

// earliestMarker returns the offset of the first marker in line, or -1.
func earliestMarker(line string, markers []string) int {
	at := -1
	for _, m := range markers {
		if i := strings.Index(line, m); i != -1 && (at == -1 || i < at) {
			at = i
		}
	}
	return at
}

// TestComposeUpAcquiresImagesOverTheAuthenticatedPullPath pins that anything
// bringing a compose stack up acquires that stack's images FIRST, through the
// shared pre-pull.
//
// Two separate failures live in this one gate. `docker compose up` pulls what it
// is missing with no retry of its own, so one transient Docker Hub timeout takes
// the lane down — that is what broke the v1.13.0 release's Tier-1 leg. And
// compose's pull path does not carry the credentials `docker login` wrote, so
// even a retried compose pull spends the ANONYMOUS per-runner-IP quota: measured
// three seconds after `Login Succeeded!`, `up` was refused with "you have reached
// your UNAUTHENTICATED pull rate limit" while a `docker pull` of the same image
// in the same job succeeded.
//
// The scan is repo-wide rather than Justfile-only because that is exactly how
// the second failure survived the first fix: the Justfile lanes were covered,
// and the three compatibility harnesses, e2e.yml's compose-smoke jobs and the
// histogram bench — all of which call `docker compose up` from a shell script or
// a workflow step — were not, so they went on spending the anonymous quota.
func TestComposeUpAcquiresImagesOverTheAuthenticatedPullPath(t *testing.T) {
	t.Parallel()

	found := 0
	for label, body := range composeUpUnits(t) {
		prePulled := false
		for _, line := range logicalLines(body) {
			if isProse(line) {
				continue
			}
			marker := earliestMarker(line, composePrePullMarkers)
			if up := composeUpCommand.FindStringIndex(line); up != nil {
				found++
				// Order is the whole guarantee: a pre-pull that runs after the
				// `up` acquires images `up` has already gone to the registry
				// for. A marker later on the same line is later, too.
				if !prePulled && (marker == -1 || marker > up[0]) {
					t.Errorf("%s brings a compose stack up without first pre-pulling its images through %s:\n\t%s\n"+
						"`docker compose up` pulls what it is missing single-attempt AND over the anonymous pull path, "+
						"so a Docker Hub timeout or a rate limit fails the lane even though the job is logged in.",
						label, strings.Join(composePrePullMarkers, " / "), strings.TrimSpace(line))
				}
			}
			if marker != -1 {
				prePulled = true
			}
		}
	}

	if found < minComposeUpSites {
		t.Fatalf("only %d `docker compose ... up` command(s) found in the tree, want at least %d — the guard "+
			"is scanning for a shape that no longer exists", found, minComposeUpSites)
	}
}

// TestJustfileIntegrationLanesPrePullTestImages pins that every `-tags=integration`
// recipe acquires its container images through `_pull-retry` first. Those tests
// start ClickHouse via testcontainers, which does the pull itself with no retry
// — the `schema-ddl` job failed exactly that way during the v1.13.0 release
// window. testcontainers reuses an image already in the daemon, so pre-pulling
// is what makes the pull retryable.
func TestJustfileIntegrationLanesPrePullTestImages(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	const integrationTag = "-tags=integration"

	found := 0
	for name, body := range justRecipes(t, string(buf)) {
		if !strings.Contains(body, integrationTag) {
			continue
		}
		found++
		if !strings.Contains(body, "_pull-retry") {
			t.Errorf("Justfile recipe %q runs %s tests without pre-pulling their images via `_pull-retry`. "+
				"testcontainers pulls single-attempt, so one Docker Hub timeout fails the lane.", name, integrationTag)
		}
	}

	if found == 0 {
		t.Fatal("no `-tags=integration` recipe found — the guard is scanning for a shape that no longer exists")
	}
}

// TestIntegrationImagePinsMatchTheJustfile holds the Justfile's pre-pull list
// equal to the container images the integration tests actually name. A test that
// introduces a new image without adding it here would go back to an unretried
// testcontainers pull, and nothing else would notice.
func TestIntegrationImagePinsMatchTheJustfile(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	justfile := string(buf)

	pinned := map[string]bool{}
	for _, v := range []string{"CH_TEST_IMAGE", "CH_TEST_IMAGE_PRIOR", "CH_STRICT_SCAN_IMAGE"} {
		for _, img := range justVariableList(t, justfile, v) {
			pinned[img] = true
		}
	}

	// `clickhouse/clickhouse-server:<tag>` as a Go string literal. Scoped to that
	// repository because it is the only image the integration tests start; a new
	// one lands as a failure here rather than silently widening the pattern.
	imageRE := regexp.MustCompile(`"(clickhouse/clickhouse-server:[A-Za-z0-9._-]+)"`)

	referenced := map[string][]string{}
	for _, root := range []string{"../../internal", "../../test"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range imageRE.FindAllStringSubmatch(string(src), -1) {
				referenced[m[1]] = append(referenced[m[1]], path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(referenced) == 0 {
		t.Fatal("no integration test names a ClickHouse image — the guard is scanning for a shape that no longer exists")
	}

	for img, files := range referenced {
		if !pinned[img] {
			t.Errorf("%s is started by %s but is not in the Justfile's pre-pull list; testcontainers would "+
				"pull it single-attempt. Add it to CH_TEST_IMAGE / CH_TEST_IMAGE_PRIOR and to the lane that needs it.",
				img, files[0])
		}
	}
	for img := range pinned {
		if _, ok := referenced[img]; !ok {
			t.Errorf("the Justfile pre-pulls %s but no integration test starts it — a stale pin costs every "+
				"integration lane an image download it never uses.", img)
		}
	}
}

// TestComposePrePullUsesTheAuthenticatedPullPath pins that the pre-pull fetches
// with `docker pull`, not `docker compose pull`.
//
// The two do not share a credential source. Measured four seconds apart inside
// one job (run 30702344415, job 91376064528): `docker pull` fetched a Docker Hub
// image whole, while compose's pull in the same job was refused with "you have
// reached your UNAUTHENTICATED pull rate limit" — the registry's own words for a
// request that carried no credentials, seconds after `docker/login-action`
// reported success. Run through compose, the mechanism built to absorb Docker
// Hub failures was spending the ANONYMOUS quota, which is why the pre-pull never
// helped the lanes it was added for (issue #1565).
func TestComposePrePullUsesTheAuthenticatedPullPath(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	body := composePrePullRecipe(t, string(buf))

	if !strings.Contains(body, composePullModule) {
		t.Errorf("%s does not go through `%s`, which owns the model resolution and the shared retry policy.",
			composePrePullRecipeName, composePullModule)
	}

	for _, line := range logicalLines(body) {
		if isProse(line) {
			continue
		}
		if regexp.MustCompile(`\bdocker\s+compose\b.*\bpull\b`).MatchString(line) {
			t.Errorf("%s pre-pulls with `docker compose pull`: %s\n"+
				"Compose's pull path does not carry the credentials `docker login` wrote, so it spends the "+
				"anonymous per-runner-IP quota and fails while `docker pull` of the same image succeeds.",
				composePrePullRecipeName, strings.TrimSpace(line))
		}
	}
}

// TestComposePrePullPullsExactlyTheFetchableImages drives the resolver against a
// stub `docker` whose `compose config` reports the REAL migration-tier compose
// model, and asserts it pulls every fetchable image and no built one.
//
// The first cut of the pre-pull pulled every image `config --images` listed that
// `docker image inspect` could not find locally. Tier-2's `dead-end-receiver` is
// built by compose during `up`, so at pre-pull time it is legitimately absent —
// and `cerberus-migration-tier2:dead-end-receiver` exists in no registry, so the
// pull failed five times and took the lane with it (run 30281594098). Absence
// from the daemon means "not built yet" just as often as "not fetched yet"; only
// the `build:` sections distinguish them.
func TestComposePrePullPullsExactlyTheFetchableImages(t *testing.T) {
	t.Parallel()

	model, fetchable, built := composeModelFromTree(t, "../../test/e2e/migration")
	if len(built) == 0 {
		t.Fatal("no compose service under test/e2e/migration is built rather than fetched — " +
			"the guard is scanning for a shape that no longer exists")
	}
	if len(fetchable) == 0 {
		t.Fatal("no compose service under test/e2e/migration is fetched from a registry — " +
			"the guard is scanning for a shape that no longer exists")
	}

	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.json")
	if err := os.WriteFile(modelPath, model, 0o600); err != nil {
		t.Fatalf("write compose model: %v", err)
	}
	callLog := filepath.Join(dir, "pulls")
	writeStubComposeDocker(t, dir, modelPath, callLog)

	cmd := exec.Command("node", "../../.github/scripts/"+composePullModule, "docker-compose.yml")
	cmd.Env = append(
		os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"COMPOSE_PULL_BACKOFF_SECONDS=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed on a model whose every pull succeeds: %v\noutput:\n%s", composePullModule, err, out)
	}

	logged, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read stub call log: %v", err)
	}
	pulled := strings.Fields(string(logged))
	sort.Strings(pulled)

	if !slices.Equal(pulled, fetchable) {
		t.Errorf("pulled %v, want exactly %v\noutput:\n%s", pulled, fetchable, out)
	}
	for _, ref := range built {
		if slices.Contains(pulled, ref) {
			t.Errorf("pulled %q, which compose BUILDS during `up`. It exists in no registry, so the pull "+
				"fails and takes the lane with it.", ref)
		}
	}
}

const (
	composePrePullRecipeName = "_compose-pull-retry"
	composePullModule        = "compose-pull-images.mjs"
)

func composePrePullRecipe(t *testing.T, justfile string) string {
	t.Helper()

	body, ok := justRecipes(t, justfile)[composePrePullRecipeName]
	if !ok {
		t.Fatalf("no %q recipe in the Justfile — the guard is scanning for a shape that no longer exists",
			composePrePullRecipeName)
	}
	return body
}

// mirrorRegistryPrefix is how a ref announces itself as the GHCR copy rather
// than the upstream image (.github/scripts/lib/mirror.mjs).
const mirrorRegistryPrefix = "ghcr.io/"

// writeStubComposeDocker drops a `docker` onto dir that answers `compose …
// config` with the model at modelPath, reports every image as absent from the
// daemon, and records each image the caller ACQUIRES under its upstream name.
//
// Acquisition has two shapes and the log flattens them, because what this test
// asserts is which images end up in the daemon, not which registry answered.
// `pullImageWithRetry` reaches for the mirrored copy first and re-tags it to the
// upstream name, so a mirrored image is `docker pull ghcr.io/…` followed by
// `docker tag ghcr.io/… <upstream>`; an unmirrored one is a plain `docker pull
// <upstream>`. The stub records the upstream name in both cases.
func writeStubComposeDocker(t *testing.T, dir, modelPath, callLog string) {
	t.Helper()

	script := strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"  compose)",
		"    cat " + shellQuote(modelPath),
		"    exit 0",
		"    ;;",
		"  image)",
		// Nothing is held locally, so the model — not daemon state — is the
		// only thing that can keep a built image out of the pull set.
		"    exit 1",
		"    ;;",
		"  pull)",
		"    case \"$2\" in",
		// The mirror fetch itself is not the acquisition; the re-tag is.
		"      " + mirrorRegistryPrefix + "*) exit 0 ;;",
		"    esac",
		"    echo \"$2\" >> " + shellQuote(callLog),
		"    exit 0",
		"    ;;",
		"  tag)",
		"    echo \"$3\" >> " + shellQuote(callLog),
		"    exit 0",
		"    ;;",
		"esac",
		"echo \"stub: unexpected docker $*\" >&2",
		"exit 2",
		"",
	}, "\n")

	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
}

// composeModelFromTree reads every compose file under root and renders the
// merged services as the JSON `docker compose config --format json` prints,
// alongside the sorted image refs compose would fetch and the ones it builds.
func composeModelFromTree(t *testing.T, root string) (model []byte, fetchable, built []string) {
	t.Helper()

	type service struct {
		Image      string    `yaml:"image"`
		Build      yaml.Node `yaml:"build"`
		PullPolicy string    `yaml:"pull_policy"`
	}

	services := map[string]any{}
	fetchSet := map[string]bool{}
	builtSet := map[string]bool{}
	files := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if d.IsDir() || !strings.HasPrefix(base, "docker-compose") {
			return nil
		}
		if ext := filepath.Ext(base); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc struct {
			Services map[string]service `yaml:"services"`
		}
		if err := yaml.Unmarshal(src, &doc); err != nil {
			return err
		}
		files++
		for name, svc := range doc.Services {
			isBuilt := !svc.Build.IsZero() || svc.PullPolicy == "build" || svc.PullPolicy == "never"
			rendered := map[string]any{"image": svc.Image}
			if !svc.Build.IsZero() {
				rendered["build"] = map[string]any{"context": "."}
			}
			if svc.PullPolicy != "" {
				rendered["pull_policy"] = svc.PullPolicy
			}
			services[base+"-"+name] = rendered
			switch {
			case isBuilt:
				builtSet[svc.Image] = true
			case svc.Image != "":
				fetchSet[svc.Image] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files == 0 {
		t.Fatalf("no compose file under %s — the guard is scanning for a shape that no longer exists", root)
	}

	model, err = json.Marshal(map[string]any{"services": services})
	if err != nil {
		t.Fatalf("render compose model: %v", err)
	}
	// A ref that is built in one tier and fetched in another is built: compose
	// would have it in the daemon, and no registry serves it.
	for ref := range fetchSet {
		if builtSet[ref] {
			delete(fetchSet, ref)
		}
	}
	for ref := range fetchSet {
		fetchable = append(fetchable, ref)
	}
	for ref := range builtSet {
		built = append(built, ref)
	}
	sort.Strings(fetchable)
	sort.Strings(built)
	return model, fetchable, built
}

// TestJustfileNoUnretriedDockerPull pins that `docker pull` appears only inside
// `_pull-retry`, the one recipe whose job is to retry it. Every other call site
// goes through that recipe rather than rolling its own single-attempt pull.
func TestJustfileNoUnretriedDockerPull(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	const retryRecipe = "_pull-retry"

	for name, body := range justRecipes(t, string(buf)) {
		if name == retryRecipe {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "docker pull ") {
				t.Errorf("Justfile recipe %q calls `docker pull` directly: %s\n"+
					"Route it through `just %s <image>...` so a transient registry timeout retries "+
					"instead of failing the lane.", name, strings.TrimSpace(line), retryRecipe)
			}
		}
	}
}
