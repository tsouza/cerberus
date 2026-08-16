import assert from 'node:assert/strict';
import test from 'node:test';

import {
  CANCEL_EXPRESSION,
  COALESCED_WORKFLOWS,
  GROUP_EXPRESSION,
  MAIN_REF,
  QUICKSTART_CANCEL_EXPRESSION,
  QUICKSTART_GROUP_EXPRESSION,
  auditMainCoalescing,
  coalescingDecision,
  concurrencyGroupValues,
  parseTopLevelConcurrency,
  validateE2EWorkflow,
  validateEnrolledWorkflow,
  validateQuickstartWorkflow,
} from './main-coalescing.mjs';

const decision = (eventName, ref, runId = '17', schedule = '') =>
  coalescingDecision({
    eventName,
    ref,
    workflow: 'deep',
    runId,
    schedule,
  });

test('main pushes replace only main pushes', () => {
  assert.deepEqual(decision('push', MAIN_REF), {
    cancelInProgress: true,
    group: 'deep-latest-main-push',
    reason: 'latest_main_push',
  });
  assert.equal(
    decision('push', MAIN_REF, 'old').group,
    decision('push', MAIN_REF, 'new').group,
  );
});

test('scheduled runs replace only the same scheduled mode', () => {
  const nightly = '17 3 * * *';
  assert.deepEqual(decision('schedule', MAIN_REF, '17', nightly), {
    cancelInProgress: true,
    group: `deep-latest-main-schedule-${nightly}`,
    reason: 'equivalent_schedule',
  });
  assert.equal(
    decision('schedule', MAIN_REF, 'old', nightly).group,
    decision('schedule', MAIN_REF, 'new', nightly).group,
  );
  assert.notEqual(
    decision('schedule', MAIN_REF, 'old', nightly).group,
    decision('schedule', MAIN_REF, 'new', '45 4 * * *').group,
  );
});

test('a main push cannot cancel nightly-only evidence', () => {
  const push = decision('push', MAIN_REF, 'push');
  const nightly = decision('schedule', MAIN_REF, 'nightly', '17 3 * * *');
  assert.equal(push.cancelInProgress, true);
  assert.equal(nightly.cancelInProgress, true);
  assert.notEqual(push.group, nightly.group);
});

test('PR, queue, qualification, maintenance, release, and unknown events never cancel', () => {
  const protectedEvents = [
    'pull_request',
    'pull_request_target',
    'merge_group',
    'workflow_dispatch',
    'workflow_call',
    'release',
    'repository_dispatch',
    'issues',
    'unknown',
    '',
  ];
  const refs = [
    MAIN_REF,
    'refs/heads/release/1.2.x',
    'refs/heads/release/candidate',
    'refs/heads/gh-readonly-queue/main/pr-7-deadbeef',
    'refs/tags/v1.2.3',
    '',
  ];

  for (const eventName of protectedEvents) {
    for (const ref of refs) {
      const first = decision(eventName, ref, '101');
      const second = decision(eventName, ref, '102');
      assert.equal(first.cancelInProgress, false, `${eventName} ${ref}`);
      assert.equal(first.reason, 'distinct_run', `${eventName} ${ref}`);
      assert.notEqual(first.group, second.group, `${eventName} ${ref}`);
    }
  }
});

test('push and schedule fail safe to distinct groups on every non-main ref', () => {
  for (const eventName of ['push', 'schedule']) {
    for (const ref of [
      'refs/heads/release/1.2.x',
      'refs/heads/feature',
      'refs/tags/v1.2.3',
      '',
      undefined,
    ]) {
      const verdict = decision(eventName, ref, '17', '17 3 * * *');
      assert.equal(verdict.cancelInProgress, false, `${eventName} ${ref}`);
      assert.equal(verdict.group, 'deep-17', `${eventName} ${ref}`);
    }
  }
});

test('a malformed schedule without its declared cron fails safe to a unique group', () => {
  const first = decision('schedule', MAIN_REF, '101');
  const second = decision('schedule', MAIN_REF, '102');
  assert.equal(first.cancelInProgress, false);
  assert.equal(second.cancelInProgress, false);
  assert.notEqual(first.group, second.group);
});

test('structural parser reads only the top-level concurrency mapping', () => {
  const parsed = parseTopLevelConcurrency(`name: fixture
concurrency:
  group: ${GROUP_EXPRESSION}
  cancel-in-progress: ${CANCEL_EXPRESSION}
jobs:
  test:
    concurrency:
      group: ignored
      cancel-in-progress: false
`);
  assert.deepEqual(parsed, {
    group: GROUP_EXPRESSION,
    cancelInProgress: CANCEL_EXPRESSION,
  });
});

test('coalescing enrollment scans concurrency groups, not prose', () => {
  assert.deepEqual(
    concurrencyGroupValues(`name: fixture
# latest-main is documentation, not cancellation policy.
concurrency:
  group: fixture-\${{ github.run_id }}
  cancel-in-progress: false
jobs:
  test:
    steps:
      - name: Assert latest-main negative controls
        run: true
`),
    ['fixture-${{ github.run_id }}'],
  );
  assert.deepEqual(
    concurrencyGroupValues(`name: fixture
jobs:
  test:
    concurrency:
      group: >-
        fixture-latest-main
      cancel-in-progress: false
`),
    ['fixture-latest-main'],
  );
});

test('enrollment rejects broadened cancellation and a shared qualification group', () => {
  const valid = `name: fixture
concurrency:
  group: ${GROUP_EXPRESSION}
  cancel-in-progress: ${CANCEL_EXPRESSION}
jobs: {}
`;
  assert.deepEqual(validateEnrolledWorkflow('fixture.yml', valid), []);

  const broad = valid.replace(
    CANCEL_EXPRESSION,
    "${{ github.event_name != 'merge_group' }}",
  );
  assert.match(
    validateEnrolledWorkflow('fixture.yml', broad).join('\n'),
    /cancel-in-progress/,
  );

  const shared = valid.replace(GROUP_EXPRESSION, 'deep-${{ github.ref }}');
  assert.match(
    validateEnrolledWorkflow('fixture.yml', shared).join('\n'),
    /concurrency group/,
  );

  const nestedShared = valid.replace(
    'jobs: {}',
    `jobs:
  qualification:
    concurrency:
      group: qualification-\${{ github.ref }}
      cancel-in-progress: false`,
  );
  assert.match(
    validateEnrolledWorkflow('fixture.yml', nestedShared).join('\n'),
    /job-level concurrency group is not bound to github.run_id/,
  );
});

test('quickstart is an explicit negative control with unique non-PR groups', () => {
  assert.ok(!COALESCED_WORKFLOWS.includes('.github/workflows/quickstart.yml'));
  const valid = `name: quickstart
concurrency:
  group: ${QUICKSTART_GROUP_EXPRESSION}
  cancel-in-progress: ${QUICKSTART_CANCEL_EXPRESSION}
jobs: {}
`;
  assert.deepEqual(validateQuickstartWorkflow(valid), []);

  const latestOnly = valid.replace(QUICKSTART_GROUP_EXPRESSION, GROUP_EXPRESSION);
  const problems = validateQuickstartWorkflow(latestOnly).join('\n');
  assert.match(problems, /every non-PR run needs a unique run-id group/);
  assert.match(problems, /must never enter a latest-main cancellation group/);
});

test('exhaustive E2E has no workflow cancellation and no ref-shared job groups', () => {
  const valid = `name: e2e
jobs:
  qualification:
    concurrency:
      group: qualification-\${{ github.run_id }}
      cancel-in-progress: false
`;
  assert.deepEqual(validateE2EWorkflow(valid), []);

  const refShared = valid.replace('github.run_id', 'github.ref');
  assert.match(
    validateE2EWorkflow(refShared).join('\n'),
    /job-level concurrency group is not bound to github.run_id/,
  );

  const workflowCancellation = `name: e2e
concurrency:
  group: e2e-latest
  cancel-in-progress: true
jobs: {}
`;
  assert.match(
    validateE2EWorkflow(workflowCancellation).join('\n'),
    /must not have workflow-level cancellation/,
  );
});

test('the checked-in registry and every enrolled workflow match the policy', () => {
  assert.deepEqual(auditMainCoalescing(process.cwd()), []);
});
