package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Docker Compose scopes containers, networks and volumes by PROJECT name, and
// two stacks that resolve the same project name are one stack as far as the
// daemon is concerned: `up` adopts the other's containers, `down -v
// --remove-orphans` destroys them. Nothing about that is loud — the second
// checkout's run simply operates on the first checkout's stack and reports
// green.
//
// Every checkout of this repository on one machine resolves the SAME project
// name unless something distinguishes them, and dropping the `name:` keys would
// not distinguish them either: compose's fallback is the basename of the
// directory holding the first `-f` file, which is `loki` / `tempo` /
// `tier1-dual` in every checkout alike. So each compose file spells its project
// name `<stable-base>${COMPOSE_PROJECT_SUFFIX:-}` and one script
// (scripts/compose-project-suffix.sh) derives that variable from the checkout's
// path — empty in a primary checkout and in CI, a short path hash in the linked
// worktrees agents run in.
//
// These pins hold the mechanism at its single root. The `name:` key is honoured
// by every compose invocation whatever runs it, so the guards below are over
// the compose files and the wiring that populates the variable, never over
// individual call sites.
const (
	composeSuffixVar    = "COMPOSE_PROJECT_SUFFIX"
	composeSuffixScript = "scripts/compose-project-suffix.sh"

	// The exact interpolation every project name ends with. The `:-` default is
	// what keeps an unset variable byte-identical to no mechanism at all.
	composeProjectSuffixRef = "${" + composeSuffixVar + ":-}"
)

// composeFileRE matches the compose-file naming shapes the tree uses and the
// ones compose itself defaults to, so a stack added under any of them is
// covered without being listed anywhere. A matching name is only the first
// half of the test: the file also has to declare `services:` to be a compose
// model rather than, say, an OTel collector config that happens to be named
// after the compose stack it configures.
var composeFileRE = regexp.MustCompile(`^(docker-compose|compose)([.-][A-Za-z0-9._-]+)?\.(yml|yaml)$`)

// composeStack is the subset of a compose model these pins read. Parsed from
// the raw YAML rather than from `docker compose config`, because the point is
// what the file DECLARES: an interpolated value would already have been
// resolved away.
type composeStack struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		ContainerName string `yaml:"container_name"`
		Image         string `yaml:"image"`
	} `yaml:"services"`
	Volumes  map[string]composeResource `yaml:"volumes"`
	Networks map[string]composeResource `yaml:"networks"`
}

// composeResource is a top-level volume or network. Its own `name:` overrides
// project scoping the same way `container_name` does for containers.
type composeResource struct {
	Name string `yaml:"name"`
}

// composeFiles returns every compose file in the tree, repo-relative and
// sorted. Nested git checkouts (the vendored `compatibility/prometheus/
// upstream` submodule) are skipped: their compose files are upstream's, not
// this repository's to name.
func composeFiles(t *testing.T) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == repoRoot {
				return nil
			}
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !composeFileRE.MatchString(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if len(readComposeStack(t, rel).Services) == 0 {
			return nil
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	if len(found) == 0 {
		t.Fatal("no compose file in the tree — the guard is scanning for a shape that no longer exists")
	}

	sort.Strings(found)
	return found
}

func readComposeStack(t *testing.T, rel string) composeStack {
	t.Helper()

	buf, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var out composeStack
	if err := yaml.Unmarshal(buf, &out); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return out
}

// composeProjectBase splits a declared project name into its stable base and
// asserts the per-checkout suffix is interpolated onto it. The base is what CI
// and the recipes address; the suffix is what keeps two checkouts apart.
func composeProjectBase(t *testing.T, rel, declared string) string {
	t.Helper()

	base, ok := strings.CutSuffix(declared, composeProjectSuffixRef)
	if !ok {
		t.Fatalf("%s declares `name: %s`, which is the same project in every checkout of this repo. "+
			"Two of them tear down each other's containers, networks and volumes with no error anywhere. "+
			"Spell it `%s%s` so the checkout's own suffix applies.", rel, declared, declared, composeProjectSuffixRef)
	}
	if base == "" {
		t.Fatalf("%s declares a project name that is nothing but the suffix. Without a stable base the "+
			"stack has no name to address it by when the suffix is empty, which is the case in CI.", rel)
	}
	return base
}

// TestComposeProjectNamesCarryTheCheckoutSuffix pins that every stack in the
// tree names itself, and names itself per checkout. A stack whose name is a
// bare literal is shared by every worktree on the machine.
func TestComposeProjectNamesCarryTheCheckoutSuffix(t *testing.T) {
	t.Parallel()

	named := 0
	bases := map[string]string{}
	for _, rel := range composeFiles(t) {
		declared := readComposeStack(t, rel).Name
		if declared == "" {
			// An overlay, merged behind a file that does name the project.
			// TestComposeOverlaysMergeBehindANamedStack holds it to that.
			continue
		}
		named++
		base := composeProjectBase(t, rel, declared)
		if other, dup := bases[base]; dup {
			t.Errorf("%s and %s both declare project base %q, so they are one stack: bringing either up "+
				"adopts the other's containers.", other, rel, base)
		}
		bases[base] = rel
	}

	if named == 0 {
		t.Fatal("no compose file declares a project name — the guard is scanning for a shape that no longer exists")
	}
}

// composeInvocations returns, for every line of the Justfile that names compose
// files, the compose files it names in order. Line continuations are joined
// first, so a multi-line `docker compose -f a \ -f b ...` reads as one
// invocation. Both spellings that carry file lists are covered — the `-f` flags
// of a direct `docker compose` call, and the positional arguments of the
// `_compose-pull-retry` helper — because the invariant is about which file
// comes FIRST, not about how it is passed.
func composeInvocations(t *testing.T, files []string) [][]string {
	t.Helper()

	buf, err := os.ReadFile(filepath.Join(repoRoot, "Justfile"))
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	joined := strings.ReplaceAll(string(buf), "\\\n", " ")

	var out [][]string
	for _, line := range strings.Split(joined, "\n") {
		type mention struct {
			at  int
			rel string
		}
		var mentions []mention
		for _, rel := range files {
			if at := strings.Index(line, rel); at >= 0 {
				mentions = append(mentions, mention{at: at, rel: rel})
			}
		}
		if len(mentions) == 0 {
			continue
		}
		sort.Slice(mentions, func(i, j int) bool { return mentions[i].at < mentions[j].at })
		ordered := make([]string, 0, len(mentions))
		for _, m := range mentions {
			ordered = append(ordered, m.rel)
		}
		out = append(out, ordered)
	}
	return out
}

// TestComposeOverlaysMergeBehindANamedStack pins the one shape allowed to omit
// `name:`: an overlay that is always merged behind another compose file and so
// inherits ITS project — suffix included. The migration lane's Tier 2 is that
// shape, joining Tier 1's running stack rather than minting a second,
// disconnected project.
//
// Compose resolves the project name from the LAST `name:` among the merged
// files, so an overlay that grew a `name:` of its own would rename the merged
// project and would have to re-derive the suffix to stay in step. An overlay
// that is instead invoked FIRST falls back to its own directory basename and
// silently starts a second stack.
//
// Both directions are pinned, and the second one is what holds Tier 2 to Tier
// 1's project no matter which files exist: whatever is merged behind a leading
// file must contribute no `name:` of its own. That guard reads the invocations,
// so it keeps asserting even if the tree stops having a nameless overlay in it.
func TestComposeOverlaysMergeBehindANamedStack(t *testing.T) {
	t.Parallel()

	files := composeFiles(t)
	invocations := composeInvocations(t, files)

	merges := 0
	for _, inv := range invocations {
		if len(inv) < 2 {
			continue
		}
		merges++
		for _, other := range inv[1:] {
			declared := readComposeStack(t, other).Name
			if declared == "" {
				continue
			}
			t.Errorf("%s is merged behind %s (%s) yet declares `name: %s`. Compose takes the project "+
				"from the LAST `name:` among the merged files, so this invocation runs under a project "+
				"the leading stack was not brought up as, and the two halves land in different projects.",
				other, inv[0], strings.Join(inv, " + "), declared)
		}
	}
	if merges == 0 {
		t.Fatal("no Justfile invocation merges two compose files — the guard is scanning for a shape that " +
			"no longer exists")
	}

	overlays := 0
	for _, rel := range files {
		if readComposeStack(t, rel).Name != "" {
			continue
		}
		overlays++

		merged := 0
		for _, inv := range invocations {
			if inv[0] == rel {
				t.Errorf("%s declares no `name:` but is the FIRST compose file of a Justfile invocation "+
					"(%s). First position makes compose fall back to this file's directory basename, "+
					"which is the same in every checkout and is not the stack the overlay belongs to.",
					rel, strings.Join(inv, " + "))
				continue
			}
			for _, other := range inv[1:] {
				if other == rel {
					merged++
				}
			}
		}
		if merged == 0 {
			t.Errorf("%s declares no `name:` and no Justfile invocation merges it behind another compose "+
				"file. Nothing gives it a project, so it would resolve one from its own directory "+
				"basename — shared by every checkout.", rel)
		}
	}

	if overlays == 0 {
		t.Fatal("no compose overlay in the tree — the guard is scanning for a shape that no longer exists")
	}
}

// TestNoComposeResourceEscapesProjectScoping pins that no compose file opts a
// container, network or volume OUT of project scoping. `container_name:`, and
// a `name:` on a top-level network or volume, make the daemon use the literal
// instead of `<project>-<service>-<n>`, so a second copy of the stack collides on
// that literal no matter how the project is named — the project suffix cannot
// save it.
func TestNoComposeResourceEscapesProjectScoping(t *testing.T) {
	t.Parallel()

	scoped := 0
	for _, rel := range composeFiles(t) {
		stack := readComposeStack(t, rel)
		for svc, def := range stack.Services {
			scoped++
			if def.ContainerName != "" {
				t.Errorf("%s: service %s pins `container_name: %s`. That name is global to the daemon, "+
					"so a second checkout running this stack collides on it however the project is "+
					"named. Drop the key and ask compose for the name it assigned.", rel, svc, def.ContainerName)
			}
		}
		for name, def := range stack.Volumes {
			if def.Name != "" {
				t.Errorf("%s: volume %s pins `name: %s`, which leaves the project's namespace. Two "+
					"checkouts would then share one volume — and one `down -v` would wipe the other's data.",
					rel, name, def.Name)
			}
		}
		for name, def := range stack.Networks {
			if def.Name != "" {
				t.Errorf("%s: network %s pins `name: %s`, which leaves the project's namespace, so two "+
					"checkouts' stacks would be wired onto one network.", rel, name, def.Name)
			}
		}
	}

	if scoped == 0 {
		t.Fatal("no compose service in the tree — the guard is scanning for a shape that no longer exists")
	}
}

// composeShellCallers returns every tracked shell script in the tree that
// actually invokes `docker compose`, repo-relative and sorted. Comment lines
// are stripped first, so the scripts that only DESCRIBE the mechanism — this
// derivation script among them — are not mistaken for call sites.
//
// Discovery rather than a list: a harness entrypoint added later is a context
// compose runs in, and a list is exactly the thing that would still read green
// while the new entrypoint shared another checkout's stack.
func composeShellCallers(t *testing.T) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == repoRoot {
				return nil
			}
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".sh" {
			return nil
		}
		buf, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(buf), "\n") {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "#") {
				continue
			}
			if !strings.Contains(code, "docker compose") && !strings.Contains(code, "docker-compose") {
				continue
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			found = append(found, filepath.ToSlash(rel))
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	sort.Strings(found)
	return found
}

// TestLocallyBuiltImageTagsCarryTheCheckoutSuffix pins the other namespace a
// project name does NOT cover: image tags belong to the daemon, not to the
// project. Two checkouts that both build `cerberus:dev` overwrite one another's
// tag, so a stack correctly isolated down to its containers still ends up
// running a server built from a tree it was never pointed at — and the window
// is wide, because the stacks reference the tag by name (`pull_policy: never`,
// no `--build`) long after the build that produced it.
//
// An image reference with no `/` names no registry namespace, which is what a
// tag built into the local daemon looks like; every image this tree pulls is
// namespace-qualified (`grafana/…`, `prom/…`, `ghcr.io/…`), so the two sets do
// not overlap.
func TestLocallyBuiltImageTagsCarryTheCheckoutSuffix(t *testing.T) {
	t.Parallel()

	local := 0
	for _, rel := range composeFiles(t) {
		for svc, def := range readComposeStack(t, rel).Services {
			if def.Image == "" || strings.Contains(def.Image, "/") {
				continue
			}
			local++
			if !strings.Contains(def.Image, composeProjectSuffixRef) {
				t.Errorf("%s: service %s runs locally built image %q, whose tag is global to the daemon. "+
					"A second checkout building the same tag replaces it under this stack's feet. Spell it "+
					"with %s so each checkout builds and runs its own.", rel, svc, def.Image, composeProjectSuffixRef)
			}
		}
	}

	if local == 0 {
		t.Fatal("no compose service runs a locally built image — the guard is scanning for a shape that " +
			"no longer exists")
	}
}

// TestComposeProjectSuffixIsWiredIntoEveryEntrypoint pins that every context a
// compose command runs from populates the variable, and populates it from the
// one derivation script. Two of those contexts are fixed — the Justfile exports
// it to every recipe (and so to every script and Go test a recipe runs), and
// direnv exports it into an interactive dev shell (and so to a bare `docker
// compose up`). The rest are discovered: each standalone harness entrypoint is
// invoked directly by CI and by developers, not only through `just`, so it
// cannot rely on a recipe's environment.
//
// Workflow YAML is deliberately not in the set. A CI checkout is a primary
// clone, where the derivation yields an empty suffix and every project name is
// byte-identical to what it would be with no mechanism at all.
func TestComposeProjectSuffixIsWiredIntoEveryEntrypoint(t *testing.T) {
	t.Parallel()

	callers := composeShellCallers(t)
	if len(callers) == 0 {
		t.Fatal("no shell script in the tree invokes docker compose — the guard is scanning for a shape " +
			"that no longer exists")
	}

	for _, wiring := range append([]string{"Justfile", ".envrc"}, callers...) {
		buf, err := os.ReadFile(filepath.Join(repoRoot, wiring))
		if err != nil {
			t.Fatalf("read %s: %v", wiring, err)
		}
		src := string(buf)
		if !strings.Contains(src, "export "+composeSuffixVar) {
			t.Errorf("%s does not export %s. Without it a compose invocation from that context resolves "+
				"the bare project name and shares its stack with every other checkout.", wiring, composeSuffixVar)
		}
		if !strings.Contains(src, composeSuffixScript) {
			t.Errorf("%s sets %s without going through %s. One derivation is what makes `down` resolve "+
				"the project `up` created.", wiring, composeSuffixVar, composeSuffixScript)
		}
	}
}

// TestComposeSuffixScriptIsTrackedExecutable pins the derivation script's mode
// in the INDEX, not on disk. Every wiring above execs it directly, and a fresh
// clone gets whatever mode git recorded: a script committed 100644 leaves each
// of those call sites failing to start the one thing that isolates the stacks.
func TestComposeSuffixScriptIsTrackedExecutable(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("git", "ls-files", "-s", "--", composeSuffixScript)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %s: %v", composeSuffixScript, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("%s is not tracked. The compose files interpolate %s and every wiring execs this script, "+
			"so a clone would have nothing to run.", composeSuffixScript, composeSuffixVar)
	}

	// git records exactly two file modes; 100755 is the executable one.
	const trackedExecutableMode = "100755"
	if fields[0] != trackedExecutableMode {
		t.Fatalf("%s is tracked with mode %s, want %s — a fresh clone would check it out non-executable "+
			"and every wiring's exec of it would fail.", composeSuffixScript, fields[0], trackedExecutableMode)
	}
}

// suffixScriptBasename is the name the derivation script is committed under in
// the throwaway repository the derivation test builds, so every worktree of it
// checks out its own copy.
const suffixScriptBasename = "compose-project-suffix.sh"

// runSuffixScript runs CHECKOUT's own copy of the derivation script with a
// hermetic environment: no inherited suffix, no user or system git config.
//
// The working directory is deliberately somewhere else entirely. The script
// describes the checkout that holds it, not the directory it was called from —
// `just` runs recipes from a directory of its own choosing and the harness
// scripts cd into their own subtree first — so a run from outside any
// repository has to succeed and still name the right checkout.
func runSuffixScript(t *testing.T, checkout string) string {
	t.Helper()

	cmd := exec.Command(filepath.Join(checkout, suffixScriptBasename))
	cmd.Dir = t.TempDir()
	cmd.Env = append(hermeticGitEnv(t), composeSuffixVar+"=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run %s of %s: %v", suffixScriptBasename, checkout, err)
	}
	return string(out)
}

func hermeticGitEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// TestComposeProjectSuffixDerivation exercises the derivation itself rather
// than its shape: a primary checkout — which is what CI checks out — gets no
// suffix at all, so its project names are byte-identical to what they would be
// without this mechanism; a linked worktree gets one; and the value is a pure
// function of the worktree's path, because `down` runs in a later process than
// `up` and has to name the same project.
func TestComposeProjectSuffixDerivation(t *testing.T) {
	t.Parallel()

	// The repository's own script, committed into a throwaway repository so each
	// worktree of it checks out a copy: the derivation reads the checkout that
	// holds the script, which is what makes it independent of the caller's cwd.
	script, err := os.ReadFile(filepath.Join(repoRoot, composeSuffixScript))
	if err != nil {
		t.Fatalf("read %s: %v", composeSuffixScript, err)
	}

	primary := t.TempDir()
	runGit(t, primary, "init", "-q", ".")
	const executableMode = 0o755
	if err := os.WriteFile(filepath.Join(primary, suffixScriptBasename), script, executableMode); err != nil {
		t.Fatalf("write %s: %v", suffixScriptBasename, err)
	}
	runGit(t, primary, "add", suffixScriptBasename)
	runGit(t, primary, "-c", "user.email=pin@example.invalid", "-c", "user.name=pin",
		"commit", "-q", "-m", "root")

	if got := runSuffixScript(t, primary); got != "" {
		t.Fatalf("a primary checkout derived suffix %q, want empty. CI checks out a primary clone, so a "+
			"non-empty suffix there renames every project, container, network and volume the lanes address.", got)
	}

	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	runGit(t, primary, "worktree", "add", "-q", "--detach", first)
	runGit(t, primary, "worktree", "add", "-q", "--detach", second)

	firstSuffix := runSuffixScript(t, first)
	secondSuffix := runSuffixScript(t, second)

	if firstSuffix == "" {
		t.Fatal("a linked worktree derived an empty suffix, so concurrent agents share one stack — which " +
			"is the collision this mechanism exists to prevent")
	}
	if firstSuffix != runSuffixScript(t, first) {
		t.Fatalf("the suffix for one worktree changed between two runs (%q then %q). `down` would resolve "+
			"a different project than `up` created and leave the stack running.", firstSuffix, runSuffixScript(t, first))
	}
	if firstSuffix == secondSuffix {
		t.Fatalf("two worktrees derived the same suffix %q, so they still share one project", firstSuffix)
	}

	// A project name is `[a-z0-9][a-z0-9_-]*`; the suffix is appended to a base
	// that already satisfies the first character.
	suffixRE := regexp.MustCompile(`^[a-z0-9_-]+$`)
	if !suffixRE.MatchString(firstSuffix) {
		t.Fatalf("derived suffix %q is not a legal compose project-name fragment", firstSuffix)
	}
}

// dockerBinConst is the exported constant the harness names the docker client
// through (test/e2e/migration/lib.DockerBin). A docker invocation spells its
// program either as that constant or as the bare string, and the scan below
// recognises both — so hoisting the literal into a constant, which is what a
// tidy-up would do first, does not blind the guard.
const dockerBinConst = "DockerBin"

// isDockerProgram reports whether e is the program argument of a docker
// invocation.
func isDockerProgram(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING && v.Value == `"docker"`
	case *ast.Ident:
		return v.Name == dockerBinConst
	case *ast.SelectorExpr:
		return v.Sel.Name == dockerBinConst
	}
	return false
}

// dockerProgramArg returns the expression naming the program of an os/exec
// call, and whether the call is one.
func dockerProgramArg(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Command":
		if len(call.Args) > 0 {
			return call.Args[0], true
		}
	case "CommandContext":
		if len(call.Args) > 1 {
			return call.Args[1], true
		}
	}
	return nil, false
}

// assignsEnv reports whether n is an assignment to some value's Env field —
// `cmd.Env = …`, whatever the variable is called.
func assignsEnv(n ast.Node) bool {
	assign, ok := n.(*ast.AssignStmt)
	if !ok {
		return false
	}
	for _, lhs := range assign.Lhs {
		if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
			return true
		}
	}
	return false
}

// goFiles returns every Go file in the tree. Nested git checkouts are skipped
// for the same reason composeFiles skips them: their contents are upstream's.
func goFiles(t *testing.T) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == repoRoot {
				return nil
			}
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) == ".go" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	sort.Strings(found)
	return found
}

// TestNoGoDockerInvocationReplacesTheEnvironment pins that the Go side of the
// tree launches docker only with the environment it inherited. The migration
// lane pauses and unpauses containers through `docker compose -f <tier-1
// file>`, and it is the inherited COMPOSE_PROJECT_SUFFIX that decides WHICH
// checkout's ClickHouse gets paused: an invocation handed a constructed
// environment resolves the unsuffixed project, and the fault lands in whichever
// checkout owns that one.
//
// The scan is over the parsed syntax, not the source text, so it holds the
// invariant rather than a spelling of it — renaming the variable, hoisting the
// program name into a constant or moving the call to another package all keep
// the guard pointed at the same thing. It is repo-wide because the next docker
// call site is the one nobody remembers to wire up.
func TestNoGoDockerInvocationReplacesTheEnvironment(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	invocations := 0
	for _, path := range goFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			t.Fatalf("relativise %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			var dockerAt, envAt token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if prog, isExec := dockerProgramArg(call); isExec && isDockerProgram(prog) && !dockerAt.IsValid() {
						dockerAt = call.Pos()
					}
				}
				if assignsEnv(n) && !envAt.IsValid() {
					envAt = n.Pos()
				}
				return true
			})

			if !dockerAt.IsValid() {
				continue
			}
			invocations++
			if envAt.IsValid() {
				t.Errorf("%s: %s runs docker (%s) and sets Env (%s). A constructed environment drops the "+
					"inherited %s, so compose resolves the unsuffixed project and the command lands in "+
					"whichever checkout's stack owns it. Leave Env nil so the child inherits.",
					rel, fn.Name.Name, fset.Position(dockerAt), fset.Position(envAt), composeSuffixVar)
			}
		}
	}

	if invocations == 0 {
		t.Fatal("no Go code in the tree runs docker — the guard is scanning for a shape that no longer exists")
	}
}
