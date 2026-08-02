package regression

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A build's `FROM` is the one image acquisition the GHCR mirror could not reach.
//
// `pullImageWithRetry` serves a mirrored copy by pulling it and re-tagging it to
// the upstream name in the HOST daemon. That covers a compose `image:` key and a
// `docker pull`, and covers nothing about a `FROM`: the `docker-container`
// BuildKit driver `.github/actions/setup-buildx` installs runs BuildKit inside a
// container with its own content store and resolves `FROM` against the registry
// no matter what the host holds. So `golang:1.26` sat in the mirror inventory
// while every build in the tree went on resolving it from Docker Hub — the
// mirror present and inert on the exact ref that opened the incident:
//
//	#5 ERROR: unexpected status from HEAD request to
//	   https://registry-1.docker.io/v2/library/golang/manifests/1.26:
//	   429 Too Many Requests
//
// The only lever on a `FROM` is the ref itself, so the ref is a build arg and
// `build-with-registry-retry.mjs` — the single command every build already runs
// through — resolves it mirror-first and exports it.
//
// The regression shape is a Dockerfile that goes back to naming a Docker Hub
// image directly in a `FROM`. It reintroduces the incident silently: the build
// keeps working, on every machine, right up until the shared quota is out. So
// the guards below are on the SHAPE — no Dockerfile this repo builds may name a
// metered image in a `FROM`, every build site must pass the arg, and the table
// pairing an arg with its upstream ref must agree with both the Dockerfiles and
// the mirror inventory. A guard naming the three files that happened to have the
// defect would not survive the fourth.
const (
	treeRoot = "../.."

	// Floors for the discovery scans. There are 3 in-scope Dockerfiles, 10
	// in-scope compose build stanzas and 3 direct `docker build` invocations
	// today; each floor sits below its real count, so a refactor that leaves a
	// guard scanning for a shape nothing matches fails instead of passing
	// vacuously.
	minBaseImageDockerfiles   = 3
	minComposeBuildStanzas    = 8
	minDirectBuildInvocations = 2
)

var (
	// `ARG NAME=default` — the declaration that makes a `FROM` substitutable.
	dockerfileArgDecl = regexp.MustCompile(`(?m)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)=(\S+)\s*$`)
	// The image ref of a `FROM`, before any `AS <stage>`.
	dockerfileFrom = regexp.MustCompile(`(?m)^\s*FROM\s+(\S+)`)
	// `${NAME}` or `$NAME` as the whole ref.
	dockerfileFromArg = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)
	// The `buildBaseImageArgs` table in lib/mirror.mjs.
	buildArgTableLiteral = regexp.MustCompile(`(?s)export const buildBaseImageArgs = Object\.freeze\(\{(.*?)\}\);`)
	buildArgTableEntry   = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:\s*['"]([^'"]+)['"]`)
	// A `-f <path>` / `--file <path>` naming the Dockerfile a direct build uses.
	dockerBuildDashF = regexp.MustCompile(`(?:-f|--file)[= ]+(\S+)`)
)

// buildBaseImageArgs reads the arg/ref table out of lib/mirror.mjs rather than
// restating it, so this guard and the JS the wrapper actually runs cannot
// disagree about which args exist.
func buildBaseImageArgs(t *testing.T) map[string]string {
	t.Helper()

	src, err := os.ReadFile(mirrorModule)
	if err != nil {
		t.Fatalf("read %s: %v", mirrorModule, err)
	}
	body := buildArgTableLiteral.FindStringSubmatch(string(src))
	if body == nil {
		t.Fatalf("%s no longer exports a `buildBaseImageArgs` object literal — the guard cannot read the arg "+
			"table, and without one every build silently reverts to resolving Docker Hub", mirrorModule)
	}

	out := map[string]string{}
	for _, m := range buildArgTableEntry.FindAllStringSubmatch(body[1], -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("`buildBaseImageArgs` in %s is empty, so no build arg points any `FROM` at the mirror",
			mirrorModule)
	}
	return out
}

// meteredFromRef reports the Docker-Hub-resolvable image a `FROM` names, or ""
// when the ref is not one: an arg-indirected ref, a build stage by name
// (`FROM build`), `FROM scratch`, or a ref on a registry with its own quota
// (`gcr.io/distroless/static`). A digest suffix is stripped first — pinning by
// digest fixes WHAT is pulled, not WHERE from, so a digest-pinned Hub ref is
// metered exactly like a tagged one.
func meteredFromRef(ref string) string {
	if strings.HasPrefix(ref, "$") {
		return ""
	}
	if at := strings.Index(ref, "@"); at != -1 {
		ref = ref[:at]
	}
	if !strings.Contains(ref, ":") {
		return "" // no tag: a stage name, `scratch`, or a degenerate ref
	}
	if !isDockerHubImageRef(ref) {
		return ""
	}
	return ref
}

// composeBuildStanza is one `build:` block: which Dockerfile it builds, and
// which build args it passes.
type composeBuildStanza struct {
	file       string
	line       int
	dockerfile string
	args       map[string]bool
}

// composeBuildStanzas parses every `build:` block in the tree's compose files.
// A hand-rolled indentation walk rather than a YAML dependency: the shape being
// asserted IS the indentation — an `args:` block nested under the right
// `build:` — and this package is dependency-light on purpose.
func composeBuildStanzas(t *testing.T) []composeBuildStanza {
	t.Helper()

	var out []composeBuildStanza
	for _, file := range mirrorScanFiles(t) {
		if !strings.Contains(filepath.Base(file), "compose") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) != "build:" {
				continue
			}
			stanza := composeBuildStanza{file: file, line: i + 1, args: map[string]bool{}}
			buildIndent := indentOf(line)
			argsIndent := -1
			for _, child := range lines[i+1:] {
				trimmed := strings.TrimSpace(child)
				if trimmed == "" {
					continue
				}
				if indentOf(child) <= buildIndent {
					break
				}
				if argsIndent >= 0 && indentOf(child) > argsIndent {
					if !strings.HasPrefix(trimmed, "#") {
						stanza.args[strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[0])] = true
					}
					continue
				}
				argsIndent = -1
				if trimmed == "args:" {
					argsIndent = indentOf(child)
					continue
				}
				if rest, ok := strings.CutPrefix(trimmed, "dockerfile:"); ok {
					stanza.dockerfile = strings.Trim(strings.TrimSpace(rest), `"'`)
				}
			}
			out = append(out, stanza)
		}
	}
	return out
}

// directBuildSite is one `docker build -f <path>` outside compose.
type directBuildSite struct {
	file       string
	line       string
	dockerfile string
}

// directBuildSites finds every `docker build` in the tree that names its
// Dockerfile explicitly, reusing the same scan and prose filter as the sibling
// guard that pins those builds to the retry wrapper.
func directBuildSites(t *testing.T) []directBuildSite {
	t.Helper()

	var out []directBuildSite
	for _, file := range buildScanFiles(t) {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range logicalLines(string(src)) {
			if isProse(line) || !dockerBuildCommand.MatchString(line) {
				continue
			}
			if m := dockerBuildDashF.FindStringSubmatch(line); m != nil {
				out = append(out, directBuildSite{file: file, line: line, dockerfile: m[1]})
			}
		}
	}
	return out
}

// dockerfilesThisRepoBuilds is the in-scope set, derived rather than listed: a
// Dockerfile is in scope exactly when some build site in this tree names it.
// That is what keeps the vendored upstream snapshot under
// compatibility/tempo/upstream/ out of scope — nothing builds it — WITHOUT an
// exclusion list, which would also hide a real Dockerfile added there later.
func dockerfilesThisRepoBuilds(t *testing.T) map[string][]string {
	t.Helper()

	out := map[string][]string{}
	for _, stanza := range composeBuildStanzas(t) {
		if stanza.dockerfile != "" {
			out[stanza.dockerfile] = append(out[stanza.dockerfile], stanza.file)
		}
	}
	for _, site := range directBuildSites(t) {
		out[site.dockerfile] = append(out[site.dockerfile], site.file)
	}
	return out
}

// baseImageArgsUsedBy maps each in-scope Dockerfile to the build args its own
// `FROM` lines resolve through — the args every build site of that file must
// pass.
func baseImageArgsUsedBy(t *testing.T, table map[string]string) map[string][]string {
	t.Helper()

	needs := map[string][]string{}
	for dockerfile := range dockerfilesThisRepoBuilds(t) {
		src, err := os.ReadFile(filepath.Join(treeRoot, dockerfile))
		if err != nil {
			continue // reported by TestBuiltDockerfilesNameNoMeteredImageDirectly
		}
		for _, m := range dockerfileFrom.FindAllStringSubmatch(string(src), -1) {
			name := dockerfileFromArg.FindStringSubmatch(m[1])
			if name == nil {
				continue
			}
			if _, known := table[name[1]]; known {
				needs[dockerfile] = append(needs[dockerfile], name[1])
			}
		}
	}
	return needs
}

// TestBuiltDockerfilesNameNoMeteredImageDirectly guards the regression shape
// itself. A `FROM golang:1.26` builds fine everywhere and reintroduces the 429
// the moment the shared quota runs out — invisible at the only time anyone would
// think to look for it.
func TestBuiltDockerfilesNameNoMeteredImageDirectly(t *testing.T) {
	t.Parallel()

	table := buildBaseImageArgs(t)
	scanned := 0

	for dockerfile, sites := range dockerfilesThisRepoBuilds(t) {
		src, err := os.ReadFile(filepath.Join(treeRoot, dockerfile))
		if err != nil {
			t.Errorf("%s is built by %s but cannot be read: %v", dockerfile, strings.Join(sites, ", "), err)
			continue
		}
		scanned++

		declared := map[string]string{}
		for _, m := range dockerfileArgDecl.FindAllStringSubmatch(string(src), -1) {
			declared[m[1]] = m[2]
		}

		for _, m := range dockerfileFrom.FindAllStringSubmatch(string(src), -1) {
			ref := m[1]
			if metered := meteredFromRef(ref); metered != "" {
				t.Errorf("%s names the Docker Hub image %s directly in a FROM (built by %s). BuildKit's "+
					"docker-container driver resolves that against the registry with its own content "+
					"store, so no host-side pre-pull and no mirror can cover it — every build of this file "+
					"spends the shared anonymous quota. Declare `ARG <NAME>=%s`, write `FROM ${<NAME>}`, "+
					"and add the pair to `buildBaseImageArgs` in %s.",
					dockerfile, metered, strings.Join(sites, ", "), metered, mirrorModule)
				continue
			}
			name := dockerfileFromArg.FindStringSubmatch(ref)
			if name == nil {
				continue // a build stage by name, `scratch`, or an unmetered registry
			}

			upstream, known := table[name[1]]
			if !known {
				t.Errorf("%s resolves a FROM through ${%s}, but `buildBaseImageArgs` in %s has no entry for "+
					"it, so the build wrapper never sets it and every build takes the ARG default — the "+
					"mirror is bypassed silently.", dockerfile, name[1], mirrorModule)
				continue
			}
			def, ok := declared[name[1]]
			if !ok {
				t.Errorf("%s uses ${%s} in a FROM without declaring `ARG %s=%s`. With no default, a plain "+
					"`docker build` outside CI resolves an empty ref.", dockerfile, name[1], name[1], upstream)
				continue
			}
			if def != upstream {
				t.Errorf("%s declares `ARG %s=%s` but `buildBaseImageArgs` pairs %s with %s. The default is "+
					"what someone outside this repo builds from — the mirror packages are private — so the "+
					"two must name the same upstream image.", dockerfile, name[1], def, name[1], upstream)
			}
		}
	}

	if scanned < minBaseImageDockerfiles {
		t.Errorf("only %d built Dockerfiles found (expected at least %d) — the discovery scan has stopped "+
			"matching build sites, so this guard is asserting nothing", scanned, minBaseImageDockerfiles)
	}
}

// TestEveryBuildSitePassesTheBaseImageArg pins the other half. Indirecting the
// Dockerfile achieves nothing on its own: a build site that passes no arg gets
// the ARG default, which is the upstream ref, which is the original defect with
// extra steps.
func TestEveryBuildSitePassesTheBaseImageArg(t *testing.T) {
	t.Parallel()

	table := buildBaseImageArgs(t)
	needs := baseImageArgsUsedBy(t, table)

	stanzas := 0
	for _, stanza := range composeBuildStanzas(t) {
		required, inScope := needs[stanza.dockerfile]
		if !inScope {
			continue
		}
		stanzas++
		for _, arg := range required {
			if !stanza.args[arg] {
				t.Errorf("%s:%d builds %s but its `build:` passes no `%s` arg, so the build takes the ARG "+
					"default and resolves Docker Hub. Add `args:` with `%s: \"${%s:-%s}\"` — the `:-` "+
					"default keeps a plain `docker compose build` working for anyone without mirror "+
					"credentials.", stanza.file, stanza.line, stanza.dockerfile, arg, arg, arg, table[arg])
			}
		}
	}
	if stanzas < minComposeBuildStanzas {
		t.Errorf("only %d in-scope compose build stanzas found (expected at least %d) — the parser has "+
			"stopped matching, so this half is asserting nothing", stanzas, minComposeBuildStanzas)
	}

	direct := 0
	for _, site := range directBuildSites(t) {
		required, inScope := needs[site.dockerfile]
		if !inScope {
			continue
		}
		direct++
		for _, arg := range required {
			if !strings.Contains(site.line, "--build-arg "+arg) {
				t.Errorf("%s builds %s without `--build-arg %s`, so the build takes the ARG default and "+
					"resolves Docker Hub:\n\t%s\nThe valueless form is deliberate — docker reads the value "+
					"from the environment, which build-with-registry-retry.mjs has already resolved "+
					"mirror-first.", site.file, site.dockerfile, arg, strings.TrimSpace(site.line))
			}
		}
	}
	if direct < minDirectBuildInvocations {
		t.Errorf("only %d in-scope direct `docker build` invocations found (expected at least %d) — the "+
			"scan has stopped matching, so this half is asserting nothing", direct, minDirectBuildInvocations)
	}
}

// TestBuildWrapperResolvesBaseImagesThroughTheMirror is the behavioural half of
// the pin. Everything above is structure — the arg exists, the Dockerfile takes
// it, the build sites pass it. None of that says the wrapper puts the MIRROR ref
// in it, and a wrapper that exported the upstream ref would satisfy every one of
// those guards while leaving the incident exactly where it was.
//
// Three outcomes, because the fallback matters as much as the hit: a runner that
// cannot read the mirror — a fork, a local machine, this repo before the mirror
// is populated — must still build, from Docker Hub, rather than fail on a
// private package it was never going to be able to pull.
func TestBuildWrapperResolvesBaseImagesThroughTheMirror(t *testing.T) {
	t.Parallel()

	const arg = "GO_IMAGE"
	upstream := buildBaseImageArgs(t)[arg]
	if upstream == "" {
		t.Fatalf("`buildBaseImageArgs` has no %s entry", arg)
	}
	mirrored := mirroredRefOf(t, upstream)
	const operatorOverride = "golang:1.26-alpine"

	cases := []struct {
		name string
		// probeHits is whether `docker buildx imagetools inspect` succeeds,
		// i.e. whether this runner can read the mirror.
		probeHits bool
		preset    string
		want      string
	}{
		{
			name:      "a runner that can read the mirror builds from the mirror",
			probeHits: true,
			want:      mirrored,
		},
		{
			name:      "a runner that cannot read the mirror still builds, from upstream",
			probeHits: false,
			want:      upstream,
		},
		{
			name:      "a ref the caller set wins over the mirror",
			probeHits: true,
			preset:    operatorOverride,
			want:      operatorOverride,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			seenPath := filepath.Join(dir, "seen")
			writeStubDockerRecordingBuildArgEnv(t, dir, seenPath, arg, tc.probeHits)

			cmd := exec.Command("node", buildRetryWrapperPath,
				"docker", "build", "--build-arg", arg, "-f", "Dockerfile.local", "-t", "cerberus:e2e", ".")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if tc.preset != "" {
				cmd.Env = append(cmd.Env, arg+"="+tc.preset)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run %s: %v\noutput:\n%s", buildRetryWrapper, err, out)
			}

			seen, err := os.ReadFile(seenPath)
			if err != nil {
				t.Fatalf("the build never ran, so nothing recorded what %s it saw: %v", arg, err)
			}
			if got := strings.TrimSpace(string(seen)); got != tc.want {
				t.Errorf("the build saw %s=%q, want %q\noutput:\n%s", arg, got, tc.want, out)
			}
		})
	}
}

// mirroredRefOf asks lib/mirror.mjs what the mirrored ref of an image is,
// rather than restating the naming rule here. A copy of the rule would pass this
// test while disagreeing with the mirror the workflows actually publish to.
func mirroredRefOf(t *testing.T, image string) string {
	t.Helper()

	abs, err := filepath.Abs(mirrorModule)
	if err != nil {
		t.Fatalf("resolve %s: %v", mirrorModule, err)
	}
	out, err := exec.Command("node", "--input-type=module", "-e",
		"import { mirroredRef } from "+strconv.Quote("file://"+abs)+";"+
			"process.stdout.write(String(mirroredRef("+strconv.Quote(image)+")));").Output()
	if err != nil {
		t.Fatalf("ask %s for the mirrored ref of %s: %v", mirrorModule, image, err)
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" || ref == "null" {
		t.Fatalf("`mirroredRef(%q)` is %q — the image is not in `mirroredImages`, so no build of it can "+
			"reach the mirror", image, ref)
	}
	return ref
}

// writeStubDockerRecordingBuildArgEnv drops a `docker` onto dir that answers the
// wrapper's mirror probe as probeHits dictates and, on a build, records the
// value of the named build arg it inherited from the environment.
func writeStubDockerRecordingBuildArgEnv(t *testing.T, dir, seenPath, arg string, probeHits bool) {
	t.Helper()

	probeExit := "1"
	if probeHits {
		probeExit = "0"
	}
	script := strings.Join([]string{
		"#!/bin/sh",
		`[ "$1" = buildx ] && exit ` + probeExit,
		// `--build-arg NAME` with no value is docker's own "take it from the
		// environment" form, so the environment IS the channel under test.
		"printf '%s' \"$" + arg + "\" > " + shellQuote(seenPath),
		"exit 0",
		"",
	}, "\n")

	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
}

// TestBaseImageArgsAreMirrored closes the loop: an arg pointing at an image the
// mirror was never told about resolves upstream anyway, because `mirroredRef`
// returns null outside the inventory. The indirection would be present, wired,
// tested — and inert.
func TestBaseImageArgsAreMirrored(t *testing.T) {
	t.Parallel()

	inventory := mirrorInventory(t)
	for arg, image := range buildBaseImageArgs(t) {
		if !inventory[image] {
			t.Errorf("`buildBaseImageArgs` pairs %s with %s, which is absent from `mirroredImages` — "+
				"`mirroredRef` returns null for it, so the wrapper resolves the upstream ref and every "+
				"build still spends the Docker Hub quota.", arg, image)
		}
	}
}
