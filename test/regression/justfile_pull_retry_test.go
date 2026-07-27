package regression

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	for _, v := range []string{"CH_TEST_IMAGE", "CH_TEST_IMAGE_PRIOR"} {
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
