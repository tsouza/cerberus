package regression

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// manualGoldenUpdateModule is update-golden.yml's trusted controller: its
// MODE=plan builds the regeneration matrix the workflow fans out over, and its
// MODE=package cuts the patch a leg uploads for the publisher.
const manualGoldenUpdateModule = ".github/scripts/manual-golden-update.mjs"

// The branch the reproduction regenerates, and the default branch it diverged
// from. The plan refuses a target equal to the default branch.
const (
	unclassifiedSiteBranch  = "fix/new-rejection-site"
	unclassifiedSiteDefault = "main"
)

// unclassifiedSiteRequestedShard is the shard the reproduction's dispatch
// names. Any body shard works: what matters is that the plan gives its leg the
// `parity` shard as predecessor context, because the branch touched a lowering
// package the rejection catalogue is derived from.
const unclassifiedSiteRequestedShard = "chsql"

// unclassifiedSiteFile and unclassifiedSiteAnchor locate where the
// reproduction inserts its new rejection site: right after an existing,
// already-classified guard in the PromQL lowering.
const (
	unclassifiedSiteFile   = "internal/promql/absent.go"
	unclassifiedSiteAnchor = "\t\treturn nil, fmt.Errorf(\"promql: absent() expects 1 argument, got %d\", len(c.Args))\n\t}\n"
)

// unclassifiedSiteMessage is the new site's rejection message, and
// unclassifiedSiteGuard the source the reproduction adds to construct it. The
// guard is unreachable in practice; the catalogue scan is syntactic, so it
// records the site all the same.
const (
	unclassifiedSiteMessage = "promql: absent() reproduction site for an unclassified rejection"
	unclassifiedSiteGuard   = "\tif c.PosRange.Start < 0 {\n\t\treturn nil, fmt.Errorf(\"" +
		unclassifiedSiteMessage + "\")\n\t}\n"
)

// unclassifiedSiteKeyPrefix is the stable part of the new site's catalogue
// key; the suffix after `#` hashes the message.
const unclassifiedSiteKeyPrefix = "internal/promql/absent.go:lowerAbsent#"

// unclassifiedSiteShard is the catalogue shard the new entry lands in, one
// shard per lowering source file and function.
const unclassifiedSiteShard = "test/rejection-parity/catalogue/internal__promql__absent.go__lowerAbsent.json"

// classificationGuardTest is the curation gate that refuses an unclassified
// catalogue entry.
const classificationGuardTest = "TestCatalogueEntriesAreClassified"

// TestUpdateGoldenRegeneratesPastAnUnclassifiedRejectionSite reproduces the
// update-golden.yml run on a branch that adds a new rejection site.
//
// The workflow gives every body-shard leg the `parity` shard as predecessor
// context. That shard writes the new site into the rejection catalogue
// unclassified, because classification is a deliberate manual step. When the
// regeneration recipe also asserted classification, the predecessor failed in
// every leg, no leg uploaded a patch, and the publisher never ran: a branch
// carrying a new site could regenerate nothing at all through the workflow.
//
// The test copies the working tree into a scratch repository, commits a new
// rejection site on a branch, asks the workflow's own plan for the matrix,
// and runs the predecessor stage of a body-shard leg exactly as the leg runs
// it. It then asserts all three halves of the contract:
//
//  1. the regeneration succeeds and the leg's patch carries the new entry,
//     still unclassified;
//  2. the output names the entry, the shard file holding it, and how to
//     classify it;
//  3. the classification gate still refuses the entry, so classification
//     stays manual.
func TestUpdateGoldenRegeneratesPastAnUnclassifiedRejectionSite(t *testing.T) {
	t.Parallel()

	src, err := filepath.Abs(repoRootFromRegression)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	repo := copyWorkingTree(t, src)

	scratchGit(t, repo, "init", "-q", "-b", unclassifiedSiteDefault)
	scratchGit(t, repo, "add", "-A")
	scratchGit(t, repo, "commit", "-q", "--no-verify", "-m", "base")
	scratchGit(t, repo, "update-ref", "refs/remotes/origin/"+unclassifiedSiteDefault, "HEAD")
	scratchGit(t, repo, "checkout", "-q", "-b", unclassifiedSiteBranch)
	addUnclassifiedSite(t, repo)
	scratchGit(t, repo, "commit", "-q", "--no-verify", "-am", "add a rejection site")

	leg := planLegWithParityPredecessor(t, repo)

	// The leg's first stage, exactly as the workflow's "Regenerate shard with
	// predecessor context" step runs it. A later stage never starts once this
	// one fails, so this is the step that decided the reported run.
	regen := exec.Command("just", "update-golden", parityShard)
	regen.Dir = repo
	regen.Env = append(scratchEnv(), "GOLDEN_UPDATE_BASE_REF=HEAD", "UPDATE_GOLDEN_WORKFLOW=1")
	out, code := combinedOutput(t, regen)
	if code != 0 {
		t.Fatalf("the %q predecessor of the %q leg (command shards %q) failed with exit %d on a "+
			"branch that only adds a new rejection site. Every leg runs that predecessor, so no "+
			"shard regenerates and the publisher never runs.\n%s",
			parityShard, leg.Shard, leg.CommandShards, code, out)
	}

	for _, want := range []string{unclassifiedSiteKeyPrefix, unclassifiedSiteMessage, unclassifiedSiteShard, classificationGuardTest} {
		if !strings.Contains(out, want) {
			t.Errorf("the regeneration output does not mention %q. It must name the entry that "+
				"needs classification, the file holding it, and how to classify it.\n%s", want, out)
		}
	}

	if class, ok := catalogueClassOf(t, filepath.Join(repo, unclassifiedSiteShard), unclassifiedSiteMessage); !ok {
		t.Errorf("%s gained no entry for %q; the regeneration did not record the new site",
			unclassifiedSiteShard, unclassifiedSiteMessage)
	} else if class != "" {
		t.Errorf("the new site's entry was written with class %q. Classification is a manual "+
			"step; regeneration must leave a new entry unclassified.", class)
	}

	patch := filepath.Join(t.TempDir(), parityShard+".patch")
	pkg := exec.Command("node", manualGoldenUpdateModule)
	pkg.Dir = repo
	pkg.Env = append(
		scratchEnv(),
		"MODE=package",
		"TARGET_ROOT=.",
		"SHARD="+parityShard,
		"ALLOWED_SHARDS="+parityShard,
		"OUTPUT_PATH="+patch,
	)
	if pkgOut, pkgCode := combinedOutput(t, pkg); pkgCode != 0 {
		t.Fatalf("packaging the %q leg's patch failed with exit %d:\n%s", parityShard, pkgCode, pkgOut)
	}
	body, err := os.ReadFile(patch)
	if err != nil {
		t.Fatalf("read %s: %v", patch, err)
	}
	if !strings.Contains(string(body), unclassifiedSiteMessage) {
		t.Errorf("the %q leg's patch does not carry the new catalogue entry; the publisher "+
			"would push a catalogue missing the site.\n%s", parityShard, body)
	}

	guard := exec.Command("go", "test", "-count=1", "-run", "^"+classificationGuardTest+"$", "./test/rejection-parity/")
	guard.Dir = repo
	guard.Env = scratchEnv()
	guardOut, guardCode := combinedOutput(t, guard)
	if guardCode == 0 || !strings.Contains(guardOut, unclassifiedSiteKeyPrefix) {
		t.Errorf("%s passed (exit %d) or did not name the new site with an unclassified entry "+
			"in the catalogue. Classification must stay manual: the gate has to refuse the entry "+
			"until someone classifies it.\n%s", classificationGuardTest, guardCode, guardOut)
	}
}

// planMatrixRow is one row of the regeneration matrix the plan emits.
type planMatrixRow struct {
	Shard         string `json:"shard"`
	CommandShards string `json:"command_shards"`
}

// planLegWithParityPredecessor runs the workflow's plan for the reproduction's
// dispatch and returns a non-parity leg that runs `parity` as predecessor.
func planLegWithParityPredecessor(t *testing.T, repo string) planMatrixRow {
	t.Helper()

	outputs := filepath.Join(t.TempDir(), "github_output")
	plan := exec.Command("node", manualGoldenUpdateModule)
	plan.Dir = repo
	plan.Env = append(
		scratchEnv(),
		"MODE=plan",
		"TARGET_ROOT=.",
		"BRANCH="+unclassifiedSiteBranch,
		"SHARDS_INPUT="+unclassifiedSiteRequestedShard,
		"DEFAULT_BRANCH="+unclassifiedSiteDefault,
		"WORKFLOW_REF=refs/heads/"+unclassifiedSiteDefault,
		"PUSH_TOKEN_SET=true",
		"GITHUB_OUTPUT="+outputs,
	)
	if out, code := combinedOutput(t, plan); code != 0 {
		t.Fatalf("the workflow plan failed with exit %d:\n%s", code, out)
	}
	raw, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatalf("read plan outputs: %v", err)
	}

	var matrix struct {
		Include []planMatrixRow `json:"include"`
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "matrix="); ok {
			if err := json.Unmarshal([]byte(value), &matrix); err != nil {
				t.Fatalf("parse plan matrix %q: %v", value, err)
			}
		}
	}
	for _, row := range matrix.Include {
		if row.Shard != parityShard && strings.Contains(" "+row.CommandShards+" ", " "+parityShard+" ") {
			return row
		}
	}
	t.Fatalf("no leg of the plan runs %q as predecessor context, so this reproduction no longer "+
		"matches the workflow it pins. Plan outputs:\n%s", parityShard, raw)

	return planMatrixRow{}
}

// addUnclassifiedSite inserts the reproduction's new rejection site.
func addUnclassifiedSite(t *testing.T, repo string) {
	t.Helper()

	path := filepath.Join(repo, unclassifiedSiteFile)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Count(string(src), unclassifiedSiteAnchor) != 1 {
		t.Fatalf("%s no longer holds exactly one copy of the anchor this reproduction inserts "+
			"its site after:\n%s", unclassifiedSiteFile, unclassifiedSiteAnchor)
	}
	patched := strings.Replace(string(src), unclassifiedSiteAnchor, unclassifiedSiteAnchor+unclassifiedSiteGuard, 1)
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// catalogueClassOf returns the class of the entry carrying message in one
// catalogue shard, and whether such an entry exists.
func catalogueClassOf(t *testing.T, shard, message string) (string, bool) {
	t.Helper()

	raw, err := os.ReadFile(shard)
	if err != nil {
		t.Fatalf("read %s: %v", shard, err)
	}
	var doc struct {
		Entries []struct {
			Message string `json:"message"`
			Class   string `json:"class"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", shard, err)
	}
	for _, e := range doc.Entries {
		if e.Message == message {
			return e.Class, true
		}
	}

	return "", false
}

// copyWorkingTree copies every tracked and untracked-but-not-ignored file of
// src into a fresh directory, so the reproduction sees the working tree as it
// stands rather than only what is committed.
func copyWorkingTree(t *testing.T, src string) string {
	t.Helper()

	list := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	list.Dir = src
	raw, err := list.Output()
	if err != nil {
		t.Fatalf("list files of %s: %v", src, err)
	}
	dst := t.TempDir()
	for rel := range strings.SplitSeq(string(raw), "\x00") {
		if rel == "" {
			continue
		}
		copyTreeEntry(t, filepath.Join(src, rel), filepath.Join(dst, rel))
	}

	return dst
}

// copyTreeEntry copies one regular file or symbolic link. A path listed by git
// but deleted from the working tree is skipped, as the working tree has it; so
// is a submodule checkout, which git lists as one directory entry and which no
// parity generator reads.
func copyTreeEntry(t *testing.T, from, to string) {
	t.Helper()

	info, err := os.Lstat(from)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("stat %s: %v", from, err)
	}
	if info.IsDir() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", to, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(from)
		if err != nil {
			t.Fatalf("readlink %s: %v", from, err)
		}
		if err := os.Symlink(target, to); err != nil {
			t.Fatalf("symlink %s: %v", to, err)
		}
		return
	}
	in, err := os.Open(from)
	if err != nil {
		t.Fatalf("open %s: %v", from, err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		t.Fatalf("create %s: %v", to, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy %s: %v", from, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", to, err)
	}
}

// scratchEnv is the caller's environment with a fixed git identity and no
// global or system git configuration, so the scratch repository commits the
// same way on every host. HOME is kept: the Go build and module caches live
// under it.
func scratchEnv() []string {
	return append(
		os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=regression",
		"GIT_AUTHOR_EMAIL=regression@example.invalid",
		"GIT_COMMITTER_NAME=regression",
		"GIT_COMMITTER_EMAIL=regression@example.invalid",
	)
}

// scratchGit runs one git command in the scratch repository.
func scratchGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = scratchEnv()
	if out, code := combinedOutput(t, cmd); code != 0 {
		t.Fatalf("git %s in %s exited %d:\n%s", strings.Join(args, " "), dir, code, out)
	}
}

// combinedOutput runs cmd and returns its combined output and exit code. A
// command that could not start at all fails the test.
func combinedOutput(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run %s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
	}

	return string(out), exitErr.ExitCode()
}
