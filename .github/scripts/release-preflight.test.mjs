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
  supportWindowProblem,
  unpublishedWorkProblem,
  MODE_MAINLINE,
  MODE_MAINTENANCE,
  REUSABLE_JOB_SEPARATOR,
  RETIRED_LINES,
  SUPPORTED_MINOR_LINES,
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

// ---------------------------------------------------------------------------
// early retirement — RETIRED_LINES, and the guard on deleting a branch
// ---------------------------------------------------------------------------

// Tags spanning the three lines the window keeps, so anything the declaration
// rejects is rejected DESPITE the arithmetic saying it is supported.
const IN_WINDOW_TAGS = ['v1.13.2', 'v1.12.3', 'v1.11.4', 'v1.10.7'];

test('every declared retirement is a branch the window math can see', () => {
  // `supportWindowProblem` returns null for anything that is not a maintenance
  // branch, and it does so BEFORE consulting the declaration. A typo in a
  // RETIRED_LINES key — `releases/1.11.x`, `release/1.11`, a stray space — is
  // therefore not a loud failure but a silent one: the entry sits in the map
  // looking authoritative while the line it names stays publishable. Asserting
  // the declaration actually fires is the only thing standing between a typo
  // and a retired line quietly accepting a release.
  assert.ok(RETIRED_LINES.size > 0, 'the map is expected to hold the lines retired so far');
  for (const [branch, reason] of RETIRED_LINES) {
    const problem = supportWindowProblem({ branch, tags: IN_WINDOW_TAGS });
    assert.ok(problem, `${branch} is declared retired but the gate does not block it`);
    assert.match(problem, /end-of-life/);
    assert.ok(reason && reason.length > 0, `${branch} is declared retired with no reason`);
  }
});

test('release/1.11.x is retired even though the window still counts it as supported', () => {
  // The case the declaration exists for. v1.13 is current and the window keeps
  // the latest 3 minors, so 1.11 is one inside the boundary by arithmetic; it
  // is retired anyway, and the block has to name the reason rather than the
  // version distance, because the distance is not why.
  assert.equal(SUPPORTED_MINOR_LINES, 3, 'the fixture below assumes a latest-3 window');
  assert.equal(supportWindowProblem({ branch: 'release/1.12.x', tags: IN_WINDOW_TAGS }), null);

  const problem = supportWindowProblem({ branch: 'release/1.11.x', tags: IN_WINDOW_TAGS });
  assert.match(problem, /end-of-life/);
  assert.match(problem, /retired ahead of the latest-3 support window/);
  assert.doesNotMatch(problem, /minor\(s\) behind/, 'the arithmetic reason would be the wrong one here');
});

test('a declared retirement does not depend on the tag set being resolvable', () => {
  // The declaration is the whole basis for the block, so it must survive a
  // world where the window cannot be computed at all. Otherwise re-creating the
  // deleted branch and publishing from it would be refused only while the tag
  // listing happened to work.
  assert.match(supportWindowProblem({ branch: 'release/1.11.x', tags: [] }), /end-of-life/);
  assert.equal(supportWindowProblem({ branch: 'release/1.12.x', tags: [] }), null, 'negative control');
});

test('a branch whose tip is a published tag on its own line is safe to delete', () => {
  const tags = [
    { name: 'v1.11.4', sha: 'aaa' },
    { name: 'v1.11.3', sha: 'bbb' },
    { name: 'v1.12.3', sha: 'ccc' },
  ];
  assert.equal(unpublishedWorkProblem({ branch: 'release/1.11.x', tipSha: 'aaa', tags }), null);
});

test('a branch carrying commits no release published is refused, and the highest tag is named', () => {
  // The state that makes deletion lossy: a backport merged but never released.
  // Reporting the highest tag is what turns the refusal into an action — it is
  // the commit the operator has to diff against to see what would be lost.
  const tags = [
    { name: 'v1.11.4', sha: 'aaa' },
    { name: 'v1.11.10', sha: 'ddd' },
  ];
  const problem = unpublishedWorkProblem({ branch: 'release/1.11.x', tipSha: 'unreleased', tags });
  assert.match(problem, /not the commit of any published v1\.11\.\* tag/);
  assert.match(problem, /v1\.11\.10 at ddd/, 'highest is by patch NUMBER, not string order');
});

test('a tip matching some other line\'s tag does not count as published', () => {
  // A coincidence between a branch tip and an unrelated line's tag says nothing
  // about this line's history, and usually means the branch and its name have
  // diverged — the point at which a destructive step should stop.
  const tags = [
    { name: 'v1.12.3', sha: 'ccc' },
    { name: 'v1.11.4', sha: 'aaa' },
  ];
  assert.match(
    unpublishedWorkProblem({ branch: 'release/1.11.x', tipSha: 'ccc', tags }),
    /not the commit of any published v1\.11\.\* tag/,
  );
});

test('a line with no stable tag at all is refused rather than assumed empty', () => {
  // No tag means no reconstruction: the branch IS the only record. A prerelease
  // does not count, matching the rest of the gate's reading of what "published"
  // means.
  const tags = [
    { name: 'v1.11.0-rc.1', sha: 'aaa' },
    { name: 'v1.12.3', sha: 'ccc' },
  ];
  const problem = unpublishedWorkProblem({ branch: 'release/1.11.x', tipSha: 'aaa', tags });
  assert.match(problem, /no published stable tag/);
});

test('an unresolvable tip and a non-maintenance branch are both refusals, not passes', () => {
  // Both are the shape where "nothing to check" reads identically to "checked
  // and fine". This driver deletes branches, so neither may return null.
  const tags = [{ name: 'v1.11.4', sha: 'aaa' }];
  assert.match(unpublishedWorkProblem({ branch: 'release/1.11.x', tipSha: '', tags }), /could not resolve/);
  assert.match(unpublishedWorkProblem({ branch: 'main', tipSha: 'aaa', tags }), /not a release maintenance line/);
});
