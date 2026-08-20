// release-source-pr-dashboard-gate.test.mjs — node:test guard for the #2361
// fix, run on the required `lint` lane.
//
// Why it exists separately from the module's own `--self-test`: that self-test
// only runs inside release.yml's `preflight` job, which fires on push-to-main /
// a maintenance push — never on an ordinary pull_request. Every change here
// would otherwise be unverified until a release is actually cut. This suite
// runs on every PR.
//
// What it pins: the exact #2361 incident shape (a release-staging PR whose own
// `dashboard` check-run is FAILURE) must block; a green one must not; a commit
// with no associated release-staging PR must be a clean no-op (this gate adds,
// it never relaxes, release-preflight.mjs's existing coverage); and the
// network helpers correctly authenticate + paginate.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  DASHBOARD_CHECK_NAME,
  RELEASE_HEAD_REF_PREFIX,
  isReleaseStagingPR,
  releaseStagingSourcePRs,
  latestDashboardCheckRun,
  sourcePRDashboardProblem,
  evaluateSourcePRs,
  associatedPulls,
  checkRunsForSha,
} from './release-source-pr-dashboard-gate.mjs';

const releasePR = (number, ref = 'release/v1.15.0-chart-0.15.0', sha = 'aaaa1111') => ({
  number,
  head: { ref, sha },
});
const ordinaryPR = (number, sha = 'bbbb2222') => ({ number, head: { ref: 'fix/some-bug', sha } });
const run = (conclusion, status = 'completed', id = 1, name = DASHBOARD_CHECK_NAME) => ({
  name,
  status,
  conclusion,
  id,
});

test('RELEASE_HEAD_REF_PREFIX matches dashboard-matrix.mjs / e2e.yml INCLUDE_CRAWL selection', () => {
  assert.equal(RELEASE_HEAD_REF_PREFIX, 'release/');
});

test('isReleaseStagingPR discriminates on head.ref', () => {
  assert.equal(isReleaseStagingPR(releasePR(1)), true);
  assert.equal(isReleaseStagingPR(ordinaryPR(1)), false);
  assert.equal(isReleaseStagingPR({}), false);
  assert.equal(isReleaseStagingPR(undefined), false);
});

test('releaseStagingSourcePRs filters a mixed pulls list', () => {
  assert.deepEqual(
    releaseStagingSourcePRs([releasePR(1), ordinaryPR(2), releasePR(3, 'release/2.0.x-staging', 'cccc')]).map(
      (pr) => pr.number,
    ),
    [1, 3],
  );
  assert.deepEqual(releaseStagingSourcePRs([]), []);
  assert.deepEqual(releaseStagingSourcePRs(undefined), []);
});

test('latestDashboardCheckRun picks the highest id and ignores other names', () => {
  assert.equal(latestDashboardCheckRun([]), null);
  assert.equal(latestDashboardCheckRun([run('success', 'completed', 1, 'lint')]), null);
  const latest = latestDashboardCheckRun([
    run('failure', 'completed', 1),
    run('success', 'completed', 3),
    run('failure', 'completed', 2),
  ]);
  assert.equal(latest.id, 3);
  assert.equal(latest.conclusion, 'success');
});

test('sourcePRDashboardProblem: absence blocks', () => {
  const problem = sourcePRDashboardProblem({ pr: releasePR(2360), checkRuns: [] });
  assert.match(problem, /never posted a "dashboard" check-run/);
  assert.match(problem, /#2361/);
});

test('sourcePRDashboardProblem: still-running blocks', () => {
  const problem = sourcePRDashboardProblem({
    pr: releasePR(2360),
    checkRuns: [run(null, 'in_progress')],
  });
  assert.match(problem, /is still in_progress/);
});

test('sourcePRDashboardProblem: the exact #2361 incident shape (own head commit red) blocks', () => {
  const problem = sourcePRDashboardProblem({
    pr: releasePR(2360),
    checkRuns: [run('failure')],
  });
  assert.match(problem, /concluded failure/);
  assert.match(problem, /native auto-merge does not wait on it/);
});

test('sourcePRDashboardProblem: a completed success does not block', () => {
  assert.equal(sourcePRDashboardProblem({ pr: releasePR(2360), checkRuns: [run('success')] }), null);
});

test('sourcePRDashboardProblem: skipped is not treated as green on a release PR', () => {
  const problem = sourcePRDashboardProblem({ pr: releasePR(2360), checkRuns: [run('skipped')] });
  assert.match(problem, /concluded skipped/);
});

test('sourcePRDashboardProblem: a re-run supersedes an earlier failure by id', () => {
  const problem = sourcePRDashboardProblem({
    pr: releasePR(2360),
    checkRuns: [run('failure', 'completed', 1), run('success', 'completed', 2)],
  });
  assert.equal(problem, null, 'the later, higher-id success must win: ' + problem);
});

test('evaluateSourcePRs: no associated release-staging PR is a clean no-op', () => {
  const r = evaluateSourcePRs({ pulls: [ordinaryPR(1)], checkRunsByPR: new Map() });
  assert.deepEqual(r, { problems: [], checked: 0 });
});

test('evaluateSourcePRs: the #2361 shape — one release-staging PR, its own dashboard run is FAILURE', () => {
  const r = evaluateSourcePRs({
    pulls: [releasePR(2360)],
    checkRunsByPR: new Map([[2360, [run('failure')]]]),
  });
  assert.equal(r.checked, 1);
  assert.equal(r.problems.length, 1);
  assert.match(r.problems[0], /PR #2360/);
});

test('evaluateSourcePRs: a green release-staging PR passes', () => {
  const r = evaluateSourcePRs({
    pulls: [releasePR(2360)],
    checkRunsByPR: new Map([[2360, [run('success')]]]),
  });
  assert.deepEqual(r, { problems: [], checked: 1 });
});

test('evaluateSourcePRs: an ordinary PR sharing the association list is never checked', () => {
  const r = evaluateSourcePRs({
    pulls: [releasePR(2360), ordinaryPR(9)],
    checkRunsByPR: new Map([
      [2360, [run('success')]],
      [9, []], // would be an absence-failure if this PR were (wrongly) checked
    ]),
  });
  assert.deepEqual(r, { problems: [], checked: 1 });
});

test('evaluateSourcePRs: multiple release-staging PRs are each checked independently', () => {
  const r = evaluateSourcePRs({
    pulls: [releasePR(1, 'release/1.15.x-a', 'sha-a'), releasePR(2, 'release/1.15.x-b', 'sha-b')],
    checkRunsByPR: new Map([
      [1, [run('success')]],
      [2, [run('failure')]],
    ]),
  });
  assert.equal(r.checked, 2);
  assert.equal(r.problems.length, 1);
  assert.match(r.problems[0], /PR #2/);
});

// --- network helpers: auth header + pagination, mocked fetch ---------------

test('associatedPulls sends the bearer token and returns the parsed array', async () => {
  let seenUrl = null;
  let seenAuth = null;
  const pulls = await associatedPulls({
    apiBase: 'https://api.invalid',
    repo: 'tsouza/cerberus',
    sha: 'deadbeef',
    headers: { Authorization: 'Bearer test-token' },
    fetchImpl: async (url, options) => {
      seenUrl = url;
      seenAuth = options.headers.Authorization;
      return { ok: true, json: async () => [releasePR(2360)] };
    },
  });
  assert.equal(seenAuth, 'Bearer test-token');
  assert.match(seenUrl, /\/repos\/tsouza\/cerberus\/commits\/deadbeef\/pulls/);
  assert.equal(pulls.length, 1);
});

test('associatedPulls throws on a non-ok response rather than swallowing it', async () => {
  await assert.rejects(
    () =>
      associatedPulls({
        apiBase: 'https://api.invalid',
        repo: 'tsouza/cerberus',
        sha: 'deadbeef',
        headers: {},
        fetchImpl: async () => ({ ok: false, status: 404, statusText: 'Not Found' }),
      }),
    /404 Not Found/,
  );
});

test('checkRunsForSha paginates until a short page', async () => {
  let calls = 0;
  const page1 = Array.from({ length: 100 }, (_, i) => run('success', 'completed', i));
  const page2 = [run('failure', 'completed', 999)];
  const runs = await checkRunsForSha({
    apiBase: 'https://api.invalid',
    repo: 'tsouza/cerberus',
    sha: 'aaaa1111',
    headers: {},
    fetchImpl: async (url) => {
      calls += 1;
      const page = new URL(url).searchParams.get('page');
      return { ok: true, json: async () => ({ check_runs: page === '1' ? page1 : page2 }) };
    },
  });
  assert.equal(calls, 2, 'a full 100-item page must trigger a second fetch');
  assert.equal(runs.length, 101);
});
