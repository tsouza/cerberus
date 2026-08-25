import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  findOpenPRsForBranch,
  listInFlightRuns,
  parseTargetBranch,
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
