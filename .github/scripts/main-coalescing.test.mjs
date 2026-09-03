import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import {
  CANCEL_COALESCED_WORKFLOWS,
  CANCEL_EXPRESSION,
  COALESCED_WORKFLOWS,
  GROUP_EXPRESSION,
  LATEST_MAIN_SUFFIX,
  MAIN_REF,
  NEVER_COALESCED_WORKFLOWS,
  QUEUE_CANCEL_LITERAL,
  QUEUE_COALESCED_WORKFLOWS,
  QUICKSTART_CANCEL_EXPRESSION,
  QUICKSTART_GROUP_EXPRESSION,
  auditMainCoalescing,
  coalescingDecision,
  concurrencyGroupValues,
  parseTopLevelConcurrency,
  tierPartitionProblems,
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

test('a queue-coalesced workflow keeps the group and refuses the cancellation expression', () => {
  // tsouza/cerberus#2991: the two halves of "coalesced" are separable. The
  // shared GROUP still replaces a superseded run while it is pending, which
  // costs nothing; CANCELLATION kills work already in progress, which starves
  // any lane whose run outlasts the median push gap.
  const queued = `name: fixture
concurrency:
  group: ${GROUP_EXPRESSION}
  cancel-in-progress: ${QUEUE_CANCEL_LITERAL}
jobs: {}
`;
  assert.deepEqual(validateEnrolledWorkflow('fixture.yml', queued, { queueOnly: true }), []);

  // The SAME text is a failure for an ordinarily-enrolled workflow: opting out
  // of cancellation has to be a declared enrollment, never something a
  // workflow can do to itself by editing one line.
  assert.match(
    validateEnrolledWorkflow('fixture.yml', queued, { queueOnly: false }).join('\n'),
    /cancel-in-progress/,
  );

  // And the converse: a queue-coalesced member that regains the cancellation
  // expression is a failure, which is what stops #2991 silently returning.
  const cancelling = queued.replace(QUEUE_CANCEL_LITERAL, CANCEL_EXPRESSION);
  assert.match(
    validateEnrolledWorkflow('fixture.yml', cancelling, { queueOnly: true }).join('\n'),
    /queue-coalesced/,
  );

  // A shared group is still required — queue-coalescing relaxes cancellation
  // only, never the grouping that makes a burst collapse to one run.
  assert.match(
    validateEnrolledWorkflow('fixture.yml', queued.replace(GROUP_EXPRESSION, 'deep-${{ github.ref }}'), {
      queueOnly: true,
    }).join('\n'),
    /concurrency group/,
  );

  // Every queue-coalesced workflow must still be enrolled: the narrower set is
  // a refinement of the enrollment, not an escape from it.
  for (const workflow of QUEUE_COALESCED_WORKFLOWS) {
    assert.ok(COALESCED_WORKFLOWS.includes(workflow), workflow);
  }
});

test('the executable model cancels nothing for a queue-coalesced workflow', () => {
  const queued = coalescingDecision({
    eventName: 'push',
    ref: MAIN_REF,
    workflow: 'deep',
    runId: '17',
    queueOnly: true,
  });
  assert.equal(queued.cancelInProgress, false);
  // The GROUP is unchanged — that is the whole point of the distinction.
  assert.equal(queued.group, decision('push', MAIN_REF).group);
  assert.equal(queued.reason, 'latest_main_push');
  // The ordinary model still cancels, so this assertion is not vacuous.
  assert.equal(decision('push', MAIN_REF).cancelInProgress, true);
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

// ---------------------------------------------------------------------------
// Non-vacuity of the tier policy (tsouza/cerberus#2994).
//
// The audit above passes on the checked-in tree. A green that a policy change
// cannot turn red is not evidence, so these drive the REAL workflow text
// through the validator with the one line under policy flipped, in BOTH
// directions, and pin the tier partition itself.

test('every queue-coalesced workflow reds if cancellation is restored to it', () => {
  assert.ok(QUEUE_COALESCED_WORKFLOWS.length > 0);
  for (const workflow of QUEUE_COALESCED_WORKFLOWS) {
    const body = readFileSync(path.join(process.cwd(), workflow), 'utf8');
    // The checked-in text is accepted, so the flip below is the only variable.
    assert.deepEqual(validateEnrolledWorkflow(workflow, body), [], workflow);
    assert.equal(
      parseTopLevelConcurrency(body).cancelInProgress,
      QUEUE_CANCEL_LITERAL,
      workflow,
    );

    // Anchored to a line carrying EXACTLY the two-space top-level indent:
    // migration-e2e.yml also holds a six-space JOB-level `cancel-in-progress:
    // false`, and a bare substring replace would be only accidentally safe
    // (it happens to sit later in the file). Rewriting the wrong line would
    // exercise the nested-concurrency check instead of the tier check.
    assert.equal(
      (body.match(/^ {2}cancel-in-progress: false$/gm) ?? []).length,
      1,
      `${workflow} must carry exactly one top-level cancel-in-progress line`,
    );
    const flip = (value) => {
      const rewritten = body.replace(
        /^ {2}cancel-in-progress: false$/m,
        `  cancel-in-progress: ${value}`,
      );
      assert.notEqual(rewritten, body, workflow);
      assert.equal(
        parseTopLevelConcurrency(rewritten).cancelInProgress,
        value,
        workflow,
      );
      return rewritten;
    };

    // Someone "optimises" the literal back into the cancel expression, or
    // reaches for the cruder bare `true`. Both are refused by the tier check.
    for (const value of [CANCEL_EXPRESSION, 'true']) {
      assert.match(
        validateEnrolledWorkflow(workflow, flip(value)).join('\n'),
        /queue-coalesced/,
        `${workflow} <- ${value}`,
      );
    }
  }
});

test('every still-cancelling workflow reds if it is quietly opted out', () => {
  assert.ok(CANCEL_COALESCED_WORKFLOWS.length > 0);
  for (const workflow of CANCEL_COALESCED_WORKFLOWS) {
    const body = readFileSync(path.join(process.cwd(), workflow), 'utf8');
    assert.deepEqual(validateEnrolledWorkflow(workflow, body), [], workflow);
    assert.equal(
      parseTopLevelConcurrency(body).cancelInProgress,
      CANCEL_EXPRESSION,
      workflow,
    );

    // The pin is two-sided: a workflow the policy deliberately leaves
    // cancelling cannot opt itself out by editing one line either. Moving it
    // has to be a declared tier change with its reason, the same as moving one
    // the other way.
    const optedOut = body.replace(
      /^ {2}cancel-in-progress: \$\{\{.*\}\}$/m,
      `  cancel-in-progress: ${QUEUE_CANCEL_LITERAL}`,
    );
    assert.notEqual(optedOut, body, workflow);
    assert.equal(
      parseTopLevelConcurrency(optedOut).cancelInProgress,
      QUEUE_CANCEL_LITERAL,
      workflow,
    );
    assert.match(
      validateEnrolledWorkflow(workflow, optedOut).join('\n'),
      /want the canonical main-push\/equivalent-schedule-only expression/,
      workflow,
    );
  }
});

test('the two cancellation tiers partition the enrollment exactly', () => {
  assert.deepEqual(tierPartitionProblems(), []);
  assert.equal(
    QUEUE_COALESCED_WORKFLOWS.length + CANCEL_COALESCED_WORKFLOWS.length,
    COALESCED_WORKFLOWS.length,
  );
  // The tiers are disjoint and neither is a superset of the enrollment.
  for (const workflow of QUEUE_COALESCED_WORKFLOWS) {
    assert.ok(!CANCEL_COALESCED_WORKFLOWS.includes(workflow), workflow);
    assert.ok(COALESCED_WORKFLOWS.includes(workflow), workflow);
  }
  for (const workflow of CANCEL_COALESCED_WORKFLOWS) {
    assert.ok(COALESCED_WORKFLOWS.includes(workflow), workflow);
  }
  // The starved pair the issue named is queue-coalesced, and the lanes the
  // measurement found cancellation still pays for stay cancelling. Both halves
  // are pinned: a sweep that moved everything would red here just as a
  // reversion would.
  assert.ok(QUEUE_COALESCED_WORKFLOWS.includes('.github/workflows/mutation.yml'));
  assert.ok(QUEUE_COALESCED_WORKFLOWS.includes('.github/workflows/chdb.yml'));
  for (const workflow of [
    '.github/workflows/agpl-oracle.yml',
    '.github/workflows/codeql.yml',
    '.github/workflows/perf-benchmark.yml',
    '.github/workflows/perf-nightly.yml',
  ]) {
    assert.ok(CANCEL_COALESCED_WORKFLOWS.includes(workflow), workflow);
    assert.ok(!QUEUE_COALESCED_WORKFLOWS.includes(workflow), workflow);
  }
});

test('a workflow in neither tier, or in both, is a policy failure', () => {
  // The SAME function the audit calls, driven over injected rosters so the
  // reachability of each branch is shown rather than restated.
  const enrolled = ['a.yml', 'b.yml'];
  assert.deepEqual(
    tierPartitionProblems({ enrolled, queued: ['a.yml'], cancelling: ['b.yml'] }),
    [],
  );
  assert.match(
    tierPartitionProblems({ enrolled, queued: ['a.yml'], cancelling: [] }).join('\n'),
    /b\.yml: latest-main enrolled but in neither cancellation tier/,
  );
  assert.match(
    tierPartitionProblems({
      enrolled,
      queued: ['a.yml', 'b.yml'],
      cancelling: ['b.yml'],
    }).join('\n'),
    /b\.yml: enrolled in both cancellation tiers/,
  );
  assert.match(
    tierPartitionProblems({
      enrolled,
      queued: ['a.yml', 'stray.yml'],
      cancelling: ['b.yml'],
    }).join('\n'),
    /stray\.yml: carries a cancellation tier but is absent/,
  );
});

test('the never-coalesced workflows are untouched by the tier policy', () => {
  // A negative control for the whole change: ci.yml must not acquire either
  // tier, and its concurrency must stay outside the latest-main groups.
  for (const workflow of NEVER_COALESCED_WORKFLOWS) {
    assert.ok(!QUEUE_COALESCED_WORKFLOWS.includes(workflow), workflow);
    assert.ok(!CANCEL_COALESCED_WORKFLOWS.includes(workflow), workflow);
  }
  const ci = readFileSync(
    path.join(process.cwd(), '.github/workflows/ci.yml'),
    'utf8',
  );
  assert.ok(
    !concurrencyGroupValues(ci).some((group) => group.includes(LATEST_MAIN_SUFFIX)),
  );
});
