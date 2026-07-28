// release-preflight.test.mjs — node:test guard for the RELEASE GATE's expected
// set, run on the required `lint` lane.
//
// Why it exists separately from `release-preflight.mjs --self-test`: that
// self-test only runs inside release.yml, which has NO `pull_request:` trigger.
// Every change to the gate would therefore be unverified until a release is
// actually cut — the one moment you least want to discover it. This suite runs
// on every PR (`lint` has no `docs_only` condition, unlike `check`).
//
// What it pins, and why each pin is written the way it is:
//
//   The gate used to be observation-derived: `evaluate()` iterated the
//   check-runs the API returned, so a lane that never ran contributed zero
//   entries and therefore zero problems. "migration-e2e now gates the release"
//   was satisfiable by changing nothing. The `required` set is the fix, so the
//   headline test is a NEGATIVE control: the same absent-lane world must be
//   GREEN once the name leaves `required`. A detector rewritten to fire
//   unconditionally passes an absence test but fails that one.
//
//   Every case also asserts the OTHER detectors stay silent, so a rewrite that
//   turns one problem into three does not slip through on a `.some()` match.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  evaluate,
  parseCheckList,
  requiredChecksPending,
  allSuitesSettled,
  MODE_MAINLINE,
  MODE_MAINTENANCE,
  REUSABLE_JOB_SEPARATOR,
} from './release-preflight.mjs';

test('parseCheckList keeps commas inside a check name', () => {
  // `property (PromQL + LogQL + TraceQL, rapid N=500)` is a branch-protection
  // required context. Under a comma separator it split into two names no lane
  // will ever post, so the preflight waited out its full window and aborted
  // the release — a wiring bug that only ever surfaces mid-publish.
  const names = parseCheckList(
    'check\nproperty (PromQL + LogQL + TraceQL, rapid N=500)\n  migration-e2e  \n\n',
  );
  assert.deepEqual(names, [
    'check',
    'property (PromQL + LogQL + TraceQL, rapid N=500)',
    'migration-e2e',
  ]);
  assert.deepEqual(parseCheckList(undefined), []);
});

// The release run's own jobs, as release.yml wires them into RELEASE_SELF_JOBS.
const SELF_JOBS = new Set([
  'gate',
  'preflight',
  'goreleaser',
  'release-artifact-migration',
  'publish',
  'brew-smoke',
  'chart-release',
]);

// A representative slice of the required set release.yml supplies. It MUST
// contain `migration-e2e` — that is the lane the whole deliverable is about, and
// the negative control below asserts on its removal, so a required set that
// silently lost it would make this file's headline test vacuous.
const REQUIRED = ['check', 'lint', 'compose-smoke', 'migration-e2e'];

const SHA = 'abcdef1234567890';

function run(name, { status = 'completed', conclusion = 'success', id = 1 } = {}) {
  return { name, status, conclusion, id };
}

// A world in which exactly `names` posted a green check-run on the pushed
// commit, which is also the branch tip.
function world({ names = REQUIRED, statuses = [], ...rest } = {}) {
  return {
    branchHead: SHA,
    pushedSha: SHA,
    selfJobs: SELF_JOBS,
    branchLabel: 'release/1.4.x',
    checkRuns: names.map((n) => run(n)),
    statuses,
    required: REQUIRED,
    mode: MODE_MAINTENANCE,
    ...rest,
  };
}

test('positive control: every required lane ran green -> no problems', () => {
  const r = evaluate(world());
  assert.deepEqual(r.problems, []);
  assert.equal(r.gated, REQUIRED.length);
});

test('an absent required lane blocks, and ONLY on its absence', () => {
  // Precondition: the negative control below is only meaningful if the name is
  // actually in the required set.
  assert.ok(REQUIRED.includes('migration-e2e'), 'REQUIRED must contain migration-e2e for this test to mean anything');

  const withoutLane = REQUIRED.filter((n) => n !== 'migration-e2e');

  // (a) The lane never posted a check-run: everything else is green, and the
  // release is BLOCKED anyway.
  const blocked = evaluate(world({ names: withoutLane }));
  assert.equal(blocked.problems.length, 1, `expected exactly one problem, got: ${blocked.problems.join('; ')}`);
  assert.match(blocked.problems[0], /^migration-e2e: REQUIRED lane posted no check-run/);

  // (a, negative control) The SAME world with the name out of `required` is
  // green. This is what fails if the detector is rewritten to fire
  // unconditionally — the other way to fake an absence test green.
  const green = evaluate(world({ names: withoutLane, required: withoutLane }));
  assert.deepEqual(green.problems, []);
});

test('a required lane that ran RED is a different problem than one that never ran', () => {
  const names = REQUIRED.filter((n) => n !== 'migration-e2e');
  const r = evaluate(
    world({
      names,
      checkRuns: [...names.map((n) => run(n)), run('migration-e2e', { conclusion: 'failure' })],
    }),
  );
  assert.equal(r.problems.length, 1, `expected exactly one problem, got: ${r.problems.join('; ')}`);
  assert.equal(r.problems[0], 'migration-e2e: failure');
  assert.ok(
    !r.problems.some((p) => /posted no check-run/.test(p)),
    'a lane that ran and failed must not be reported as absent',
  );
});

test('an empty required set is itself a blocking problem', () => {
  const r = evaluate(world({ required: [] }));
  assert.equal(r.problems.length, 1, `expected exactly one problem, got: ${r.problems.join('; ')}`);
  assert.match(r.problems[0], /RELEASE_REQUIRED_CHECKS is empty/);
  // The degradation this guards: without it, the same call is silently green.
  assert.ok(/deny-list/.test(r.problems[0]), 'the message must name the failure mode it prevents');
});

test('an informational PREFIX that swallows a required lane is a wiring error, not an absence', () => {
  // `migration` as an informational prefix would de-gate the whole migration
  // family, including the required aggregate.
  const r = evaluate(world({ informational: ['migration'] }));
  assert.equal(r.problems.length, 1, `expected exactly one problem, got: ${r.problems.join('; ')}`);
  assert.match(r.problems[0], /^migration-e2e is both REQUIRED and de-gated by RELEASE_INFORMATIONAL_CHECKS/);
  assert.ok(
    !r.problems.some((p) => /posted no check-run/.test(p)),
    'the swallowed lane must be reported as mis-wired, not as merely absent',
  );

  // An informational prefix that does NOT collide stays a legitimate de-gate.
  const ok = evaluate(world({ informational: ['compose-smoke-shard-info', 'dashboard'] }));
  assert.deepEqual(ok.problems, []);
});

test('a required lane that is also a self-job is a wiring error', () => {
  const r = evaluate(world({ required: [...REQUIRED, 'goreleaser'] }));
  assert.equal(r.problems.length, 1, `expected exactly one problem, got: ${r.problems.join('; ')}`);
  assert.match(r.problems[0], /^goreleaser is both REQUIRED and excluded by RELEASE_SELF_JOBS/);
});

test('requiredChecksPending splits missing from running', () => {
  const checkRuns = [run('check'), run('lint'), run('compose-smoke', { status: 'in_progress', conclusion: null })];
  const { missing, running } = requiredChecksPending(checkRuns, REQUIRED);
  assert.deepEqual(missing, ['migration-e2e']);
  assert.deepEqual(running, ['compose-smoke']);

  // All present + completed -> nothing pending, so the wait loop proceeds.
  const settled = requiredChecksPending(
    REQUIRED.map((n) => run(n)),
    REQUIRED,
  );
  assert.deepEqual(settled, { missing: [], running: [] });

  // Latest-per-name: a green re-run supersedes an earlier in-flight run.
  const rerun = requiredChecksPending(
    [run('check', { status: 'in_progress', conclusion: null, id: 1 }), run('check', { id: 2 })],
    ['check'],
  );
  assert.deepEqual(rerun, { missing: [], running: [] });
});

test('a required lane that never runs is NOT hidden by suite-level settledness', () => {
  // The trap this closes: every suite that DID run is `completed`, so the
  // suite-only wait is done and the gate would evaluate against a silence.
  const suites = [{ id: 1, status: 'completed', app: { slug: 'ci' }, latest_check_runs_count: 3 }];
  assert.equal(allSuitesSettled(suites, null).done, true);
  const { missing } = requiredChecksPending([run('check'), run('lint'), run('compose-smoke')], REQUIRED);
  assert.deepEqual(missing, ['migration-e2e'], 'suite settledness says done; the required set says otherwise');
});

test('mode split: the branch-tip rule is maintenance-only, in both directions', () => {
  const moved = { branchHead: 'ffffffffffffffff' };

  const mainline = evaluate(world({ ...moved, branchLabel: 'main', mode: MODE_MAINLINE }));
  assert.deepEqual(mainline.problems, [], 'main legitimately moves under a release waiting on CI');

  const maintenance = evaluate(world({ ...moved, mode: MODE_MAINTENANCE }));
  assert.equal(maintenance.problems.length, 1, `expected exactly one problem, got: ${maintenance.problems.join('; ')}`);
  assert.match(maintenance.problems[0], /is NOT the tip of release\/1\.4\.x/);
});

test('mode split: the EOL support window is maintenance-only, in both directions', () => {
  const tags = ['v1.5.0', 'v1.4.0', 'v1.3.0', 'v1.2.0'];

  const mainline = evaluate(world({ branchLabel: 'main', tags, mode: MODE_MAINLINE }));
  assert.deepEqual(mainline.problems, []);

  const maintenance = evaluate(world({ branchLabel: 'release/1.2.x', tags, mode: MODE_MAINTENANCE }));
  assert.ok(
    maintenance.problems.some((p) => /end-of-life/.test(p)),
    `an EOL maintenance line must be refused, got: ${maintenance.problems.join('; ')}`,
  );
});

test("a self-job's reusable-workflow children are excluded structurally", () => {
  // release-artifact-migration `uses:` migration-e2e.yml, so its check-runs are
  // posted as "release-artifact-migration / migration-tier1". They start AFTER
  // this preflight, so gating on them deadlocks the release.
  assert.equal(REUSABLE_JOB_SEPARATOR, ' / ');
  const child = 'release-artifact-migration' + REUSABLE_JOB_SEPARATOR + 'migration-tier1';
  const r = evaluate(
    world({
      names: REQUIRED,
      checkRuns: [...REQUIRED.map((n) => run(n)), run(child, { status: 'in_progress', conclusion: null })],
    }),
  );
  assert.deepEqual(r.problems, []);
  assert.equal(r.gated, REQUIRED.length, 'the reusable child must not be counted as a gated check');
});

test('legacy statuses satisfy the required set too, and a red one still blocks', () => {
  const required = [...REQUIRED, 'GitGuardian'];
  const green = evaluate(world({ required, statuses: [{ context: 'GitGuardian', state: 'success' }] }));
  assert.deepEqual(green.problems, []);

  const red = evaluate(world({ required, statuses: [{ context: 'GitGuardian', state: 'failure' }] }));
  assert.equal(red.problems.length, 1, `expected exactly one problem, got: ${red.problems.join('; ')}`);
  assert.equal(red.problems[0], 'GitGuardian: status failure');
});
