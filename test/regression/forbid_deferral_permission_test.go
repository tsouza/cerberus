package regression

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// GitHub answers a resource the caller may not read with 404, never 403 —
// confirming a 403 would confirm the resource exists, which it declines to do.
// So "#1431 is not a number in this repository" and "this token may not read
// issues at all" reach `forbid-deferral` as the same status code, and the gate
// used to map both to "the marker cites nothing", i.e. to an accusation against
// the author's prose.
//
// That is not a thought experiment. PR #1413's `forbid-deferral` run resolved
// zero of two genuinely open citations and reported an untracked violation; the
// same module over the same commit range and the same description resolved both
// locally. The one variable was the token: that workflow version had no
// `issues: read` grant. The instance self-healed when the grant landed on
// `main`, which is exactly what makes it worth pinning — the gate is one
// `permissions:` edit, one expired credential or one fork-scoped token away
// from telling every author that the issues they filed do not exist, and its
// output would say nothing about the real cause.
//
// The tests below drive the real module end to end — a real git range, a real
// `node` process, a stub GitHub API — because the conflation lived in the
// runner half, not in the pure scanners the `node --test` guard covers. Each
// case fixes the API's answers and asserts on the verdict AND on which
// diagnosis the run printed: a gate that fails for the wrong reason sends the
// author to fix prose that was already correct while the real fault stays
// hidden, which is worse than a gate that does not fail at all.
const (
	forbidDeferralModule = "../../.github/scripts/forbid-deferral.mjs"

	// The repository slug the stub API serves. It is the real one because the
	// module resolves issue-URL citations against GITHUB_REPOSITORY.
	forbidDeferralRepo = "tsouza/cerberus"

	// The verdict line the gate prints when a marker cites nothing usable. In
	// the permission-fault case this line is the bug: it must NOT appear.
	untrackedVerdict = "untracked deferral"

	// The three diagnoses that must stay distinguishable.
	missingIssueDiagnosis = "names nothing in this repository"
	unreadableIssuesDiag  = "cannot read issues in"
	unreadableRepoDiag    = "cannot read the repository"

	// Marker fixtures are spelled in halves, exactly as the module's own unit
	// test spells them: this gate scans the added lines of the pull request
	// that adds this file, and a plain literal would make the file unmergeable
	// under the rule it is testing. The runtime strings are exact — only this
	// file's own source text is kept clean.
	markerFixture = "not fixed " + "here"
)

// fakeIssue is one row of the stub API's issue table.
type fakeIssue struct {
	state  string
	isPull bool
}

// fakeGitHub serves the three endpoints the module touches: the repository
// itself and the issue LIST (together the capability probe), and a single
// issue (the citation lookup).
//
// Each is refusable on its own, and a refusal is a 404 rather than a 403
// because that is how GitHub refuses — it will not confirm that a resource it
// is hiding exists. A token without `issues: read` is therefore modelled by
// refusing the list AND the lookup while the repository stays readable, which
// is exactly the state PR #1413's run was in. Independent flags also let a test
// assert that an endpoint was NOT consulted: refuse it, and a run that touches
// it changes its verdict.
type fakeGitHub struct {
	issues              map[int]fakeIssue
	repoReadable        bool
	issuesListReadable  bool
	issueLookupReadable bool

	mu         sync.Mutex
	unexpected []string
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()

	prefix := "/repos/" + forbidDeferralRepo
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == prefix:
			if !f.repoReadable {
				http.NotFound(w, r)
				return
			}
			writeFakeJSON(w, map[string]any{"full_name": forbidDeferralRepo})
		case path == prefix+"/issues":
			// The list endpoint an authorised caller always reaches: 200 with
			// an empty array even in a repository holding no issues.
			if !f.issuesListReadable {
				http.NotFound(w, r)
				return
			}
			writeFakeJSON(w, []any{})
		case strings.HasPrefix(path, prefix+"/issues/"):
			if !f.issueLookupReadable {
				http.NotFound(w, r)
				return
			}
			number, err := strconv.Atoi(strings.TrimPrefix(path, prefix+"/issues/"))
			if err != nil {
				f.record(path)
				http.NotFound(w, r)
				return
			}
			issue, ok := f.issues[number]
			if !ok {
				http.NotFound(w, r)
				return
			}
			body := map[string]any{"number": number, "state": issue.state}
			if issue.isPull {
				body["pull_request"] = map[string]any{"url": "https://example.invalid/pull"}
			}
			writeFakeJSON(w, body)
		default:
			f.record(path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unexpected = append(f.unexpected, path)
}

// assertNoStrayRequests fails when the module asked for a path this stub does
// not model — a silent 404 there would look like a missing issue and could make
// a broken run read as a passing one.
func (f *fakeGitHub) assertNoStrayRequests(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.unexpected) > 0 {
		t.Errorf("the module requested paths the stub API does not model: %v", f.unexpected)
	}
}

func writeFakeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

// TestForbidDeferralTellsAMissingIssueFromAnUnreadableOne is the whole point of
// the change: five API states, five distinct verdicts, and the two that both
// arrive as HTTP 404 must never print each other's diagnosis.
func TestForbidDeferralTellsAMissingIssueFromAnUnreadableOne(t *testing.T) {
	t.Parallel()

	const (
		openIssue     = 1574
		closedIssue   = 1486
		pullRequest   = 1143
		neverExisted  = 999999
		exitClean     = 0
		exitViolation = 1
	)

	table := map[int]fakeIssue{
		openIssue:   {state: "open"},
		closedIssue: {state: "closed"},
		pullRequest: {state: "closed", isPull: true},
	}

	cases := []struct {
		name string
		// repoReadable / issuesReadable are the API state, expressed the way
		// GitHub expresses a refusal: as a 404 on the endpoint concerned. A
		// missing `issues: read` refuses the list and the single-issue lookup
		// together, which is why one flag drives both.
		repoReadable   bool
		issuesReadable bool
		// cite is the number the description's marker points at.
		cite     int
		wantExit int
		// wantSaid / wantNotSaid are phrases the run's output must and must not
		// carry. The second half is what makes the case a discrimination test
		// rather than a "something failed" test.
		wantSaid    []string
		wantNotSaid []string
	}{
		{
			name:         "an issue number that names nothing is still a violation, with the same words",
			repoReadable: true, issuesReadable: true,
			cite:     neverExisted,
			wantExit: exitViolation,
			wantSaid: []string{untrackedVerdict, missingIssueDiagnosis},
			// The permission diagnosis would be a lie here: the token is fine.
			wantNotSaid: []string{unreadableIssuesDiag, unreadableRepoDiag},
		},
		{
			name:         "a token without issues:read fails as a permission fault, not as the author",
			repoReadable: true, issuesReadable: false,
			cite:     openIssue,
			wantExit: exitViolation,
			wantSaid: []string{unreadableIssuesDiag, "issues: read"},
			// The regression itself: the cited issue is open and correctly
			// filed, so reporting it as an untracked marker accuses the author
			// of the workflow's fault and prints a remedy they already applied.
			wantNotSaid: []string{untrackedVerdict, missingIssueDiagnosis},
		},
		{
			name:         "a token that cannot see the repository names the repository, not the grant",
			repoReadable: false, issuesReadable: false,
			cite:     openIssue,
			wantExit: exitViolation,
			wantSaid: []string{unreadableRepoDiag},
			// A wrong slug and a missing issues grant have different fixes.
			wantNotSaid: []string{untrackedVerdict, "issues: read"},
		},
		{
			name:         "an open issue tracks the work and the run is clean",
			repoReadable: true, issuesReadable: true,
			cite:        openIssue,
			wantExit:    exitClean,
			wantSaid:    []string{"tracked by an open issue"},
			wantNotSaid: []string{untrackedVerdict, unreadableIssuesDiag},
		},
		{
			name:         "a closed issue is untracked work wearing a citation",
			repoReadable: true, issuesReadable: true,
			cite:        closedIssue,
			wantExit:    exitViolation,
			wantSaid:    []string{untrackedVerdict, "closed issue"},
			wantNotSaid: []string{unreadableIssuesDiag},
		},
		{
			name:         "a pull request is not an issue, however open it is",
			repoReadable: true, issuesReadable: true,
			cite:        pullRequest,
			wantExit:    exitViolation,
			wantSaid:    []string{untrackedVerdict, "is a pull request, not an issue"},
			wantNotSaid: []string{unreadableIssuesDiag},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeGitHub{
				issues:              table,
				repoReadable:        tc.repoReadable,
				issuesListReadable:  tc.issuesReadable,
				issueLookupReadable: tc.issuesReadable,
			}
			srv := api.serve(t)

			body := fmt.Sprintf("A change that does a thing.\n\n%s, tracked by #%d.\n", markerFixture, tc.cite)
			out, exit := runForbidDeferral(t, srv.URL, body)

			if exit != tc.wantExit {
				t.Errorf("exit code = %d, want %d\noutput:\n%s", exit, tc.wantExit, out)
			}
			for _, want := range tc.wantSaid {
				if !strings.Contains(out, want) {
					t.Errorf("the run never says %q, so it does not report the cause it found\noutput:\n%s",
						want, out)
				}
			}
			for _, unwanted := range tc.wantNotSaid {
				if strings.Contains(out, unwanted) {
					t.Errorf("the run says %q, which is the wrong diagnosis for this API state — the author "+
						"would be sent to fix something that is not broken\noutput:\n%s", unwanted, out)
				}
			}
			api.assertNoStrayRequests(t)
		})
	}
}

// TestForbidDeferralProbesTheIssueGrantOnlyWhenALookupComesBackEmpty pins the
// cost model. The capability probe is two extra API calls; a run whose every
// citation resolves has nothing to disambiguate and must not pay them, and a
// probe that fired unconditionally would double the gate's API traffic on every
// green PR for no verdict at all.
func TestForbidDeferralProbesTheIssueGrantOnlyWhenALookupComesBackEmpty(t *testing.T) {
	t.Parallel()

	const openIssue = 1574

	// The stub refuses BOTH probe endpoints while serving the citation lookup —
	// a state no real token is in, and precisely the instrument this needs: a
	// run that resolves every citation must never reach the probe, so an
	// unconditional probe would fail here with a permission diagnosis instead
	// of passing.
	api := &fakeGitHub{
		issues:              map[int]fakeIssue{openIssue: {state: "open"}},
		repoReadable:        false,
		issuesListReadable:  false,
		issueLookupReadable: true,
	}
	srv := api.serve(t)

	body := fmt.Sprintf("A change that does a thing.\n\n%s, tracked by #%d.\n", markerFixture, openIssue)
	out, exit := runForbidDeferral(t, srv.URL, body)
	if exit != 0 {
		t.Fatalf("a correctly cited open issue must pass; exit = %d\noutput:\n%s", exit, out)
	}
	if strings.Contains(out, unreadableIssuesDiag) || strings.Contains(out, unreadableRepoDiag) {
		t.Errorf("the capability probe ran on a run with nothing to disambiguate\noutput:\n%s", out)
	}
	api.assertNoStrayRequests(t)
}

// TestForbidDeferralJobGrantsTheIssueLookupItsPermission is the static half of
// the same fix. The runtime probe turns a missing grant into an honest error;
// this keeps the grant from going missing in the first place. Both are needed:
// the workflow's own token is only one of the ways the lookup can lose its
// permission, and a comment saying "keep issues: read" cannot fail.
func TestForbidDeferralJobGrantsTheIssueLookupItsPermission(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, forbidDeferralWorkflowPath), forbidDeferralJob)
	for _, grant := range []string{"issues: read", "pull-requests: read"} {
		if !strings.Contains(job, grant) {
			t.Errorf("%s job %q no longer grants %q. Every cited number is resolved through the API, and "+
				"GitHub answers an unreadable resource with 404 rather than 403 — without the grant the "+
				"lookup cannot succeed, and the gate would fail every run naming a permission fault it "+
				"cannot fix on the author's behalf. Body:\n%s",
				forbidDeferralWorkflowPath, forbidDeferralJob, grant, job)
		}
	}
}

// runForbidDeferral runs the real module over a real two-commit range with the
// GitHub API pointed at `apiBase`, and returns its combined output and exit
// code. The range is a throwaway repository rather than this one: the gate
// walks `git log` and `git diff` in its working directory, and pinning it to
// cerberus's own history would make the test's verdict depend on whatever the
// branch under test happens to contain.
func runForbidDeferral(t *testing.T, apiBase, prBody string) (string, int) {
	t.Helper()

	module, err := filepath.Abs(forbidDeferralModule)
	if err != nil {
		t.Fatalf("resolve %s: %v", forbidDeferralModule, err)
	}
	dir, base, head := newScannedRange(t)

	cmd := exec.Command("node", module)
	cmd.Dir = dir
	cmd.Env = append(
		environWithoutGitHubVars(),
		"GITHUB_REPOSITORY="+forbidDeferralRepo,
		"GITHUB_TOKEN=stub-token",
		"GITHUB_EVENT_NAME=pull_request",
		"GITHUB_API_URL="+apiBase,
		"PR_BODY="+prBody,
		"BASE_SHA="+base,
		"HEAD_SHA="+head,
		"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "step-summary.md"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v (node must be on PATH)\noutput:\n%s", forbidDeferralModule, err, out)
		}
		return string(out), exitErr.ExitCode()
	}
	return string(out), 0
}

// environWithoutGitHubVars is the ambient environment with every variable the
// module reads stripped. On a runner all of GITHUB_REPOSITORY, GITHUB_TOKEN,
// GITHUB_EVENT_NAME and GITHUB_API_URL are already set, and a duplicated key
// resolves by position rather than by intent — the stub API would be bypassed
// for the real one and the test would assert nothing.
func environWithoutGitHubVars() []string {
	stripped := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(name, "GITHUB_"), name == "PR_BODY", name == "BASE_SHA", name == "HEAD_SHA":
			continue
		}
		stripped = append(stripped, entry)
	}
	return stripped
}

// newScannedRange builds a two-commit git repository and returns its path plus
// the two shas. The gate asserts positively that it inspected something — a
// non-empty commit list and a non-empty file set — so the range must carry a
// real diff, and neither commit message nor file content may carry a marker of
// its own or the fixture would report violations the case did not ask for.
func newScannedRange(t *testing.T) (dir, base, head string) {
	t.Helper()

	dir = t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "gate@example.invalid")
	runGit(t, dir, "config", "user.name", "gate fixture")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	writeFixtureFile(t, dir, "surface.md", "the first line of the surface\n")
	runGit(t, dir, "add", "surface.md")
	runGit(t, dir, "commit", "--quiet", "-m", "add the surface the range starts from")
	base = strings.TrimSpace(gitOutputIn(t, dir, "rev-parse", "HEAD"))

	writeFixtureFile(t, dir, "surface.md", "the first line of the surface\nand a second one\n")
	runGit(t, dir, "add", "surface.md")
	runGit(t, dir, "commit", "--quiet", "-m", "extend the surface so the range has a diff")
	head = strings.TrimSpace(gitOutputIn(t, dir, "rev-parse", "HEAD"))

	return dir, base, head
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// gitOutputIn runs git in `dir` under the same hermetic environment runGit uses
// — no global or system config, so a contributor's own gitconfig cannot change
// what the fixture range looks like — and returns its stdout.
func gitOutputIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return string(out)
}
