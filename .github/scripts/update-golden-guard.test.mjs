import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  UnresolvableMergeGroupError,
  findOpenPRsForBranch,
  findPRHeadBranch,
  listInFlightRuns,
  parseQueuedPRNumber,
  parseTargetBranch,
  resolveGuardedBranch,
  runForWorkflowRunEvent,
  runTargetsBranch,
  setCommitStatus,
  waitForBranchClear,
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

test('waitForBranchClear passes immediately when nothing targets the branch', async () => {
  let calls = 0;
  const result = await waitForBranchClear({
    listRuns: async () => {
      calls += 1;
      return [run({ display_title: 'update-golden[other-branch]' })];
    },
    branch: 'fix/example',
    sleep: async () => assert.fail('must not sleep when the branch is already clear'),
  });
  assert.deepEqual(result, { clear: true, waitedMs: 0, runs: [] });
  assert.equal(calls, 1);
});

test('waitForBranchClear polls until the matching run leaves the in-flight list', async () => {
  const responses = [
    [run()], // in_progress
    [run({ status: 'queued' })], // still there, now queued (serialised second dispatch)
    [], // finished — no longer in_progress or queued
  ];
  let sleeps = 0;
  const result = await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    pollIntervalMs: 5,
    sleep: async () => {
      sleeps += 1;
    },
    now: () => 0,
  });
  assert.equal(result.clear, true);
  assert.equal(sleeps, 2);
});

test('waitForBranchClear fails closed once the deadline passes with a run still in flight', async () => {
  let ticks = 0;
  const result = await waitForBranchClear({
    listRuns: async () => [run()],
    branch: 'fix/example',
    pollIntervalMs: 10,
    maxWaitMs: 25,
    sleep: async () => {
      ticks += 10;
    },
    now: () => ticks,
  });
  assert.equal(result.clear, false);
  assert.equal(result.timedOut, true);
  assert.equal(result.runs.length, 1);
});

test('waitForBranchClear ignores a run for a branch whose name is a substring of this one', async () => {
  const result = await waitForBranchClear({
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]' })],
    branch: 'fix/example-longer',
    sleep: async () => assert.fail('must not sleep — the only run present targets a different branch'),
  });
  assert.equal(result.clear, true);
});

test('waitForBranchClear treats several in-flight runs (serialised dispatches) as one hazard', async () => {
  const responses = [
    [run({ html_url: 'https://…/1' }), run({ html_url: 'https://…/2', status: 'queued' })],
    [],
  ];
  let sleeps = 0;
  const result = await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    sleep: async () => {
      sleeps += 1;
    },
    now: () => 0,
  });
  assert.equal(result.clear, true);
  assert.equal(sleeps, 1);
});

test('waitForBranchClear calls onWaiting with the matching runs while polling', async () => {
  const responses = [[run()], []];
  const seen = [];
  await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    sleep: async () => {},
    now: () => 0,
    onWaiting: (runs) => seen.push(runs.length),
  });
  assert.deepEqual(seen, [1]);
});

test('waitForBranchClear sees a merely-"requested" run as still in flight (#2350 narrower race)', async () => {
  // Finding #11: before IN_FLIGHT_STATUSES included 'requested', a poll
  // landing in the window between dispatch creation and runner pickup would
  // have missed this run entirely (listRuns only ever queried in_progress
  // and queued) and reported a false-clear.
  const responses = [[run({ status: 'requested' })], []];
  let sleeps = 0;
  const result = await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    sleep: async () => {
      sleeps += 1;
    },
    now: () => 0,
  });
  assert.equal(result.clear, true);
  assert.equal(sleeps, 1, 'must have polled once more instead of clearing immediately on a "requested" run');
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
    fetchJSON: async (url) => {
      const status = new URL(url).searchParams.get('status');
      seenStatuses.push(status);
      return { workflow_runs: [run({ status, html_url: `https://…/${status}` })] };
    },
  });
  assert.deepEqual(seenStatuses.sort(), ['in_progress', 'queued', 'requested']);
  assert.equal(runs.length, 3);
});

test('findOpenPRsForBranch queries the Pulls API scoped to owner:branch and returns the array', async () => {
  let seenURL;
  const prs = await findOpenPRsForBranch({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    branch: 'fix/example',
    fetchJSON: async (url) => {
      seenURL = url;
      return [{ number: 42, head: { sha: 'abc123' } }];
    },
  });
  assert.match(seenURL, /\/repos\/tsouza\/cerberus\/pulls\?state=open&head=tsouza%3Afix%2Fexample/);
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
        fetchJSON: async () => ({ message: 'not found' }),
      }),
    /unexpected response listing open PRs/,
  );
});

test('setCommitStatus POSTs to the Statuses API with the fixed context and a truncated description', async () => {
  let seenURL;
  let seenInit;
  await setCommitStatus({
    api: 'https://api.github.com',
    repo: 'tsouza/cerberus',
    token: 't',
    sha: 'deadbeef',
    state: 'pending',
    description: 'x'.repeat(200),
    targetUrl: 'https://…/run/1',
    postJSON: async (url, token, init) => {
      seenURL = url;
      seenInit = init;
      return null;
    },
  });
  assert.equal(seenURL, 'https://api.github.com/repos/tsouza/cerberus/statuses/deadbeef');
  assert.equal(seenInit.method, 'POST');
  const body = JSON.parse(seenInit.body);
  assert.equal(body.state, 'pending');
  assert.equal(body.context, 'update-golden-guard');
  assert.equal(body.description.length, 140);
  assert.equal(body.target_url, 'https://…/run/1');
});

test('runForWorkflowRunEvent is a no-op when the display_title does not match the update-golden[<branch>] shape', async () => {
  let findCalled = false;
  let pushCalled = false;
  await runForWorkflowRunEvent({
    env: { WORKFLOW_RUN_DISPLAY_TITLE: 'some other workflow' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => {
      findCalled = true;
      return [];
    },
    pushStatus: async () => {
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
    env: { WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [],
    listRuns: async () => {
      listCalled = true;
      return [];
    },
    pushStatus: async () => {
      pushCalled = true;
    },
  });
  assert.equal(listCalled, false, 'must not bother listing runs once there is no PR to gate');
  assert.equal(pushCalled, false);
});

test('runForWorkflowRunEvent pushes a pending status to every matching open PR while a run is in flight', async () => {
  const pushed = [];
  await runForWorkflowRunEvent({
    env: {
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
    pushStatus: async (args) => pushed.push(args),
  });
  assert.equal(pushed.length, 2);
  assert.equal(pushed[0].state, 'pending');
  assert.equal(pushed[0].sha, 'sha1');
  assert.equal(pushed[1].sha, 'sha2');
  assert.equal(pushed[0].targetUrl, 'https://…/run/9');
});

test('runForWorkflowRunEvent pushes success once no run remains in flight against the branch', async () => {
  const pushed = [];
  await runForWorkflowRunEvent({
    env: { WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [{ number: 1, head: { sha: 'sha1' } }],
    // A DIFFERENT branch's run is still in flight; must not count against ours.
    listRuns: async () => [run({ display_title: 'update-golden[other-branch]', status: 'in_progress' })],
    pushStatus: async (args) => pushed.push(args),
  });
  assert.equal(pushed.length, 1);
  assert.equal(pushed[0].state, 'success');
});

test('runForWorkflowRunEvent stays pending when a serialised second dispatch is still queued behind the first', async () => {
  const pushed = [];
  await runForWorkflowRunEvent({
    env: { WORKFLOW_RUN_DISPLAY_TITLE: 'update-golden[fix/example]' },
    token: 't',
    repo: 'tsouza/cerberus',
    api: 'https://api.github.com',
    findPRs: async () => [{ number: 1, head: { sha: 'sha1' } }],
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]', status: 'queued' })],
    pushStatus: async (args) => pushed.push(args),
  });
  assert.equal(pushed[0].state, 'pending');
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

test('the merge_group poll blocks on a dispatch against the queued PR branch (the #2350 race, on the queue)', async () => {
  // The behavioural pin: resolve the queue branch to the PR's head branch,
  // then feed that branch through the SAME poll the pull_request path uses.
  // A free-pass merge_group implementation would clear here on the first read.
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

  const stillRunning = await waitForBranchClear({
    listRuns: async () => [run()],
    branch,
    pollIntervalMs: 10,
    maxWaitMs: 25,
    sleep: async () => {},
    now: (() => {
      let t = 0;
      return () => (t += 20);
    })(),
  });
  assert.equal(stillRunning.clear, false, 'a dispatch against the queued PR branch must hold the queue entry');
  assert.equal(stillRunning.timedOut, true);

  const cleared = await waitForBranchClear({
    listRuns: async () => [run({ display_title: 'update-golden[unrelated]' })],
    branch,
    sleep: async () => assert.fail('must not sleep once nothing targets the queued PR branch'),
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

/** A stub GitHub API serving just the two endpoints the queue path calls. */
async function withStubAPI(inFlightRuns, body) {
  const server = createServer((req, res) => {
    const url = new URL(req.url, 'http://stub');
    res.setHeader('content-type', 'application/json');
    if (url.pathname.endsWith(`/pulls/${QUEUED_PR}`)) {
      res.end(JSON.stringify({ number: QUEUED_PR, head: { ref: QUEUED_PR_BRANCH } }));
      return;
    }
    if (url.pathname.endsWith('/actions/workflows/update-golden.yml/runs')) {
      res.end(JSON.stringify({ workflow_runs: inFlightRuns }));
      return;
    }
    res.statusCode = 404;
    res.end(JSON.stringify({ message: `unstubbed ${url.pathname}` }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    return await body(`http://127.0.0.1:${server.address().port}`);
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

test('merge_group: the guard still FAILS (exit 1) while a dispatch targets the queued PR branch', async () => {
  const result = await withStubAPI(
    [{ display_title: `update-golden[${QUEUED_PR_BRANCH}]`, status: 'in_progress', html_url: 'https://…/run/1' }],
    (api) => runGuard(mergeGroupEnv(api, { POLL_INTERVAL_MS: '1', MAX_WAIT_MS: '1' })),
  );
  assert.equal(result.code, 1, `guard exited ${result.code} — a queue entry must not merge over a live dispatch`);
  assert.match(result.stdout, /timed out/);
});

test('pull_request: the guard still FAILS (exit 1) while a dispatch targets the PR branch (#2350)', async () => {
  const declared = workflowStepEnv('pull_request');
  assert.deepEqual(Object.keys(declared).sort(), ['BRANCH', 'GH_TOKEN', 'REPO']);
  const result = await withStubAPI(
    [{ display_title: 'update-golden[fix/example]', status: 'queued', html_url: 'https://…/run/1' }],
    (api) =>
      runGuard({
        GH_TOKEN: 'stub-token',
        REPO: 'tsouza/cerberus',
        BRANCH: 'fix/example',
        GITHUB_EVENT_NAME: 'pull_request',
        API_URL: api,
        POLL_INTERVAL_MS: '1',
        MAX_WAIT_MS: '1',
      }),
  );
  assert.equal(result.code, 1, 'the original #2350 block must still be reachable');
  assert.match(result.stdout, /timed out/);
});
