// release-source-pr-dashboard-gate.mjs — re-validates the release-staging PR's
// OWN `dashboard` (k3d Grafana crawl) verdict before a publish, closing
// tsouza/cerberus#2361.
//
// The gap this closes
// --------------------
// `dashboard`'s crawl leg — the ~50min k3d BFS that walks every Grafana
// surface — is dispatched by `.github/scripts/dashboard-matrix.mjs`
// (INCLUDE_CRAWL) only for schedule, workflow_dispatch, or a `release/*`-headed
// `pull_request`. It is NEVER dispatched for a plain `push` (see
// dashboard-matrix.mjs and e2e.yml's `dashboard-setup` job). So the ONLY CI run
// that ever exercises the crawl against a release commit is the release-staging
// PR's own run — the push-to-main run that follows the merge gets a
// smoke-only `dashboard` verdict with zero crawl-coverage evidence for that
// exact commit, even though it reports `success`.
//
// `dashboard` is also not a native branch-protection required context on
// `main` (a deliberate trade — see e2e.yml's header and docs/operations.md's
// "Two-tier test fence"), so GitHub's native auto-merge does not wait for it
// either. #2361 is the incident: PR #2360 auto-merged to `main` while its own
// `dashboard` / crawl result was FAILURE, and `release.yml`'s existing
// `RELEASE_REQUIRED_CHECKS` read of the push-triggered run's `dashboard` check
// (release-preflight.mjs) could not have caught it even if the merge had
// waited, because that push run never runs the crawl leg at all.
//
// The fix
// -------
// Given the merge commit, resolve the ORIGINAL release-staging PR via GitHub's
// commit -> pull-request association (`GET /commits/{sha}/pulls`), then read
// THAT PR's own `dashboard` check-run — the one run that ever actually
// exercised the crawl leg — directly, instead of trusting the push run's
// smoke-only proxy. A release-staging PR is identified structurally, the same
// way dashboard-matrix.mjs / e2e.yml pick it: a head ref starting with
// `release/`. A commit with no such associated PR (an ordinary merge, or a
// maintenance-line hotfix pushed with no PR at all) has nothing extra to
// re-validate here — release-preflight.mjs's existing RELEASE_REQUIRED_CHECKS
// read already covers whatever DID run on that push, and this gate adds
// nothing to relax.
//
// ABSENCE IS A FAILURE, same posture as release-preflight.mjs: a release
// PR whose own head commit never posted a `dashboard` check-run at all (e.g.
// merged manually before the k3d lane finished) blocks exactly like a red one
// — the crawl either ran and passed, or the release does not publish.
//
// Env:
//   GITHUB_TOKEN       token with pull-requests:read + checks:read.
//   GITHUB_REPOSITORY  "owner/name".
//   GITHUB_SHA         the pushed (merge) commit SHA.
//   GITHUB_API_URL     API base (default https://api.github.com).
//   argv `--self-test` runs the pure-logic assertions and exits.
//
// Exit: 0 when no release-staging source PR is found, or every one found has a
// completed, successful `dashboard` check-run on its own head commit; 1
// otherwise.

import process from 'node:process';

// dashboard-matrix.mjs / e2e.yml's own `dashboard-setup` select the crawl leg
// off exactly this prefix on `github.head_ref` — mirrored here so a PR is
// recognised as a release-staging PR the same way the lane that ran its crawl
// recognised it.
export const RELEASE_HEAD_REF_PREFIX = 'release/';

// The aggregator job name in e2e.yml. Exact match only — see
// release_required_checks_test.go's checkOwnerIndex comment on why a prefix
// match would accept a name nothing posts.
export const DASHBOARD_CHECK_NAME = 'dashboard';

function short(sha) {
  return String(sha ?? '').slice(0, 8);
}

// isReleaseStagingPR — a PR counts as release-staging iff its head ref starts
// with `release/`, exactly the signal dashboard-matrix.mjs's INCLUDE_CRAWL and
// e2e.yml's dashboard-setup `if:` already condition the crawl leg on.
export function isReleaseStagingPR(pr) {
  return typeof pr?.head?.ref === 'string' && pr.head.ref.startsWith(RELEASE_HEAD_REF_PREFIX);
}

// releaseStagingSourcePRs — filter the commit's associated pull requests down
// to the release-staging one(s). Squash-merging normally associates exactly
// one PR with the resulting commit; the filter (rather than taking pulls[0])
// is defensive against the API ever returning more than one, and against a
// non-release PR sharing the association list.
export function releaseStagingSourcePRs(pulls) {
  return (pulls ?? []).filter(isReleaseStagingPR);
}

// latestDashboardCheckRun — the highest-id `dashboard` check-run in the set (a
// re-run leaves several with the same name; the highest id is current), or
// null if none posted.
export function latestDashboardCheckRun(checkRuns) {
  let latest = null;
  for (const cr of checkRuns ?? []) {
    if (cr?.name !== DASHBOARD_CHECK_NAME) continue;
    if (!latest || cr.id > latest.id) latest = cr;
  }
  return latest;
}

// sourcePRDashboardProblem — the blocking-problem string for one release-
// staging PR, or null when its own `dashboard` check-run is a completed
// success. Pure — no network — so the self-test pins the exact boundary.
export function sourcePRDashboardProblem({ pr, checkRuns }) {
  const run = latestDashboardCheckRun(checkRuns);
  if (!run) {
    return (
      `PR #${pr.number} (${pr.head.ref}) never posted a "${DASHBOARD_CHECK_NAME}" check-run on its ` +
      `own head commit ${short(pr.head.sha)}. That PR's run is the ONLY one that ever exercises the ` +
      `k3d crawl leg for this commit (dashboard-matrix.mjs's INCLUDE_CRAWL never fires on a push), so ` +
      `its absence blocks the release the same as a red one would. See tsouza/cerberus#2361.`
    );
  }
  if (run.status !== 'completed') {
    return (
      `PR #${pr.number} (${pr.head.ref}): "${DASHBOARD_CHECK_NAME}" is still ${run.status} on its own ` +
      `head commit ${short(pr.head.sha)} — the release-staging PR merged before its own dashboard/crawl ` +
      `verdict settled. See tsouza/cerberus#2361.`
    );
  }
  if (run.conclusion !== 'success') {
    return (
      `PR #${pr.number} (${pr.head.ref}): "${DASHBOARD_CHECK_NAME}" concluded ${run.conclusion} on its ` +
      `own head commit ${short(pr.head.sha)} — the k3d Grafana crawl that validated this release found a ` +
      `regression, and merging past it (native auto-merge does not wait on it; see #2361) did not clear ` +
      `it. Refusing to publish.`
    );
  }
  return null;
}

// evaluateSourcePRs — orchestrates the pure decision over every release-
// staging PR associated with the merge commit, given a name->check-runs
// lookup already fetched by the caller. Returns { problems, checked }; empty
// problems (checked may be 0) means publish may proceed.
export function evaluateSourcePRs({ pulls, checkRunsByPR }) {
  const releasePRs = releaseStagingSourcePRs(pulls);
  const problems = [];
  for (const pr of releasePRs) {
    const checkRuns = checkRunsByPR?.get(pr.number) ?? [];
    const problem = sourcePRDashboardProblem({ pr, checkRuns });
    if (problem) problems.push(problem);
  }
  return { problems, checked: releasePRs.length };
}

// ---------------------------------------------------------------------------
// network (thin — the decision logic above is what the self-test pins)
// ---------------------------------------------------------------------------

async function getJSON(url, headers, fetchImpl) {
  const res = await fetchImpl(url, { headers });
  if (!res.ok) {
    throw new Error(`GET ${url} -> ${res.status} ${res.statusText}`);
  }
  return res.json();
}

// associatedPulls — every pull request GitHub associates with a commit. For a
// squash-merged commit on `main` this is normally exactly the one PR that
// produced it, even after its head branch was deleted post-merge.
export async function associatedPulls({ apiBase, repo, sha, headers, fetchImpl = globalThis.fetch }) {
  return getJSON(`${apiBase}/repos/${repo}/commits/${sha}/pulls?per_page=100`, headers, fetchImpl);
}

// checkRunsForSha — every check-run GitHub has posted on a commit (paginated).
export async function checkRunsForSha({ apiBase, repo, sha, headers, fetchImpl = globalThis.fetch }) {
  const out = [];
  let page = 1;
  for (;;) {
    const data = await getJSON(
      `${apiBase}/repos/${repo}/commits/${sha}/check-runs?per_page=100&page=${page}`,
      headers,
      fetchImpl,
    );
    const runs = data.check_runs ?? [];
    out.push(...runs);
    if (runs.length < 100) break;
    page += 1;
  }
  return out;
}

function ghNotice(msg) {
  process.stdout.write(`::notice::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function ghError(msg) {
  process.stdout.write(`::error::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

async function main() {
  const repo = process.env.GITHUB_REPOSITORY;
  const sha = process.env.GITHUB_SHA;
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  if (!repo || !sha || !token) {
    ghError('release-source-pr-dashboard-gate: GITHUB_REPOSITORY, GITHUB_SHA, and GITHUB_TOKEN are all required');
    process.exit(1);
  }

  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: 'application/vnd.github+json',
    'X-GitHub-Api-Version': '2022-11-28',
  };

  const pulls = await associatedPulls({ apiBase, repo, sha, headers });
  const releasePRs = releaseStagingSourcePRs(pulls);

  if (releasePRs.length === 0) {
    ghNotice(
      `release-source-pr-dashboard-gate: ${short(sha)} has no associated release-staging (${RELEASE_HEAD_REF_PREFIX}*` +
        `-headed) pull request — nothing extra to re-validate here; the push-triggered RELEASE_REQUIRED_CHECKS ` +
        `read already covers whatever ran on this commit.`,
    );
    process.exit(0);
  }

  const checkRunsByPR = new Map();
  for (const pr of releasePRs) {
    checkRunsByPR.set(pr.number, await checkRunsForSha({ apiBase, repo, sha: pr.head.sha, headers }));
  }

  const { problems, checked } = evaluateSourcePRs({ pulls, checkRunsByPR });

  if (problems.length > 0) {
    for (const p of problems) ghError(p);
    ghError(
      `release-source-pr-dashboard-gate: ${short(sha)} BLOCKED — ${problems.length} problem(s) across ` +
        `${checked} release-staging source PR(s). See tsouza/cerberus#2361.`,
    );
    process.exit(1);
  }

  ghNotice(
    `release-source-pr-dashboard-gate: ${short(sha)} OK — ${checked} release-staging source PR(s), every ` +
      `own "${DASHBOARD_CHECK_NAME}" check-run completed successfully.`,
  );
  process.exit(0);
}

// ---------------------------------------------------------------------------
// self-test
// ---------------------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };

  const dashboardRun = (conclusion, status = 'completed', id = 1) => ({
    name: DASHBOARD_CHECK_NAME,
    status,
    conclusion,
    id,
  });
  const otherRun = (name, id = 1) => ({ name, status: 'completed', conclusion: 'success', id });

  const releasePR = (number, ref = 'release/v1.15.0-chart-0.15.0', sha = 'aaaa1111') => ({
    number,
    head: { ref, sha },
  });
  const ordinaryPR = (number, sha = 'bbbb2222') => ({ number, head: { ref: 'fix/some-bug', sha } });

  // --- isReleaseStagingPR / releaseStagingSourcePRs -------------------------
  assert(isReleaseStagingPR(releasePR(1)), 'a release/*-headed PR is release-staging');
  assert(!isReleaseStagingPR(ordinaryPR(1)), 'an ordinary head ref is not release-staging');
  assert(!isReleaseStagingPR({}), 'a PR with no head is not release-staging');

  assert(
    releaseStagingSourcePRs([releasePR(1), ordinaryPR(2)]).length === 1,
    'the filter keeps only the release-staging PR',
  );
  assert(releaseStagingSourcePRs([]).length === 0, 'no pulls -> nothing found');
  assert(releaseStagingSourcePRs(undefined).length === 0, 'undefined pulls -> nothing found (defensive)');

  // --- latestDashboardCheckRun ----------------------------------------------
  assert(latestDashboardCheckRun([]) === null, 'no runs -> null');
  assert(latestDashboardCheckRun([otherRun('lint')]) === null, 'no dashboard run among others -> null');
  {
    const runs = [dashboardRun('failure', 'completed', 1), dashboardRun('success', 'completed', 2)];
    const latest = latestDashboardCheckRun(runs);
    assert(latest.conclusion === 'success', 'a re-run supersedes an earlier one by id');
  }

  // --- sourcePRDashboardProblem: absence, running, red, green ---------------
  const pr = releasePR(2360);

  {
    const problem = sourcePRDashboardProblem({ pr, checkRuns: [otherRun('lint')] });
    assert(problem && /never posted a "dashboard" check-run/.test(problem), 'absence blocks: ' + problem);
  }
  {
    const problem = sourcePRDashboardProblem({ pr, checkRuns: [dashboardRun(null, 'in_progress')] });
    assert(problem && /is still in_progress/.test(problem), 'a still-running dashboard run blocks: ' + problem);
  }
  {
    // This is the exact #2361 incident shape: dashboard concluded FAILURE on
    // the release-staging PR's own head commit.
    const problem = sourcePRDashboardProblem({ pr, checkRuns: [dashboardRun('failure')] });
    assert(problem && /concluded failure/.test(problem), 'a red dashboard run blocks: ' + problem);
  }
  {
    const problem = sourcePRDashboardProblem({ pr, checkRuns: [dashboardRun('success')] });
    assert(problem === null, 'a completed successful dashboard run does not block: ' + problem);
  }
  {
    // `skipped` must NOT be treated as green here — dashboard-setup always
    // runs (INCLUDE_CRAWL true) for a release/*-headed PR, so a skipped
    // aggregator on a release PR is itself a wiring anomaly, not a pass.
    const problem = sourcePRDashboardProblem({ pr, checkRuns: [dashboardRun('skipped')] });
    assert(problem && /concluded skipped/.test(problem), 'a skipped dashboard run on a release PR blocks: ' + problem);
  }

  // --- evaluateSourcePRs: orchestration -------------------------------------
  {
    // No release-staging PR associated with the commit -> nothing to check.
    const r = evaluateSourcePRs({ pulls: [ordinaryPR(1)], checkRunsByPR: new Map() });
    assert(r.problems.length === 0 && r.checked === 0, 'no release-staging PR -> clean no-op: ' + JSON.stringify(r));
  }
  {
    // Exactly the #2361 shape: one release-staging PR, its own dashboard run
    // is FAILURE -> blocks, naming the PR.
    const r = evaluateSourcePRs({
      pulls: [releasePR(2360)],
      checkRunsByPR: new Map([[2360, [dashboardRun('failure')]]]),
    });
    assert(r.checked === 1 && r.problems.length === 1, 'the #2361 shape blocks exactly once: ' + JSON.stringify(r));
  }
  {
    // A green release-staging PR -> clean.
    const r = evaluateSourcePRs({
      pulls: [releasePR(2360)],
      checkRunsByPR: new Map([[2360, [dashboardRun('success')]]]),
    });
    assert(r.problems.length === 0, 'a green release-staging PR passes: ' + JSON.stringify(r));
  }
  {
    // A non-release PR sharing the association list is ignored entirely, even
    // when it has no dashboard run of its own.
    const r = evaluateSourcePRs({
      pulls: [releasePR(2360), ordinaryPR(9)],
      checkRunsByPR: new Map([
        [2360, [dashboardRun('success')]],
        [9, []],
      ]),
    });
    assert(r.problems.length === 0 && r.checked === 1, 'an ordinary co-associated PR is not checked: ' + JSON.stringify(r));
  }

  process.stdout.write('::notice::release-source-pr-dashboard-gate --self-test: all assertions passed\n');
}

// ---------------------------------------------------------------------------
// dispatcher
// ---------------------------------------------------------------------------

const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;

if (invokedDirectly) {
  if (process.argv.includes('--self-test')) {
    selfTest();
    process.exit(0);
  }
  main().catch((e) => {
    ghError(`release-source-pr-dashboard-gate failed: ${e.message}`);
    process.exit(1);
  });
}
