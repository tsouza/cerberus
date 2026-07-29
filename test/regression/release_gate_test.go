package regression

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The release pipeline's shape is the only thing standing between a merge and a
// public artifact, and none of it is exercised by a PR: release.yml has no
// `pull_request:` trigger, so every edit to it is unverified until a release is
// actually cut. The pins below are pure file reads over the workflows and
// .goreleaser.yml, so they run on the required `check` lane and a regression
// fails at merge time rather than mid-release.
//
// Each pin exists because the corresponding regression is INVISIBLE — it leaves
// the pipeline green:
//
//   - a required lane dropped from RELEASE_REQUIRED_CHECKS: the preflight is
//     observation-derived for everything outside that set, so a lane that never
//     ran contributes zero problems and the release publishes on a silence;
//   - the draft->published flip moved back inside `goreleaser`: the release goes
//     public before anything drove the built image;
//   - `continue-on-error` on brew-smoke: the job becomes decoration;
//   - `skip_upload: auto` removed from .goreleaser.yml: prereleases start
//     overwriting the tap cask, which is the thing brew-smoke's prerelease
//     branch asserts against.
const (
	releaseWorkflowPath   = "../../.github/workflows/release.yml"
	migrationWorkflowPath = "../../.github/workflows/migration-e2e.yml"
	goreleaserPath        = "../../.goreleaser.yml"

	// The lane this whole gate exists to make blocking. It is push/schedule
	// triggered, so it can never be a branch-protection check — the preflight's
	// required set is the only thing that makes it gate a release.
	releaseGatedLane = "migration-e2e"

	// The job that drives releaseGatedLane against the image goreleaser built,
	// and the job that makes the release public. The ordering between them is
	// the point: everything that validates the artifact sits before the flip.
	artifactMigrationJob = "release-artifact-migration"
	publishJob           = "publish"
	brewSmokeJob         = "brew-smoke"
	preflightJob         = "preflight"
	goreleaserJobName    = "goreleaser"

	// The single point of no return in the pipeline.
	publishFlipToken = "--draft=false"

	// "Is this tag the highest stable release?" — the one signal that decides
	// which release line may take a resource only one line can hold: the rolling
	// `:latest` images, the Homebrew cask, and the GitHub `Latest` pointer.
	releaseIsLatestEnv = "RELEASE_IS_LATEST"
	isLatestOutput     = "is_latest"
	latestFlipToken    = "--latest="
)

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// requiredChecksFromPreflight extracts the RELEASE_REQUIRED_CHECKS value out of
// the preflight job body. Parsing the value (rather than substring-matching the
// whole file) is what makes the membership assertion below real: a name that
// appears somewhere else in the workflow does not satisfy it.
func requiredChecksFromPreflight(t *testing.T, job string) []string {
	t.Helper()
	const key = "RELEASE_REQUIRED_CHECKS:"
	for _, line := range strings.Split(job, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		raw = strings.Trim(raw, `"'`)
		var out []string
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	t.Fatalf("%s job %q declares no %s — the release gate has degraded to the "+
		"observational deny-list, where a lane that never ran contributes zero problems. Body:\n%s",
		releaseWorkflowPath, preflightJob, key, job)
	return nil
}

func TestReleasePreflightRequiresTheMigrationLane(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, releaseWorkflowPath)
	job := workflowJobBody(t, workflow, preflightJob)

	required := requiredChecksFromPreflight(t, job)
	if len(required) == 0 {
		t.Fatalf("%s job %q declares an EMPTY required set, which is the same silence as declaring none",
			releaseWorkflowPath, preflightJob)
	}

	found := false
	for _, name := range required {
		if name == releaseGatedLane {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s job %q does not require %q (required set: %v). That lane cannot be a branch-protection "+
			"check, so dropping it here silently un-gates it: the preflight only reports problems for "+
			"check-runs it observed, and a lane that never ran posts none.",
			releaseWorkflowPath, preflightJob, releaseGatedLane, required)
	}

	// The gate must run on BOTH publish paths. A `refs/heads/release/` guard in
	// the job body is the old maintenance-only wiring, under which a release cut
	// from main skipped the green check entirely.
	if strings.Contains(job, "refs/heads/release/") {
		t.Fatalf("%s job %q still branch-gates itself to the maintenance lines; the preflight is "+
			"publish-gated now, so a release from main must go through it too. Body:\n%s",
			releaseWorkflowPath, preflightJob, job)
	}
	for _, want := range []string{"app_publish", "chart_publish"} {
		if !strings.Contains(job, want) {
			t.Fatalf("%s job %q does not gate on %q; it must run whenever something would publish. Body:\n%s",
				releaseWorkflowPath, preflightJob, want, job)
		}
	}

	// Informational entries are PREFIX matches inside the preflight, so an
	// informational name that is a prefix of a required one de-gates the
	// required lane. The script reports that as a wiring error at runtime; this
	// pin catches it at merge time.
	informational := informationalChecksFromPreflight(t, job)
	for _, prefix := range informational {
		for _, name := range required {
			if strings.HasPrefix(name, prefix) {
				t.Fatalf("%s job %q lists informational prefix %q, which swallows required lane %q — "+
					"the required lane would be de-gated while still looking required",
					releaseWorkflowPath, preflightJob, prefix, name)
			}
		}
	}

	// The self-test that proves the required set is actually enforced runs as a
	// step of the job itself, and on the required `lint` lane.
	if !strings.Contains(job, "release-preflight.test.mjs") {
		t.Fatalf("%s job %q does not run release-preflight.test.mjs; the gate's own detectors would be "+
			"unverified. Body:\n%s", releaseWorkflowPath, preflightJob, job)
	}
	ci := readFileString(t, "../../.github/workflows/ci.yml")
	for _, want := range []string{"release-preflight.test.mjs", "brew-smoke.test.mjs"} {
		if !strings.Contains(ci, want) {
			t.Fatalf("../../.github/workflows/ci.yml does not run %q. release.yml has no pull_request "+
				"trigger, so without this step the release gate's unit guards execute only while a "+
				"release is being cut", want)
		}
	}
}

// informationalChecksFromPreflight reads the de-gate list the same way the
// preflight does, so the disjointness assertion above compares like with like.
func informationalChecksFromPreflight(t *testing.T, job string) []string {
	t.Helper()
	const key = "RELEASE_INFORMATIONAL_CHECKS:"
	for _, line := range strings.Split(job, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, key)), `"'`)
		var out []string
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

func TestReleasePublishesOnlyAfterTheBuiltArtifactIsDriven(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, releaseWorkflowPath)

	// Exactly one flip, in exactly one job. Two would mean a second, unguarded
	// path to publication; zero would mean the release never leaves draft.
	if got := strings.Count(workflow, publishFlipToken); got != 1 {
		t.Fatalf("%s contains %d occurrences of %q, want exactly 1 — the draft->published flip is the "+
			"point of no return and must have a single, guarded call site",
			releaseWorkflowPath, got, publishFlipToken)
	}
	publish := workflowJobBody(t, workflow, publishJob)
	if !strings.Contains(publish, publishFlipToken) {
		t.Fatalf("%s job %q does not perform the %q flip; publication has moved to another job, "+
			"bypassing the artifact validation that this job's `needs:` enforces. Body:\n%s",
			releaseWorkflowPath, publishJob, publishFlipToken, publish)
	}
	if !strings.Contains(publish, artifactMigrationJob) {
		t.Fatalf("%s job %q does not depend on %q, so the release would go public without the built "+
			"image ever being driven. Body:\n%s",
			releaseWorkflowPath, publishJob, artifactMigrationJob, publish)
	}

	// The artifact lane must drive the RELEASED image and assert the version it
	// reports; without either input it silently degrades into a second
	// source-tree run that proves nothing new about the artifact.
	artifact := workflowJobBody(t, workflow, artifactMigrationJob)
	for _, want := range []string{
		"uses: ./.github/workflows/migration-e2e.yml",
		"cerberus_image:",
		"expect_version:",
	} {
		if !strings.Contains(artifact, want) {
			t.Fatalf("%s job %q is missing %q. Body:\n%s",
				releaseWorkflowPath, artifactMigrationJob, want, artifact)
		}
	}
}

func TestBrewSmokeIsBlockingAndOrderedAfterPublish(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, releaseWorkflowPath)
	job := workflowJobBody(t, workflow, brewSmokeJob)

	for _, want := range []string{publishJob, "brew-smoke.mjs", "brew-smoke.test.mjs"} {
		if !strings.Contains(job, want) {
			t.Fatalf("%s job %q is missing %q. `brew install` downloads the release tarball, which 404s "+
				"while the release is still a draft — the ordering is correctness, not cosmetics. Body:\n%s",
				releaseWorkflowPath, brewSmokeJob, want, job)
		}
	}
	if strings.Contains(job, "continue-on-error") {
		t.Fatalf("%s job %q declares continue-on-error, which turns the install smoke into decoration: "+
			"a broken tap would report green. Body:\n%s", releaseWorkflowPath, brewSmokeJob, job)
	}

	// The cask serves both platforms, and cerberus is a Linux-first server
	// binary. A macOS-only smoke reports green on a cask that no Linux operator
	// can install, because `brew install` failing under Linuxbrew looks
	// identical to it working when you only ever run it on a Mac. Dropping
	// either leg from the matrix is a silent de-gate of exactly that, so the
	// pair is pinned rather than left to review.
	for _, os := range []string{"macos-latest", "ubuntu-latest"} {
		if !strings.Contains(job, os) {
			t.Fatalf("%s job %q no longer runs on %q. The cask declares darwin AND linux artefacts, so "+
				"smoking one platform leaves the other asserted but never installed. Body:\n%s",
				releaseWorkflowPath, brewSmokeJob, os, job)
		}
	}

	// The two no-cask branches of brew-smoke.mjs assert that a prerelease and
	// a maintenance backport did NOT write a cask. Those assertions are only
	// meaningful while goreleaser is still configured to leave both taps alone,
	// which is one template covering both conditions.
	//
	// Matched on the `skip_upload:` LINE, not on the file: RELEASE_IS_LATEST also
	// appears in the dockers_v2 `latest-*` skip_push templates, so a whole-file
	// substring check would still pass after a revert to a bare `skip_upload:
	// auto` — a pin that cannot fail for the thing it is pinning.
	skipUpload := valueOfYAMLKey(t, readFileString(t, goreleaserPath), "skip_upload")
	for _, want := range []string{releaseIsLatestEnv, "auto"} {
		if !strings.Contains(skipUpload, want) {
			t.Fatalf("%s has `skip_upload: %s`, which does not gate on %q. brew-smoke's prerelease and "+
				"maintenance branches assert that neither wrote a cask, and would start failing every "+
				"rc and every backport", goreleaserPath, skipUpload, want)
		}
	}
}

// valueOfYAMLKey returns the inline value of the single `<key>:` line in body,
// failing when the key is absent or appears more than once. Scoping an assertion
// to one line is what keeps it from passing on an unrelated occurrence of the
// same token elsewhere in the file.
func valueOfYAMLKey(t *testing.T, body, key string) string {
	t.Helper()

	var found []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, key+":"); ok {
			found = append(found, strings.TrimSpace(after))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 `%s:` line, found %d (%q) — the assertion below is scoped to a single "+
			"line and cannot be trusted otherwise", key, len(found), found)
	}
	return found[0]
}

// TestOnlyTheNewestLineOwnsTheSharedReleaseResources pins the fix for v1.12.1 —
// a backport cut after v1.13.0 — taking two resources that only one release line
// can hold at a time: the Homebrew cask (it overwrote v1.13.0's, downgrading
// every `brew install`) and the GitHub `Latest` pointer (v1.11.3, published last,
// claimed it). Both defaults are silently wrong for a maintenance line:
// `skip_upload: auto` filters only prereleases, and GitHub's release-update
// endpoint defaults `make_latest` to true. Neither failure reds a release, so
// nothing but this pin keeps the explicit gate in place.
func TestOnlyTheNewestLineOwnsTheSharedReleaseResources(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, releaseWorkflowPath)

	// The signal has to leave the goreleaser job: it can only be computed after
	// the tag is pushed, and the two consumers below are separate jobs.
	goreleaserJob := workflowJobBody(t, workflow, goreleaserJobName)
	for _, want := range []string{"outputs:", isLatestOutput + ":", releaseIsLatestEnv} {
		if !strings.Contains(goreleaserJob, want) {
			t.Fatalf("%s job %q is missing %q, so the highest-stable-tag signal never leaves the job "+
				"and the downstream owners fall back to their wrong-for-a-backport defaults. Body:\n%s",
				releaseWorkflowPath, goreleaserJobName, want, goreleaserJob)
		}
	}

	// Consumer 1: the draft->published flip must state the Latest pointer rather
	// than inherit `make_latest: true`.
	publish := workflowJobBody(t, workflow, publishJob)
	if !strings.Contains(publish, latestFlipToken) {
		t.Fatalf("%s job %q publishes without %q. GitHub defaults `make_latest` to true, so the flip "+
			"hands /releases/latest to whichever release published most recently — a backport would "+
			"take it from the newest line. Body:\n%s",
			releaseWorkflowPath, publishJob, latestFlipToken, publish)
	}
	if !strings.Contains(publish, isLatestOutput) {
		t.Fatalf("%s job %q sets %q from something other than the %q output, so the pointer no longer "+
			"tracks the highest stable tag. Body:\n%s",
			releaseWorkflowPath, publishJob, latestFlipToken, isLatestOutput, publish)
	}

	// Consumer 2: brew-smoke must be told which branch the tap actually took.
	// Without it the smoke has no safe default and fails closed, but pinning it
	// here keeps the maintenance assertion from being quietly dropped instead.
	brew := workflowJobBody(t, workflow, brewSmokeJob)
	if !strings.Contains(brew, releaseIsLatestEnv) {
		t.Fatalf("%s job %q does not pass %s, so the smoke cannot assert that a backport left the "+
			"newest line's cask in place. Body:\n%s",
			releaseWorkflowPath, brewSmokeJob, releaseIsLatestEnv, brew)
	}
}

func TestMigrationLaneIsCallableAndCoversTheReleaseBranches(t *testing.T) {
	t.Parallel()

	body := readFileString(t, migrationWorkflowPath)

	var parsed struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse %s: %v", migrationWorkflowPath, err)
	}

	// A maintenance backport publishes from its own line, and the preflight
	// REQUIRES this lane on the released commit. Without the release-branch
	// pattern the lane never runs there, so every patch release blocks forever.
	for _, want := range []string{"main", "release/*.x"} {
		found := false
		for _, b := range parsed.On.Push.Branches {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not push-trigger on %q (branches: %v); the release preflight requires a "+
				"green %q check-run on every commit it publishes",
				migrationWorkflowPath, want, parsed.On.Push.Branches, releaseGatedLane)
		}
	}

	if !strings.Contains(body, "\n  workflow_call:\n") {
		t.Fatalf("%s declares no workflow_call trigger, so release.yml cannot re-run the lane against "+
			"the image it just built", migrationWorkflowPath)
	}
	// The caller passes both inputs; an undeclared input is a hard workflow
	// error at dispatch time, which is a failure DURING a release.
	for _, want := range []string{"cerberus_image:", "expect_version:"} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s declares no %q input, but release.yml passes it", migrationWorkflowPath, want)
		}
	}
	// Still ineligible as a branch-protection check, which is the whole reason
	// the preflight's required set exists.
	if strings.Contains(body, "\n  pull_request:\n") {
		t.Fatalf("%s gained a pull_request trigger. The lane is a heavyweight Docker/k3d suite kept off "+
			"the PR path on purpose; if that changes, the required-set gate needs revisiting rather than "+
			"quietly acquiring a second meaning", migrationWorkflowPath)
	}
}

// The `release/*.x` ruleset ("release maintenance lines") denies `deletion` to
// everyone but the admin RepositoryRole. `eol-retire` is fail-open by design —
// a retirement hiccup must never fail a shipped release — so if its token loses
// the admin bypass the job logs an ::error:: and still exits 0, the release
// publishes green, and the EOL branch quietly survives. Nothing else in CI can
// observe that: the job only ever runs on a real minor open, on main.
//
// RELEASE_PAT is the only token that carries the bypass (an admin-owned PAT
// inherits the role; github-actions[bot] holds write and never admin). Pinning
// the reference keeps the fallback from silently becoming the only path.
func TestEOLRetireUsesTheTokenThatBypassesTheReleaseRuleset(t *testing.T) {
	body := readFileString(t, releaseWorkflowPath)

	const retireStep = "release-preflight.mjs eol-retire-line"
	if !strings.Contains(body, retireStep) {
		t.Fatalf("%s no longer invokes %q; the active-EOL half of the support window is gone",
			releaseWorkflowPath, retireStep)
	}
	if !strings.Contains(body, "secrets.RELEASE_PAT") {
		t.Fatalf("%s no longer passes secrets.RELEASE_PAT to %q. The default GITHUB_TOKEN acts as "+
			"github-actions[bot], which the release/*.x ruleset refuses, and the job is fail-open — "+
			"the branch would survive with the release still green",
			releaseWorkflowPath, retireStep)
	}
}

// release-gate-drift.yml watches RELEASE_REQUIRED_CHECKS for rot: a protected
// context nobody listed (the release publishes past a lane every PR must pass)
// or a listed name nothing posts (the next release waits out its window and
// aborts mid-publish). Being schedule-only, it has the same blind spot the
// release workflow has — no PR ever runs it — so the ways it can degrade to
// decoration are invisible in a diff:
//
//   - the live comparison step dropped, leaving only `--self-test`: the weekly
//     run goes green every week while comparing nothing;
//   - RELEASE_PAT swapped for the default GITHUB_TOKEN: branch protection is
//     unreadable to github-actions[bot] at any `permissions:` level, so the job
//     reds every week until someone deletes it as noise;
//   - the schedule removed: the detector survives as a manual-dispatch button
//     nobody presses.
func TestReleaseGateDriftDetectorRunsLiveAndNotSelfTestOnly(t *testing.T) {
	t.Parallel()

	const (
		driftWorkflowPath = "../../.github/workflows/release-gate-drift.yml"
		driftScript       = "release-gate-drift.mjs"
		selfTestFlag      = "--self-test"
	)

	body := readFileString(t, driftWorkflowPath)

	if !strings.Contains(body, "schedule:") {
		t.Fatalf("%s has no schedule: trigger. A rot detector that only runs on manual dispatch "+
			"reports drift after the release it was meant to protect", driftWorkflowPath)
	}

	// The load-bearing assertion: at least one invocation WITHOUT the self-test
	// flag. A file that mentions the script only inside a `--self-test` step is
	// the exact hollow-green shape this pin exists to reject.
	live := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, driftScript) && !strings.Contains(line, selfTestFlag) {
			live++
		}
	}
	if live == 0 {
		t.Fatalf("%s never runs %s without %s — the workflow proves its own logic and compares it "+
			"against nothing, so the gate is green whatever branch protection says",
			driftWorkflowPath, driftScript, selfTestFlag)
	}

	if !strings.Contains(body, "secrets.RELEASE_PAT") {
		t.Fatalf("%s no longer passes secrets.RELEASE_PAT. Reading a branch's protection settings "+
			"needs repo-admin rights, which the default GITHUB_TOKEN lacks at every permissions: "+
			"level, so the live comparison cannot run at all", driftWorkflowPath)
	}

	// The pure half has to stay on a PR lane, for the same reason
	// release-preflight.test.mjs does: otherwise a logic regression sits
	// undetected until the next weekly run.
	ci := readFileString(t, "../../.github/workflows/ci.yml")
	if !strings.Contains(ci, driftScript+" "+selfTestFlag) {
		t.Fatalf("ci.yml no longer runs %s %s. The detector's parse and both drift comparisons "+
			"would then be unexercised between weekly runs", driftScript, selfTestFlag)
	}
}

// prepare-release.yml stages a release PR from whatever ref it was dispatched
// on. Cutting a maintenance patch means dispatching it on
// release/<major>.<minor>.x — the staging branch is then cut from that
// checkout, so the base the PR is opened against is the only thing keeping the
// release on its own line. A literal base sends a maintenance line's entire
// history at the head line instead, and nothing downstream notices: the PR
// opens, CI runs, and the mistake is visible only to whoever reads the base.
const (
	prepareReleaseWorkflowPath = "../../.github/workflows/prepare-release.yml"
	prepareReleaseJob          = "prepare"

	// The env var carrying github.ref_name into the step's shell, and the
	// expression that must feed it.
	prepareReleaseBaseEnv  = "BASE"
	prepareReleaseBaseExpr = "${{ github.ref_name }}"
)

func TestPrepareReleaseOpensThePRAgainstTheDispatchedLine(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, prepareReleaseWorkflowPath)
	job := workflowJobBody(t, workflow, prepareReleaseJob)

	if !strings.Contains(job, prepareReleaseBaseEnv+": "+prepareReleaseBaseExpr) {
		t.Fatalf("%s job %q does not bind %s to %s. Without it the base is whatever the run: block "+
			"hardcodes, and a maintenance release staged on release/<line>.x opens against the head "+
			"line. Body:\n%s",
			prepareReleaseWorkflowPath, prepareReleaseJob, prepareReleaseBaseEnv, prepareReleaseBaseExpr, job)
	}

	var create string
	for _, line := range strings.Split(job, "\n") {
		if strings.Contains(line, "gh pr create") {
			create = strings.TrimSpace(line)
		}
	}
	if create == "" {
		t.Fatalf("%s job %q no longer opens the release PR with `gh pr create`; this pin cannot see "+
			"which base it targets. Body:\n%s", prepareReleaseWorkflowPath, prepareReleaseJob, job)
	}
	if !strings.Contains(create, `--base "$`+prepareReleaseBaseEnv+`"`) {
		t.Fatalf("%s job %q opens the release PR as %q — the base must be $%s, the dispatched ref, "+
			"not a fixed branch name",
			prepareReleaseWorkflowPath, prepareReleaseJob, create, prepareReleaseBaseEnv)
	}
}
