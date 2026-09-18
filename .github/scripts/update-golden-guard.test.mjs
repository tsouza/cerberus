import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  UnresolvableMergeGroupError,
  checkBranchClear,
  createCheckRun,
  findOpenPRsForBranch,
  findPRHeadBranch,
  guardVerdict,
  listInFlightRuns,
  parseQueuedPRNumber,
  parseTargetBranch,
  resolveGuardedBranch,
  runForWorkflowRunEvent,
  runTargetsBranch,
} from './update-golden-guard.mjs';

function run(overrides = {}) {
  return {
    display_title: 'update-golden[fix/example]',
    status: 'in_progress',
    html_url: 'https://github.com/tsouza/cerberus/actions/runs/1',
    ...overrides,
  };
}

test('runTargetsBranch matches the exact run-name shape update-golden.yml stamps', () => {
  assert.equal(runTargetsBranch('update-golden[fix/example]', 'fix/example'), true);
});

test('runTargetsBranch does not match a different branch, even a prefix of it', () => {
  assert.equal(runTargetsBranch('update-golden[fix/example-extra]', 'fix/example'), false);
  assert.equal(runTargetsBranch('update-golden[fix/example]', 'fix/example-extra'), false);
});

test('runTargetsBranch does not match an unrelated display title', () => {
  assert.equal(runTargetsBranch('update-golden', 'fix/example'), false);
  assert.equal(runTargetsBranch('some other workflow', 'fix/example'), false);
});

test('checkBranchClear passes on one snapshot when nothing targets the branch', async () => {
  let calls = 0;
  const result = await checkBranchClear({
    listRuns: async () => {
      calls += 1;
      return [run({ display_title: 'update-golden[other-branch]' })];
    },
    branch: 'fix/example',
  });
  assert.deepEqual(result, { clear: true, runs: [] });
  assert.equal(calls, 1, 'must take exactly one snapshot — no retry loop');
});

test('checkBranchClear reports not-clear immediately, without retrying, when a run is in flight', async () => {
  let calls = 0;
  const result = await checkBranchClear({
    listRuns: async () => {
      calls += 1;
      return [run()];
    },
    branch: 'fix/example',
  });
  assert.equal(result.clear, false);
  assert.equal(result.runs.length, 1);
  assert.equal(calls, 1, 'must not poll — one snapshot is the whole check');
});

test('checkBranchClear ignores a run for a branch whose name is a substring of this one', async () => {
  const result = await checkBranchClear({
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]' })],
    branch: 'fix/example-longer',
  });
  assert.equal(result.clear, true);
});

test('checkBranchClear treats several in-flight runs (serialised dispatches) as one hazard', async () => {
  const result = await checkBranchClear({
    listRuns: async () => [run({ html_url: 'https://…/1' }), run({ html_url: 'https://…/2', status: 'queued' })],
    branch: 'fix/example',
  });
  assert.equal(result.clear, false);
  assert.equal(result.runs.length, 2);
});

test('checkBranchClear sees a merely-"requested" run as still in flight (#2350 narrower race)', async () => {
  // Finding #11: before IN_FLIGHT_STATUSES included 'requested', a snapshot
  // landing in the window between dispatch creation and runner pickup would
  // have missed this run entirely (listRuns only ever queried in_progress
  // and queued) and reported a false-clear.
  const result = await checkBranchClear({
    listRuns: async () => [run({ status: 'requested' })],
    branch: 'fix/example',
  });
  assert.equal(result.clear, false);
  assert.equal(result.runs.length, 1);
});

test('parseTargetBranch extracts the branch from the update-golden[<branch>] shape', () => {
  assert.equal(parseTargetBranch('update-golden[fix/example]'), 'fix/example');
  assert.equal(parseTargetBranch('update-golden[a/b/c]'), 'a/b/c');
});

test('parseTargetBranch returns null for anything that does not have the exact shape', () => {
  assert.equal(parseTargetBranch('update-golden'), null);
  assert.equal(parseTargetBranch('update-golden[]'.slice(0, -1)), null);
  assert.equal(parseTargetBranch('some other workflow'), null);
  assert.equal(parseTargetBranch(''), null);
  assert.equal(parseTargetBranch(undefined), null);
  assert.equal(parseTargetBranch(null), null);
});

test('listInFlightRuns queries requested, in_progress and queued — one request per status', async () => {
  const seenStatuses = [];
  const runs = await listInFlightRuns({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    fetchPages: async (url, _token, pick) => {
      const status = new URL(url).searchParams.get('status');
      seenStatuses.push(status);
      return pick({ workflow_runs: [run({ status, html_url: `https://…/${status}` })] });
    },
  });
  assert.deepEqual(seenStatuses.sort(), ['in_progress', 'queued', 'requested']);
  assert.equal(runs.length, 3);
});

test('listInFlightRuns walks every page of each status rather than reading the first hundred', async () => {
  // The list endpoints used to be read as one `per_page=100` request with no
  // page loop: a busy queue's 101st in-flight run was invisible to the guard.
  const pageOf = (n, offset) => Array.from({ length: n }, (_, i) => run({ id: offset + i }));
  const runs = await listInFlightRuns({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    fetchPages: async (url, _token, pick) => {
      const status = new URL(url).searchParams.get('status');
      const pages = status === 'queued' ? [pageOf(100, 0), pageOf(1, 100)] : [pageOf(1, 0)];
      return pages.flatMap((workflow_runs) => pick({ workflow_runs }));
    },
  });
  assert.equal(runs.length, 101 + 1 + 1);
});

test('listInFlightRuns rejects a page without a workflow_runs array', async () => {
  await assert.rejects(
    () =>
      listInFlightRuns({
        api: 'https://api.github.com',
        repo: 'tsouza/cerberus',
        token: 't',
        fetchPages: async (_url, _token, pick) => pick({ message: 'not found' }),
      }),
    /unexpected response listing/,
  );
});

test('findOpenPRsForBranch queries the Pulls API scoped to owner:branch and returns the array', async () => {
  let seenURL;
  const prs = await findOpenPRsForBranch({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    branch: 'fix/example',
    fetchPages: async (url, _token, pick) => {
      seenURL = url;
      return pick([{ number: 42, head: { sha: 'abc123' } }]);
    },
  });
  assert.match(seenURL, /\/repos\/tsouza\/cerberus\/pulls\?state=open&head=tsouza%3Afix%2Fexample$/);
  assert.deepEqual(prs, [{ number: 42, head: { sha: 'abc123' } }]);
});

test('findOpenPRsForBranch rejects a non-array response', async () => {
  await assert.rejects(
    () =>
      findOpenPRsForBranch({
        api: 'https://api.github.com',
        repo: 'tsouza/cerberus',
        token: 't',
        branch: 'fix/example',
        fetchPages: async (_url, _token, pick) => pick({ message: 'not found' }),
      }),
    /unexpected response listing open PRs/,
  );
});

test('createCheckRun POSTs a check-run named update-golden-guard to the Checks API, never a commit status', async () => {
  let seenURL;
  let seenInit;
  await createCheckRun({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    sha: 'deadbeef',
    verdict: { status: 'completed', conclusion: 'success', title: 'clear', summary: 'nothing in flight' },
    detailsUrl: 'https://…/run/1',
    postJSON: async (url, token, init) => {
      seenURL = url;
      seenInit = init;
      return { id: 1 };
    },
  });
  assert.equal(seenURL, 'https://api.github.com/repos/tsouza/cerberus/check-runs');
  assert.equal(seenInit.method, 'POST');
  const body = JSON.parse(seenInit.body);
  assert.equal(body.name, 'update-golden-guard');
  assert.equal(body.head_sha, 'deadbeef');
  assert.equal(body.status, 'completed');
  assert.equal(body.conclusion, 'success');
  assert.equal(body.details_url, 'https://…/run/1');
  assert.deepEqual(body.output, { title: 'clear', summary: 'nothing in flight' });
});

test('createCheckRun sends no conclusion on an in_progress check-run (the API rejects one)', async () => {
  let body;
  await createCheckRun({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    sha: 'deadbeef',
    verdict: { status: 'in_progress', title: 'busy', summary: 'in flight' },
    postJSON: async (_url, _token, init) => {
      body = JSON.parse(init.body);
      return { id: 1 };
    },
  });
  assert.equal(body.status, 'in_progress');
  assert.equal('conclusion' in body, false);
  assert.equal('details_url' in body, false);
});

test('guardVerdict is in_progress while any run still targets the branch, whatever the completed run concluded', () => {
  const verdict = guardVerdict({
    branch: 'fix/example',
    action: 'completed',
    conclusion: 'success',
    inFlight: [run({ html_url: 'https://…/run/2', status: 'queued' })],
    runUrl: 'https://…/run/1',
  });
  assert.equal(verdict.status, 'in_progress');
  assert.equal(verdict.conclusion, undefined);
  assert.match(verdict.summary, /https:\/\/…\/run\/2/, 'must name the run still in flight');
});

test('guardVerdict is success once nothing is in flight and the completed run succeeded', () => {
  const verdict = guardVerdict({
    branch: 'fix/example',
    action: 'completed',
    conclusion: 'success',
    inFlight: [],
    runUrl: 'https://…/run/1',
  });
  assert.deepEqual([verdict.status, verdict.conclusion], ['completed', 'success']);
});

test('guardVerdict FAILS, naming the run, when the completed dispatch was cancelled — the goldens are still stale', () => {
  // Before this pinned the conclusion, the completed handler cleared the guard
  // on ANY completed run: a dispatch cancelled mid-regeneration pushed nothing,
  // yet the PR went green as though its goldens had been refreshed.
  for (const conclusion of ['cancelled', 'failure', 'timed_out']) {
    const verdict = guardVerdict({
      branch: 'fix/example',
      action: 'completed',
      conclusion,
      inFlight: [],
      runUrl: 'https://…/run/1',
    });
    assert.deepEqual([verdict.status, verdict.conclusion], ['completed', 'failure'], conclusion);
    assert.match(verdict.summary, new RegExp(conclusion), 'must say how the dispatch ended');
    assert.match(verdict.summary, /https:\/\/…\/run\/1/, 'must name the dispatch that did not land');
  }
});

test('guardVerdict does not read a conclusion on `requested` — the run has none yet', () => {
  // A `requested` event with a momentarily empty in-flight list (the run is
  // the one being requested) must not be mistaken for a failed completion.
  const verdict = guardVerdict({
    branch: 'fix/example',
    action: 'requested',
    conclusion: undefined,
    inFlight: [],
    runUrl: 'https://…/run/1',
  });
  assert.deepEqual([verdict.status, verdict.conclusion], ['completed', 'success']);
});

test('runForWorkflowRunEvent is a no-op when the display_title does not match the update-golden[<branch>] shape', async () => {
  let findCalled = false;
  let pushCalled = false;
  await runForWorkflowRunEvent({
    env: { WORKFLOW_RUN_ACTION: 'completed', WORKFLOW_RUN_CONCLUSION: 'success', WORKFLOW_RUN_DISPLAY_TITLE: 'some other workflow' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => {
      findCalled = true;
      return [];
    },
    postCheck: async () => {
      pushCalled = true;
    },
  });
  assert.equal(findCalled, false);
  assert.equal(pushCalled, false);
});

test('runForWorkflowRunEvent is a no-op when no open PR has the targeted branch', async () => {
  let listCalled = false;
  let pushCalled = false;
  await runForWorkflowRunEvent({
    env: { WORKFLOW_RUN_ACTION: 'completed', WORKFLOW_RUN_CONCLUSION: 'success', WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [],
    listRuns: async () => {
      listCalled = true;
      return [];
    },
    postCheck: async () => {
      pushCalled = true;
    },
  });
  assert.equal(listCalled, false, 'must not bother listing runs once there is no PR to gate');
  assert.equal(pushCalled, false);
});

test('runForWorkflowRunEvent creates an in_progress check-run on every matching open PR while a run is in flight', async () => {
  const created = [];
  await runForWorkflowRunEvent({
    env: {
      WORKFLOW_RUN_ACTION: 'requested',
      WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]',
      WORKFLOW_RUN_HTML_URL: 'https://…/run/9',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [
      { number: 1, head: { sha: 'sha1' } },
      { number: 2, head: { sha: 'sha2' } },
    ],
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]', status: 'in_progress' })],
    postCheck: async (args) => created.push(args),
  });
  assert.equal(created.length, 2);
  assert.equal(created[0].verdict.status, 'in_progress');
  assert.equal(created[0].sha, 'sha1');
  assert.equal(created[1].sha, 'sha2');
  assert.equal(created[0].detailsUrl, 'https://…/run/9');
});

test('runForWorkflowRunEvent creates a success check-run once no run remains in flight and the dispatch succeeded', async () => {
  const created = [];
  await runForWorkflowRunEvent({
    env: {
      WORKFLOW_RUN_ACTION: 'completed',
      WORKFLOW_RUN_CONCLUSION: 'success',
      WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [{ number: 1, head: { sha: 'sha1' } }],
    // A DIFFERENT branch's run is still in flight; must not count against ours.
    listRuns: async () => [run({ display_title: 'update-golden[other-branch]', status: 'in_progress' })],
    postCheck: async (args) => created.push(args),
  });
  assert.equal(created.length, 1);
  assert.deepEqual([created[0].verdict.status, created[0].verdict.conclusion], ['completed', 'success']);
});

test('runForWorkflowRunEvent leaves the guard RED when the completed dispatch was cancelled', async () => {
  const created = [];
  await runForWorkflowRunEvent({
    env: {
      WORKFLOW_RUN_ACTION: 'completed',
      WORKFLOW_RUN_CONCLUSION: 'cancelled',
      WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]',
      WORKFLOW_RUN_HTML_URL: 'https://…/run/9',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [{ number: 1, head: { sha: 'sha1' } }],
    listRuns: async () => [],
    postCheck: async (args) => created.push(args),
  });
  assert.equal(created.length, 1);
  assert.deepEqual([created[0].verdict.status, created[0].verdict.conclusion], ['completed', 'failure']);
  assert.match(created[0].verdict.summary, /cancelled/);
  assert.match(created[0].verdict.summary, /https:\/\/…\/run\/9/);
});

test('runForWorkflowRunEvent stays in_progress when a serialised second dispatch is still queued behind the first', async () => {
  const created = [];
  await runForWorkflowRunEvent({
    env: {
      WORKFLOW_RUN_ACTION: 'completed',
      WORKFLOW_RUN_CONCLUSION: 'success',
      WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [{ number: 1, head: { sha: 'sha1' } }],
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]', status: 'queued' })],
    postCheck: async (args) => created.push(args),
  });
  assert.equal(created[0].verdict.status, 'in_progress');
});

// --- merge_group: the queue's own copy of the poll (see the mjs header) ---

test('parseQueuedPRNumber reads the PR number out of a merge group head ref', () => {
  assert.equal(
    parseQueuedPRNumber(
      'refs/heads/gh-readonly-queue/main/pr-2951-5af3f78d5a1b2c3d4e5f60718293a4b5c6d7e8f9',
      'refs/heads/main',
    ),
    2951,
  );
});

test('parseQueuedPRNumber accepts either ref with or without the refs/heads/ prefix', () => {
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-abc123', 'main'), 7);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-abc123', 'refs/heads/main'), 7);
  assert.equal(parseQueuedPRNumber('refs/heads/gh-readonly-queue/main/pr-7-abc123', 'main'), 7);
});

test('parseQueuedPRNumber anchors on the group base ref, so a base branch with slashes still splits', () => {
  // A regex that guessed where the base name ended would read `x` as part of
  // the pull-request segment (or fail); anchoring on base_ref cannot.
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/release/1.4.x/pr-31-deadbeef', 'refs/heads/release/1.4.x'), 31);
});

test('parseQueuedPRNumber refuses a ref that belongs to a DIFFERENT base branch', () => {
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/other/pr-7-abc123', 'refs/heads/main'), null);
});

test('parseQueuedPRNumber returns null for anything without the queue-branch shape', () => {
  assert.equal(parseQueuedPRNumber('refs/heads/main', 'refs/heads/main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr--abc123', 'main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7', 'main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-zznothex', 'main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-abc123/extra', 'main'), null);
  assert.equal(parseQueuedPRNumber('', 'main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-abc123', ''), null);
  assert.equal(parseQueuedPRNumber(undefined, 'main'), null);
  assert.equal(parseQueuedPRNumber('gh-readonly-queue/main/pr-7-abc123', null), null);
});

test('findPRHeadBranch returns the head branch of one pull request by number', async () => {
  let seenURL;
  const branch = await findPRHeadBranch({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    number: 42,
    fetchJSON: async (url) => {
      seenURL = url;
      return { number: 42, head: { ref: 'fix/example', sha: 'abc123' } };
    },
  });
  assert.equal(seenURL, 'https://api.github.com/repos/tsouza/cerberus/pulls/42');
  assert.equal(branch, 'fix/example');
});

test('findPRHeadBranch rejects a response with no head.ref rather than guarding an empty branch', async () => {
  await assert.rejects(
    () =>
      findPRHeadBranch({
        api: 'https://api.github.com',
        repo: 'tsouza/cerberus',
        token: 't',
        number: 42,
        fetchJSON: async () => ({ message: 'Not Found' }),
      }),
    /no head\.ref/,
  );
});

test('resolveGuardedBranch on pull_request keeps taking the branch straight from BRANCH', async () => {
  const branch = await resolveGuardedBranch({
    eventName: 'pull_request',
    env: { BRANCH: 'fix/example' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findHeadBranch: async () => assert.fail('the pull_request path must not call the Pulls API'),
  });
  assert.equal(branch, 'fix/example');
});

test('resolveGuardedBranch on merge_group guards the QUEUED PR branch, not the projected ref', async () => {
  let seenNumber;
  const branch = await resolveGuardedBranch({
    eventName: 'merge_group',
    env: {
      MERGE_GROUP_HEAD_REF: 'refs/heads/gh-readonly-queue/main/pr-2951-abc123',
      MERGE_GROUP_BASE_REF: 'refs/heads/main',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findHeadBranch: async ({ number }) => {
      seenNumber = number;
      return 'fix/example';
    },
  });
  assert.equal(seenNumber, 2951);
  assert.equal(branch, 'fix/example', 'must guard the branch a merge would delete, not gh-readonly-queue/…');
});

test('resolveGuardedBranch FAILS an unresolvable merge group instead of certifying it', async () => {
  await assert.rejects(
    () =>
      resolveGuardedBranch({
        eventName: 'merge_group',
        env: {
          MERGE_GROUP_HEAD_REF: 'refs/heads/some/unexpected/ref',
          MERGE_GROUP_BASE_REF: 'refs/heads/main',
        },
        token: 't',
        repo: 'tsouza/cerberus',
        api: 'https://api.github.com',
        findHeadBranch: async () => assert.fail('must not reach the Pulls API with an unresolved ref'),
      }),
    UnresolvableMergeGroupError,
  );
});

test('the merge_group snapshot catches a dispatch against the queued PR branch (the #2350 race, on the queue)', async () => {
  // The behavioural pin: resolve the queue branch to the PR's head branch,
  // then feed that branch through the SAME snapshot check the pull_request
  // path uses. A free-pass merge_group implementation would clear here.
  const branch = await resolveGuardedBranch({
    eventName: 'merge_group',
    env: {
      MERGE_GROUP_HEAD_REF: 'gh-readonly-queue/main/pr-2951-abc123',
      MERGE_GROUP_BASE_REF: 'refs/heads/main',
    },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findHeadBranch: async () => 'fix/example',
  });

  const stillRunning = await checkBranchClear({
    listRuns: async () => [run()],
    branch,
  });
  assert.equal(stillRunning.clear, false, 'a dispatch against the queued PR branch must fail the queue entry');

  const cleared = await checkBranchClear({
    listRuns: async () => [run({ display_title: 'update-golden[unrelated]' })],
    branch,
  });
  assert.equal(cleared.clear, true);
});


// --- end-to-end: the workflow's own env wiring, exercised against a stub API ---
//
// The unit tests above drive exported functions directly, so they cannot see a
// name that disagrees between update-golden-guard.yml and this script — a
// MERGE_GROUP_HEAD_REF the workflow spells one way and `required()` reads
// another would leave the queue path failing at runtime with every unit test
// green. These two tests close that: they read the ACTUAL env block out of the
// workflow, resolve each `${{ … }}` expression against a synthetic merge_group
// payload, and run the real script as a subprocess against a stub GitHub API.
// One asserts it reports (exit 0) on a clear queue; the other asserts it still
// refuses (exit 1) while a dispatch targets the queued PR's branch.

const HERE = dirname(fileURLToPath(import.meta.url));
const GUARD_SCRIPT = join(HERE, 'update-golden-guard.mjs');
const GUARD_WORKFLOW = join(HERE, '..', 'workflows', 'update-golden-guard.yml');

/**
 * The `env:` mapping of the workflow step whose `if:` selects `eventName`,
 * as { NAME: '<the ${{ … }} expression or literal>' }. Deliberately a small
 * text scan rather than a YAML dependency: this file may use node: builtins
 * only.
 */
// `env:` entries sit two levels under a step's own dash, which in this
// workflow puts their keys at column 10. Anchoring on that column is what ends
// the scan at the next `run:` line instead of running past it.
const STEP_ENV_KEY_RE = /^ {10}([A-Z0-9_]+):\s*(.+)$/;

function workflowStepEnv(eventName) {
  const text = readFileSync(GUARD_WORKFLOW, 'utf8');
  const lines = text.split('\n');
  const guard = `if: github.event_name == '${eventName}'`;
  const at = lines.findIndex((l) => l.trim() === guard);
  assert.notEqual(at, -1, `no step in update-golden-guard.yml guarded by ${guard}`);
  const envAt = lines.findIndex((l, i) => i > at && l.trim() === 'env:');
  assert.notEqual(envAt, -1, `the ${eventName} step declares no env: block`);
  const env = {};
  for (const line of lines.slice(envAt + 1)) {
    const m = STEP_ENV_KEY_RE.exec(line);
    if (m === null) break;
    env[m[1]] = m[2].trim();
  }
  return env;
}

/** Resolve `${{ github.event.merge_group.head_ref }}` against a payload. */
function resolveExpression(expression, context) {
  const m = /^\$\{\{\s*([A-Za-z0-9_.]+)\s*\}\}$/.exec(expression);
  if (m === null) return expression;
  let value = context;
  for (const segment of m[1].split('.')) value = value?.[segment];
  assert.notEqual(value, undefined, `workflow expression ${expression} resolves to nothing`);
  return String(value);
}

const QUEUED_PR = 2951;
const QUEUED_PR_BRANCH = 'fix/queued-example';
const HTTP_CREATED = 201;

/**
 * A stub GitHub API serving the read endpoints the three paths call — the
 * queued PR by number, the open-PR list by head branch, the in-flight run
 * list — and accepting any write with 201. Every request is recorded (method,
 * path, parsed JSON body) and handed to `body` alongside the base URL, so a
 * test can assert on what the script WROTE, not only on its exit code.
 */
async function withStubAPI(inFlightRuns, body, { openPRs = [] } = {}) {
  const requests = [];
  const server = createServer((req, res) => {
    const url = new URL(req.url, 'http://stub');
    let raw = '';
    req.on('data', (chunk) => (raw += chunk));
    req.on('end', () => {
      requests.push({ method: req.method, path: url.pathname, body: raw === '' ? null : JSON.parse(raw) });
      res.setHeader('content-type', 'application/json');
      if (req.method === 'POST') {
        res.statusCode = HTTP_CREATED;
        res.end(JSON.stringify({ id: requests.length }));
        return;
      }
      if (url.pathname.endsWith(`/pulls/${QUEUED_PR}`)) {
        res.end(JSON.stringify({ number: QUEUED_PR, head: { ref: QUEUED_PR_BRANCH } }));
        return;
      }
      if (url.pathname.endsWith('/pulls')) {
        res.end(JSON.stringify(openPRs));
        return;
      }
      if (url.pathname.endsWith('/actions/workflows/update-golden.yml/runs')) {
        res.end(JSON.stringify({ workflow_runs: inFlightRuns }));
        return;
      }
      res.statusCode = 404;
      res.end(JSON.stringify({ message: `unstubbed ${url.pathname}` }));
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    return await body(`http://127.0.0.1:${server.address().port}`, requests);
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
}

function runGuard(env) {
  return new Promise((resolve) => {
    execFile('node', [GUARD_SCRIPT], { env: { ...process.env, ...env } }, (err, stdout, stderr) =>
      resolve({ code: err?.code ?? 0, stdout, stderr }),
    );
  });
}

/** The env the merge_group step actually exports, against a synthetic payload. */
function mergeGroupEnv(api, extra = {}) {
  const context = {
    github: {
      token: 'stub-token',
      repository: 'tsouza/cerberus',
      event: {
        merge_group: {
          head_ref: `refs/heads/gh-readonly-queue/main/pr-${QUEUED_PR}-abc123`,
          base_ref: 'refs/heads/main',
        },
      },
    },
  };
  const declared = workflowStepEnv('merge_group');
  // Pinned exactly, in both directions: a name the workflow stops exporting
  // breaks the script's `required()` read, and a name it exports that the
  // script never reads is dead wiring that reads as coverage.
  assert.deepEqual(Object.keys(declared).sort(), [
    'GH_TOKEN',
    'MERGE_GROUP_BASE_REF',
    'MERGE_GROUP_HEAD_REF',
    'REPO',
  ]);
  const env = {};
  for (const [name, expression] of Object.entries(declared)) {
    env[name] = resolveExpression(expression, context);
  }
  return { ...env, GITHUB_EVENT_NAME: 'merge_group', API_URL: api, ...extra };
}

test('merge_group: the guard REPORTS (exit 0) when nothing targets the queued PR branch', async () => {
  const result = await withStubAPI([{ display_title: 'update-golden[some-other-branch]', status: 'in_progress' }], (api) =>
    runGuard(mergeGroupEnv(api)),
  );
  assert.equal(result.code, 0, `guard exited ${result.code}\n${result.stdout}\n${result.stderr}`);
  assert.match(result.stdout, new RegExp(`PR #${QUEUED_PR}`), 'must say which queued PR it resolved');
  assert.match(result.stdout, new RegExp(QUEUED_PR_BRANCH), 'must guard the PR head branch, not the queue ref');
});

test('merge_group: the guard still FAILS (exit 1) immediately while a dispatch targets the queued PR branch', async () => {
  const started = Date.now();
  const result = await withStubAPI(
    [{ display_title: `update-golden[${QUEUED_PR_BRANCH}]`, status: 'in_progress', html_url: 'https://…/run/1' }],
    (api) => runGuard(mergeGroupEnv(api)),
  );
  assert.equal(result.code, 1, `guard exited ${result.code} — a queue entry must not merge over a live dispatch`);
  assert.match(result.stdout, /is in flight/);
  assert.match(result.stdout, /re-added to the merge queue/, 'must name the merge_group-specific recovery step');
  assert.ok(Date.now() - started < 5_000, 'must fail on one snapshot, not block waiting for the dispatch');
});

test('pull_request: the guard still FAILS (exit 1) immediately while a dispatch targets the PR branch (#2350)', async () => {
  const declared = workflowStepEnv('pull_request');
  assert.deepEqual(Object.keys(declared).sort(), ['BRANCH', 'GH_TOKEN', 'REPO']);
  const started = Date.now();
  const result = await withStubAPI(
    [{ display_title: 'update-golden[fix/example]', status: 'queued', html_url: 'https://…/run/1' }],
    (api) =>
      runGuard({
        GH_TOKEN: 'stub-token',
        REPO: 'tsouza/cerberus',
        BRANCH: 'fix/example',
        GITHUB_EVENT_NAME: 'pull_request',
        API_URL: api,
      }),
  );
  assert.equal(result.code, 1, 'the original #2350 block must still be reachable');
  assert.match(result.stdout, /is in flight/);
  assert.match(result.stdout, /flip back to success on its own/, 'must name the self-heal path, not tell the reader to wait');
  assert.ok(Date.now() - started < 5_000, 'must fail on one snapshot, not block waiting for the dispatch');
});

// --- workflow_run end-to-end: the guard writes a CHECK-RUN, not a commit status ---
//
// On PR #3557 (head 371f84db) the pull_request job's check-run failed fast
// while a dispatch was in flight; the completed handler then posted a commit
// STATUS `success` under the same name, and the check-run stayed `failure`:
// statuses and check-runs are distinct objects, and nothing in GitHub makes
// one supersede the other. These tests run the real script with the env block
// the workflow actually exports for `workflow_run` and assert on the recorded
// API calls: a successful dispatch yields a `success` check-run named
// `update-golden-guard` on the PR's head SHA, a cancelled one leaves the guard
// red and names the run, and no request ever reaches the Statuses API.

const TARGET_PR = { number: 3557, head: { sha: '371f84db' } };
const TARGET_BRANCH = 'fix/example';
const DISPATCH_URL = 'https://github.com/tsouza/cerberus/actions/runs/1';

/** The env the workflow_run step actually exports, against a synthetic payload. */
function workflowRunEnv(api, { action, conclusion }) {
  const context = {
    github: {
      token: 'stub-token',
      repository: 'tsouza/cerberus',
      event: {
        action,
        workflow_run: {
          conclusion,
          display_title: `update-golden[${TARGET_BRANCH}]`,
          html_url: DISPATCH_URL,
        },
      },
    },
  };
  const declared = workflowStepEnv('workflow_run');
  assert.deepEqual(Object.keys(declared).sort(), [
    'GH_TOKEN',
    'REPO',
    'WORKFLOW_RUN_ACTION',
    'WORKFLOW_RUN_CONCLUSION',
    'WORKFLOW_RUN_DISPLAY_TITLE',
    'WORKFLOW_RUN_HTML_URL',
  ]);
  const env = {};
  for (const [name, expression] of Object.entries(declared)) {
    env[name] = resolveExpression(expression, context);
  }
  return { ...env, GITHUB_EVENT_NAME: 'workflow_run', API_URL: api };
}

function checkRunWrites(requests) {
  return requests.filter((r) => r.method === 'POST' && r.path.endsWith('/check-runs'));
}

/** The recorded requests that hit the Statuses API — matched on a path SEGMENT, not a substring. */
function statusWrites(requests) {
  return requests.filter((r) => r.path.split('/').some((segment) => segment === 'statuses'));
}

/** The whole URLs a check-run summary names, as the tokens between whitespace and parentheses. */
function urlsNamedIn(summary) {
  return summary.split(/[\s()]+/).filter((token) => URL.canParse(token));
}

test('workflow_run: a SUCCESSFUL completed dispatch yields a success check-run on the PR head, and no commit status', async () => {
  const { result, requests } = await withStubAPI(
    [],
    async (api, requests) => ({ result: await runGuard(workflowRunEnv(api, { action: 'completed', conclusion: 'success' })), requests }),
    { openPRs: [TARGET_PR] },
  );
  assert.equal(result.code, 0, `guard exited ${result.code}\n${result.stdout}\n${result.stderr}`);
  const created = checkRunWrites(requests);
  assert.equal(created.length, 1, `expected exactly one check-run create, saw ${JSON.stringify(requests)}`);
  assert.equal(created[0].path, '/repos/tsouza/cerberus/check-runs');
  assert.equal(created[0].body.name, 'update-golden-guard');
  assert.equal(created[0].body.head_sha, TARGET_PR.head.sha);
  assert.equal(created[0].body.status, 'completed');
  assert.equal(created[0].body.conclusion, 'success');
  assert.equal(created[0].body.details_url, DISPATCH_URL);
  assert.deepEqual(statusWrites(requests), [], 'a commit status cannot flip the failed check-run; none may be posted');
});

test('workflow_run: a CANCELLED completed dispatch leaves the guard red, naming the run, and posts no commit status', async () => {
  const { result, requests } = await withStubAPI(
    [],
    async (api, requests) => ({ result: await runGuard(workflowRunEnv(api, { action: 'completed', conclusion: 'cancelled' })), requests }),
    { openPRs: [TARGET_PR] },
  );
  assert.equal(result.code, 0, `guard exited ${result.code}\n${result.stdout}\n${result.stderr}`);
  const created = checkRunWrites(requests);
  assert.equal(created.length, 1);
  assert.equal(created[0].body.head_sha, TARGET_PR.head.sha);
  assert.equal(created[0].body.conclusion, 'failure', 'a cancelled regeneration pushed nothing — the goldens are still stale');
  assert.match(created[0].body.output.summary, /cancelled/);
  assert.ok(
    urlsNamedIn(created[0].body.output.summary).some((u) => u === DISPATCH_URL),
    'must name the cancelled run',
  );
  assert.deepEqual(statusWrites(requests), []);
});

test('workflow_run: a REQUESTED dispatch yields an in_progress check-run with no conclusion', async () => {
  const { result, requests } = await withStubAPI(
    [{ display_title: `update-golden[${TARGET_BRANCH}]`, status: 'requested', html_url: DISPATCH_URL }],
    async (api, requests) => ({ result: await runGuard(workflowRunEnv(api, { action: 'requested', conclusion: '' })), requests }),
    { openPRs: [TARGET_PR] },
  );
  assert.equal(result.code, 0, `guard exited ${result.code}\n${result.stdout}\n${result.stderr}`);
  const created = checkRunWrites(requests);
  assert.equal(created.length, 1);
  assert.equal(created[0].body.status, 'in_progress');
  assert.equal('conclusion' in created[0].body, false);
  assert.deepEqual(statusWrites(requests), []);
});
