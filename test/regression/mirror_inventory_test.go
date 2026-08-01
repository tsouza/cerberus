package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The GHCR mirror only helps for images it actually holds. `mirroredRef`
// returns null for anything outside `mirroredImages`, and `pullImageWithRetry`
// then falls straight through to Docker Hub — quietly, because a fallback is
// the correct behaviour for an image nobody has mirrored yet.
//
// That quietness is the drift risk. Bumping `clickhouse/clickhouse-server:26.5`
// to `26.7` in a compose file is a one-token edit that silently moves that pull
// back onto the shared anonymous quota, and nothing goes red until the quota is
// spent — which is the incident this whole mechanism exists to remove. So the
// inventory is pinned against the tree in both directions: an upstream ref the
// tree names and the inventory omits fails here, and an inventory entry no file
// names any more fails here too (a stale entry costs a nightly mirror push and
// misleads the next reader about what CI pulls).

const (
	mirrorModule = "../../.github/scripts/lib/mirror.mjs"

	// Floor for the tree scan. The real count is in the mid-teens; the floor
	// sits below it so a refactor that leaves the scanner matching nothing
	// fails loudly instead of passing vacuously.
	minUpstreamImageRefs = 10
)

// mirrorInventoryLiteral isolates the `mirroredImages` array so the scan cannot
// pick up refs quoted in the module's prose.
var mirrorInventoryLiteral = regexp.MustCompile(`(?s)export const mirroredImages = \[(.*?)\n\];`)

// inventoryEntry matches one array element and nothing else. A bare
// `'([^']+)'` would also pair the apostrophes in the module's own prose.
var inventoryEntry = regexp.MustCompile(`(?m)^\s*'([^']+)',\s*$`)

// imageRefContexts are the shapes that make this project acquire an image, one
// regex per shape, each capturing the ref (or, for a Justfile pin, the
// space-separated list of them):
//
//   - a compose service / Kubernetes container / Helm values `image:`;
//   - a Dockerfile `FROM`;
//   - an explicit `docker pull`, wherever it is spelled;
//   - a Justfile `*_IMAGE`/`*_IMAGES` pin, which `_pull-retry` expands;
//   - a composite-action input `default:`, which is where the BuildKit
//     bootstrap image is pinned.
//
// Matching CONTEXTS rather than ref-shaped text is what keeps the scan honest.
// This tree is full of `name:name` tokens that are not images — every PromQL
// recording rule under test/e2e/migration/archetypes (`slo:http_error_budget`),
// every Dependabot `version-update:semver-minor`, every `USER nonroot:nonroot`,
// every `host:port` in a datasource URL. A shape-only scan flags all of them.
var imageRefContexts = []*regexp.Regexp{
	regexp.MustCompile(`^\s*-?\s*image:\s*["']?([^"'\s]+)["']?\s*$`),
	regexp.MustCompile(`^\s*FROM\s+(\S+)`),
	regexp.MustCompile(`\bdocker\s+pull\s+(\S+)`),
	regexp.MustCompile(`^[A-Z0-9_]*IMAGE[A-Z0-9_]*\s*:=\s*"([^"]+)"`),
	regexp.MustCompile(`^\s*default:\s*([^"'\s]+)\s*$`),
}

// justAssignment / buildTagArgument together resolve what this repository BUILDS
// rather than pulls. The build targets are all spelled `-t {{SOME_IMAGE}}`, so
// the Justfile's own variable table is what turns the placeholder into the ref
// a `*_IMAGE :=` pin would otherwise contribute to the pulled set.
var (
	justAssignment   = regexp.MustCompile(`(?m)^([A-Z0-9_]+)\s*:=\s*"([^"]*)"`)
	buildTagArgument = regexp.MustCompile(`(?:^|\s)(?:-t|--tag)\s+(\S+)`)
	justPlaceholder  = regexp.MustCompile(`^\{\{\s*([A-Z0-9_]+)\s*\}\}$`)
)

// dockerHubRef is the full shape of a Docker-Hub-resolvable ref. Applied to a
// whole captured token, so a templated value — `${CERBERUS_IMAGE:-…}`,
// `cerberus:dev${COMPOSE_PROJECT_SUFFIX:-}`, every image this repo builds
// itself — fails to match and is skipped, as is a build-stage name (`FROM
// build`) and a digest-pinned ref.
var dockerHubRef = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9._-]*/)*[a-z0-9][a-z0-9._-]*:[A-Za-z0-9][A-Za-z0-9._-]*$`)

// mirrorInventory reads `mirroredImages` out of the module rather than
// duplicating it here: a copy would be one more thing to drift.
func mirrorInventory(t *testing.T) map[string]bool {
	t.Helper()

	src, err := os.ReadFile(mirrorModule)
	if err != nil {
		t.Fatalf("read %s: %v", mirrorModule, err)
	}
	block := mirrorInventoryLiteral.FindStringSubmatch(string(src))
	if block == nil {
		t.Fatalf("%s no longer exports a `mirroredImages` array literal — the guard cannot read the inventory",
			mirrorModule)
	}

	inventory := map[string]bool{}
	for _, m := range inventoryEntry.FindAllStringSubmatch(block[1], -1) {
		inventory[m[1]] = true
	}
	if len(inventory) == 0 {
		t.Fatalf("%s exports an empty `mirroredImages` — every CI pull is back on Docker Hub's shared quota",
			mirrorModule)
	}
	return inventory
}

// isDockerHubImageRef mirrors the rule the docker CLI applies and the one
// `mirroredRef` applies: a first path segment is a REGISTRY HOST exactly when it
// looks like one, i.e. it carries a dot or a port. That is why `minio/mc`
// resolves to Docker Hub while `gcr.io/distroless/static` does not.
func isDockerHubImageRef(ref string) bool {
	slash := strings.Index(ref, "/")
	if slash == -1 {
		return true
	}
	first := ref[:slash]
	return !strings.Contains(first, ".") && !strings.Contains(first, ":")
}

// mirrorScanFiles walks the file kinds that make CI acquire an image: compose
// and Kubernetes YAML, Dockerfiles, the Justfile, every shell script, and the
// workflow / composite-action YAML.
//
// `deploy/helm/` is deliberately absent. The chart's image defaults are
// OPERATOR-facing — an operator must not be pointed at a mirror this project
// maintains for its own CI — and `chart-kubeconform.mjs` probes them against the
// real registry precisely to prove the published tag exists. Vendored upstream
// trees are absent for the same reason the sibling build guard skips them: they
// pin somebody else's shapes.
func mirrorScanFiles(t *testing.T) []string {
	t.Helper()

	const repoRoot = "../.."
	skipDirs := map[string]bool{".git": true, ".claude": true, "node_modules": true, "upstream": true, "vendor": true}

	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasSuffix(slash, "/deploy/helm") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := filepath.Ext(name)
		switch {
		case name == "Justfile":
		case ext == ".sh":
		case ext == ".yml" || ext == ".yaml":
		case strings.HasPrefix(name, "Dockerfile"):
		default:
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	return files
}

// locallyBuiltRefs — the refs this repository builds rather than pulls. A built
// image has no upstream copy to mirror, and `cerberus:e2e` is Docker-Hub-shaped
// in exactly the way an unqualified upstream name is, so the two are separated
// by what the tree DOES with them rather than by how they look.
func locallyBuiltRefs(t *testing.T, files []string) map[string]bool {
	t.Helper()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	vars := map[string]string{}
	for _, m := range justAssignment.FindAllStringSubmatch(string(buf), -1) {
		vars[m[1]] = m[2]
	}

	built := map[string]bool{}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range logicalLines(string(src)) {
			if isProse(line) {
				continue
			}
			for _, m := range buildTagArgument.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if p := justPlaceholder.FindStringSubmatch(target); p != nil {
					target = vars[p[1]]
				}
				built[target] = true
			}
		}
	}
	if len(built) == 0 {
		t.Fatal("no `docker build -t` target found in the tree — the build-target resolver is scanning for a " +
			"shape that no longer exists, so an image this repo builds could be demanded of the mirror")
	}
	return built
}

// upstreamImageRefsInTree — every Docker Hub ref the tree names, mapped to the
// files that name it so a failure says where to look.
func upstreamImageRefsInTree(t *testing.T) map[string][]string {
	t.Helper()

	files := mirrorScanFiles(t)
	built := locallyBuiltRefs(t, files)

	refs := map[string][]string{}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			// Comments name images they do not pull — a bumped-from tag, an
			// incident log, a worked example.
			if isProse(line) {
				continue
			}
			for _, context := range imageRefContexts {
				m := context.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				for _, ref := range strings.Fields(m[1]) {
					if !dockerHubRef.MatchString(ref) || !isDockerHubImageRef(ref) || built[ref] {
						continue
					}
					refs[ref] = append(refs[ref], file)
				}
			}
		}
	}
	return refs
}

// TestMirrorInventoryCoversEveryUpstreamImage pins `mirroredImages` against the
// set of Docker Hub images this tree actually names, in both directions.
func TestMirrorInventoryCoversEveryUpstreamImage(t *testing.T) {
	t.Parallel()

	inventory := mirrorInventory(t)
	found := upstreamImageRefsInTree(t)

	if len(found) < minUpstreamImageRefs {
		t.Fatalf("found only %d upstream image refs in the tree, expected at least %d — the guard is scanning "+
			"for a shape that no longer exists", len(found), minUpstreamImageRefs)
	}

	for _, ref := range sortedKeys(found) {
		if inventory[ref] {
			continue
		}
		t.Errorf("%s is pulled from Docker Hub (%s) but is not in `mirroredImages` (%s), so every CI job that "+
			"acquires it draws on the shared anonymous quota instead of this project's GHCR copy. Add it to the "+
			"inventory; the nightly mirror job copies it on the next run.",
			ref, strings.Join(dedupe(found[ref]), ", "), mirrorModule)
	}

	for _, ref := range sortedSet(inventory) {
		if _, ok := found[ref]; ok {
			continue
		}
		t.Errorf("`mirroredImages` in %s still lists %s, but no file in the tree names it any more. The nightly "+
			"job keeps copying an image nothing pulls, and the inventory stops describing what CI consumes. "+
			"Drop the entry.", mirrorModule, ref)
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
