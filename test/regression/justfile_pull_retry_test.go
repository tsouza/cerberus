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

// TestJustfileComposeUpPrePullsWithRetry pins that any recipe bringing a compose
// stack up first acquires its images through `_compose-pull-retry`. Without it,
// `up` does the pull itself, single-attempt, and a flaky registry fails the lane.
func TestJustfileComposeUpPrePullsWithRetry(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	// `docker compose ... up`, allowing the flags and line continuations that
	// sit between the two words in the real recipes.
	composeUpRE := regexp.MustCompile(`docker compose[\s\S]*?\bup\b`)

	found := 0
	for name, body := range justRecipes(t, string(buf)) {
		if !composeUpRE.MatchString(body) {
			continue
		}
		found++
		if !strings.Contains(body, "_compose-pull-retry") {
			t.Errorf("Justfile recipe %q brings a compose stack up without calling `_compose-pull-retry` first. "+
				"`docker compose up` pulls missing images with no retry, so one Docker Hub timeout fails the lane "+
				"(this is what broke the v1.13.0 release's Tier-1 leg).", name)
		}
	}

	if found == 0 {
		t.Fatal("no `docker compose ... up` recipe found — the guard is scanning for a shape that no longer exists")
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

// writeStubComposeDocker drops a `docker` onto dir that answers `compose …
// config` with the model at modelPath, reports every image as absent from the
// daemon, and records each `docker pull <ref>`.
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
		"    echo \"$2\" >> " + shellQuote(callLog),
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
