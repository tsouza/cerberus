// main-coalescing.mjs — structural guard for replaceable deep-main work.
//
// GitHub evaluates workflow concurrency before any job can run, so the policy
// has two halves: the exact expressions enrolled workflows carry and this
// executable model that proves which event/ref pairs those expressions may
// cancel. Main pushes and equivalent scheduled runs are replaceable within
// separate mode groups, so a routine push can never erase deeper nightly
// evidence. Pull requests, merge groups, manual qualification,
// reusable-workflow calls, maintenance branches, tags, releases, and unknown
// inputs receive a unique run group and can never cancel one another.
//
// MODE=check (default) reads CI_LANE_REGISTRY (default .github/ci-lanes.json),
// checks every enrolled workflow, and rejects registry/workflow drift.
//
// Exit: 0 only when the complete enrollment and its explicit exclusions match
// the policy. Node builtins only.

import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

const SCRIPT = fileURLToPath(import.meta.url);

export const MAIN_REF = 'refs/heads/main';
export const LATEST_MAIN_SUFFIX = 'latest-main';

export const GROUP_EXPRESSION =
  "${{ github.workflow }}-${{ (github.event_name == 'push' && github.ref == 'refs/heads/main') && 'latest-main-push' || (github.event_name == 'schedule' && github.ref == 'refs/heads/main' && github.event.schedule != '') && format('latest-main-schedule-{0}', github.event.schedule) || github.run_id }}";
// The literal a QUEUE_COALESCED_WORKFLOWS member carries in place of
// CANCEL_EXPRESSION. A literal, not an expression: "never cancel" has no
// event/ref condition to evaluate, and spelling it as an always-false
// expression would leave a shape a later edit could quietly make true again.
export const QUEUE_CANCEL_LITERAL = 'false';

export const CANCEL_EXPRESSION =
  "${{ (github.event_name == 'push' && github.ref == 'refs/heads/main') || (github.event_name == 'schedule' && github.ref == 'refs/heads/main' && github.event.schedule != '') }}";

// These workflows contain replaceable deep-main, soak, or scheduled test work.
// Main pushes share one group per workflow. Scheduled runs share a group only
// with the same workflow and exact cron expression.
export const COALESCED_WORKFLOWS = Object.freeze([
  '.github/workflows/agpl-oracle.yml',
  '.github/workflows/chdb.yml',
  '.github/workflows/codeql.yml',
  '.github/workflows/compatibility.yml',
  '.github/workflows/coverage.yml',
  '.github/workflows/migration-e2e.yml',
  '.github/workflows/mutation.yml',
  '.github/workflows/perf-benchmark.yml',
  '.github/workflows/perf-nightly.yml',
  '.github/workflows/perf-profile.yml',
  '.github/workflows/property.yml',
  '.github/workflows/schema-integration.yml',
  '.github/workflows/strict-scan.yml',
]);

// QUEUE-COALESCED enrollment: a shared latest-main group, but NO cancellation
// of a run already in progress (tsouza/cerberus#2991).
//
// The two halves of "coalesced" are separable, and conflating them is what
// broke coverage.yml. The GROUP is what makes deep-main work replaceable: a
// burst of pushes collapses to one PENDING run, and a third push replaces that
// pending one — the saving is real and it costs nothing, because a pending run
// holds no runner. CANCELLATION is a different act: it kills work that is
// already executing, and it only pays when the run is short enough that the
// killed work was nearly worthless.
//
// For a lane whose run is LONGER than the median push gap, cancellation does
// not coalesce anything — it starves the lane outright. coverage.yml runs ~52
// minutes against a median 38-minute gap on main, and cancellation left 71 of
// its last 120 push runs killed (median 27 minutes in, past the point where the
// expensive lane had done most of its work) against 6 successes. The lane was
// not slow; it was never allowed to finish, so `main` went unmeasured for days
// while every pull request saw a green `coverage`.
//
// A workflow listed here therefore keeps the canonical GROUP expression and
// carries a literal `cancel-in-progress: false`. Its registry `main_posture`
// stays `coalesced`, which remains the truth: a superseded run is still
// replaced, just at queue time rather than mid-flight.
//
// # The decision rule (tsouza/cerberus#2994)
//
// Cancellation is not uniformly wrong, so enrollment is not uniform. Every
// enrolled workflow is classified by ONE ordered question, and the answer is
// recorded next to the name:
//
//   1. Does a superseded run still carry information its successor will not
//      reproduce? A lane whose output is a deterministic function of the tree
//      answers NO — the newer tree's verdict is a strict replacement, and the
//      older run's result is genuinely redundant. A lane that emits a
//      per-commit measurement, publishes a score, or explores randomly-seeded
//      input answers YES.
//   2. If YES, is the measured loss material? Cancellation that lands in the
//      first quarter of a run and throws away single-digit runner minutes per
//      day is not worth trading latency-to-HEAD for.
//
// Only a workflow answering YES to both is queue-coalesced. The rest stay in
// CANCEL_COALESCED_WORKFLOWS below, and the audit requires the two tiers to
// partition COALESCED_WORKFLOWS exactly, so a fourteenth enrollment cannot
// land without an explicit tier and a reason.
//
// Figures below are the last ~99 push-on-main runs per workflow over
// 2026-08-31T06:35Z - 2026-09-03T09:05Z (74.5 h), against a median main-push
// gap of 38.5 min. A run duration is the median of that workflow's SUCCESSFUL
// runs, so a lane's fast failures cannot flatter it; a kill point is the
// median cancelled-run lifetime, expressed against that same duration.
export const QUEUE_COALESCED_WORKFLOWS = Object.freeze([
  // 48% cancelled, 35% of runner minutes (991 min) discarded, 12 of 48 kills
  // past 90% of a full run. A 34.5-min run against a 38.5-min gap is on the
  // knife edge. It also carried 30 real failures: cancelling this lane hides
  // red on the trunk as well as wasting the runner that found it.
  '.github/workflows/chdb.yml',
  // 29% cancelled, 18% of minutes (297 min). Publishes a per-head parity SCORE
  // to the badges branch and runs the parity ratchet, so a killed run is a
  // missing datum in the public compatibility record, not a redundant verdict.
  '.github/workflows/compatibility.yml',
  '.github/workflows/coverage.yml',
  // 36% cancelled, 25% of minutes (517 min), 8 of 36 kills past 90%. Uploads
  // per-tier run reports that are the evidence half of its coverage ratchet.
  '.github/workflows/migration-e2e.yml',
  // The worst of the set and the closest sibling of the coverage failure: 60%
  // cancelled, 45% of minutes (1413 min) discarded, 19 of 59 kills past 90% of
  // a full run, and only 10 successes in 74.5 h. Its 38.6-min run is level with
  // the 38.5-min median push gap — only 50% of gaps are long enough for a run
  // to finish — so cancellation coalesced nothing; it starved the lane. Its
  // output is a per-commit mutation score held to an efficacy threshold, which
  // its successor does not reproduce for the killed commit.
  '.github/workflows/mutation.yml',
  // 40% cancelled, 31% of minutes (677 min), 13 of 40 kills past 90%. Merges
  // shard profiles into one per-commit profile artifact — a measurement.
  '.github/workflows/perf-profile.yml',
  // Enrolled on question 1, not on volume: 19% cancelled and 9% of minutes
  // (93 min), but `property` runs rapid with an unpinned seed, so every run
  // explores a DIFFERENT region of the input space. A cancelled run's draws
  // are never re-drawn by its successor, which makes the loss irreplaceable
  // rather than merely repeated.
  '.github/workflows/property.yml',
  // Enrolled on kill efficiency: only 21% cancelled, but the median kill lands
  // at 86% of a completed run — the latest in the whole set — and 8 of 21 kills
  // land past 90%. Cancelling this lane almost never saves work; it discards
  // an all-but-finished real-ClickHouse DDL differential.
  '.github/workflows/schema-integration.yml',
]);

// The reasoned complement of QUEUE_COALESCED_WORKFLOWS: enrolled workflows for
// which mid-flight cancellation remains CORRECT. This is not an exemption list
// — each entry answers the decision rule above with a NO, and the audit's
// partition check makes the membership load-bearing in both directions: a
// workflow here that carries `cancel-in-progress: false` is as much a failure
// as a queue-coalesced one that regains the cancel expression.
export const CANCEL_COALESCED_WORKFLOWS = Object.freeze([
  // Question 1 is NO: a deterministic licence-boundary differential over the
  // tree, so the newer tree's verdict strictly replaces the older one.
  // Question 2 is NO as well: 12% cancelled, 8% of minutes (47 min over
  // 74.5 h), and the median kill lands 23% into a 4.7-min run.
  '.github/workflows/agpl-oracle.yml',
  // Question 1 is NO, and decisively so: code scanning surfaces only the
  // LATEST analysis for a ref, so an older commit's SARIF is superseded by
  // construction rather than merely by convention. The median kill does land
  // late (64% of a 6.3-min run), but late kills of a redundant result are
  // still redundant — 105 min over 74.5 h, and 83 of 99 pushes analysed.
  '.github/workflows/codeql.yml',
  // No `push:` trigger at all, so the latest-main-push branch of the cancel
  // expression is unreachable and no push-on-main run exists to cancel. See
  // MAIN_COALESCED_OWNER_WORKFLOWS below for the registry half of this.
  '.github/workflows/perf-benchmark.yml',
  // Question 2 is NO: 11% cancelled, 5% of minutes (23 min over 74.5 h), one
  // kill past 90% in eleven, and the median kill lands 22% into a 5.3-min run.
  // Question 1 is weak too — it re-measures the same sentinel corpus against
  // the same committed baseline every run, so a killed run's datum is
  // substantially re-derived by its successor on a near-identical tree.
  '.github/workflows/perf-nightly.yml',
  // Question 1 is NO: 24 deterministic real-ClickHouse differentials over the
  // tree, each a verdict the newer tree's run re-derives in full. 22%
  // cancelled and 13% of minutes (144 min) is a larger loss than `property`'s,
  // and it is still the right trade — the loss is repeated work, not lost
  // evidence, which is exactly the distinction question 1 draws.
  '.github/workflows/strict-scan.yml',
]);

// perf-benchmark has no push-to-main trigger. Its weekly schedule is still
// replaceable, but its registry posture for main correctly remains `never`.
export const MAIN_COALESCED_OWNER_WORKFLOWS = Object.freeze(
  COALESCED_WORKFLOWS.filter(
    (workflow) => workflow !== '.github/workflows/perf-benchmark.yml',
  ),
);

// Load-bearing exclusions. These are named here so a future broad enrollment
// cannot quietly turn work that is distinct by definition into latest-only
// work. The registry and the workflow-specific regressions own the full reason;
// this guard owns the cancellation boundary.
export const NEVER_COALESCED_WORKFLOWS = Object.freeze([
  '.github/workflows/ci.yml',
  '.github/workflows/e2e.yml',
  '.github/workflows/mirror-images.yml',
  '.github/workflows/post-merge-drift.yml',
  '.github/workflows/quickstart.yml',
  '.github/workflows/release-gate-drift.yml',
  '.github/workflows/release.yml',
]);

// Per-LANE exclusion, finer than NEVER_COALESCED_WORKFLOWS: perf-nightly.yml
// is genuinely mixed — its `perf-nightly` job needs coalescing (a real,
// expensive ClickHouse run that a rapid main push should replace, not
// queue behind), but `perf-nightly-health-notify` runs only on
// `schedule` (`if: github.event_name == 'schedule'`) and never on the
// push-to-main trigger the workflow otherwise coalesces for, so its own
// lane posture correctly stays `never` even though its owning workflow is
// coalesced-enrolled. A whole-workflow exclusion (the perf-benchmark
// pattern below) does not fit here because the OTHER lane in the same
// workflow does need coalescing.
export const NEVER_COALESCED_LANES = Object.freeze(new Set(['perf-nightly.perf-nightly-health-notify']));

export const QUICKSTART_GROUP_EXPRESSION =
  "quickstart-${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}";
export const QUICKSTART_CANCEL_EXPRESSION =
  "${{ github.event_name == 'pull_request' }}";

export function coalescingDecision({
  eventName,
  ref,
  schedule,
  workflow = 'workflow',
  runId = 'run',
  // A queue-coalesced workflow (QUEUE_COALESCED_WORKFLOWS) shares the same
  // groups but cancels nothing: a superseded run is replaced only while it is
  // still pending. The group half of the model is therefore unchanged and only
  // the cancellation half is suppressed, which is exactly the difference the
  // enrolled expressions carry.
  queueOnly = false,
} = {}) {
  const event = String(eventName ?? '');
  const boundRef = String(ref ?? '');
  const cron = String(schedule ?? '').trim();
  const mainPush = event === 'push' && boundRef === MAIN_REF;
  const equivalentSchedule =
    event === 'schedule' && boundRef === MAIN_REF && cron !== '';
  const groupKey = mainPush
    ? `${LATEST_MAIN_SUFFIX}-push`
    : equivalentSchedule
      ? `${LATEST_MAIN_SUFFIX}-schedule-${cron}`
      : runId;
  return Object.freeze({
    cancelInProgress: !queueOnly && (mainPush || equivalentSchedule),
    group: `${workflow}-${groupKey}`,
    reason: mainPush
      ? 'latest_main_push'
      : equivalentSchedule
        ? 'equivalent_schedule'
        : 'distinct_run',
  });
}

function leadingSpaces(line) {
  return line.length - line.trimStart().length;
}

/** Read group values only from concurrency mappings at any YAML depth. */
export function concurrencyGroupValues(workflowText) {
  const lines = String(workflowText ?? '').split(/\r?\n/);
  const groups = [];

  for (let index = 0; index < lines.length; index += 1) {
    const concurrency = /^(\s*)concurrency:\s*(?:#.*)?$/.exec(lines[index]);
    if (!concurrency) continue;
    const mappingIndent = concurrency[1].length;

    for (let child = index + 1; child < lines.length; child += 1) {
      const line = lines[child];
      const trimmed = line.trim();
      if (trimmed === '' || trimmed.startsWith('#')) continue;
      const indent = leadingSpaces(line);
      if (indent <= mappingIndent) break;
      if (indent !== mappingIndent + 2) continue;

      const group = /^\s*group:\s*(.*?)\s*$/.exec(line);
      if (!group) continue;
      let value = group[1];
      if (/^[>|][+-]?$/.test(value)) {
        const continuation = [];
        for (let scalar = child + 1; scalar < lines.length; scalar += 1) {
          const scalarLine = lines[scalar];
          if (scalarLine.trim() === '') continue;
          if (leadingSpaces(scalarLine) <= indent) break;
          continuation.push(scalarLine.trim());
        }
        value = continuation.join(' ');
      }
      groups.push(value);
    }
  }

  return groups;
}

/** Read exactly one top-level concurrency mapping without a YAML dependency. */
export function parseTopLevelConcurrency(workflowText) {
  const lines = String(workflowText ?? '').split(/\r?\n/);
  const starts = lines
    .map((line, index) => [line, index])
    .filter(([line]) => line === 'concurrency:')
    .map(([, index]) => index);
  if (starts.length !== 1) {
    throw new Error(
      `expected exactly one top-level concurrency mapping, found ${starts.length}`,
    );
  }

  const values = new Map();
  for (const line of lines.slice(starts[0] + 1)) {
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue;
    if (leadingSpaces(line) === 0) break;
    if (leadingSpaces(line) !== 2) continue;
    const match = /^  ([a-z-]+):\s*(.*?)\s*$/.exec(line);
    if (!match) continue;
    if (values.has(match[1])) {
      throw new Error(`duplicate concurrency key ${match[1]}`);
    }
    values.set(match[1], match[2]);
  }

  return Object.freeze({
    group: values.get('group') ?? '',
    cancelInProgress: values.get('cancel-in-progress') ?? '',
  });
}

export function validateEnrolledWorkflow(workflow, workflowText, options = {}) {
  const queueOnly = options.queueOnly ?? QUEUE_COALESCED_WORKFLOWS.includes(workflow);
  const problems = [];
  let concurrency;
  try {
    concurrency = parseTopLevelConcurrency(workflowText);
  } catch (caught) {
    return [`${workflow}: ${caught.message}`];
  }
  if (concurrency.group !== GROUP_EXPRESSION) {
    problems.push(
      `${workflow}: concurrency group is ${JSON.stringify(concurrency.group)}, want the canonical main-push/equivalent-schedule/unique-run expression`,
    );
  }
  const wantCancel = queueOnly ? QUEUE_CANCEL_LITERAL : CANCEL_EXPRESSION;
  if (concurrency.cancelInProgress !== wantCancel) {
    problems.push(
      queueOnly
        ? `${workflow}: cancel-in-progress is ${JSON.stringify(concurrency.cancelInProgress)}, want the literal ${JSON.stringify(QUEUE_CANCEL_LITERAL)} — this workflow is queue-coalesced, so it shares the latest-main group but must never kill a run already in progress`
        : `${workflow}: cancel-in-progress is ${JSON.stringify(concurrency.cancelInProgress)}, want the canonical main-push/equivalent-schedule-only expression`,
    );
  }

  problems.push(
    ...validateRunBoundNestedConcurrency(workflow, workflowText),
  );
  return problems;
}

// A job-level group is evaluated after workflow-level admission. If it is
// shared by ref, a third qualification run can replace a pending job even when
// cancel-in-progress is false. Require every nested group to be run-bound and
// every nested cancellation flag to be false.
export function validateRunBoundNestedConcurrency(workflow, workflowText) {
  const problems = [];
  for (const line of String(workflowText).split(/\r?\n/)) {
    if (/^      group:/.test(line) && !line.includes('github.run_id')) {
      problems.push(
        `${workflow}: job-level concurrency group is not bound to github.run_id: ${line.trim()}`,
      );
    }
    if (
      /^      cancel-in-progress:/.test(line) &&
      line.trim() !== 'cancel-in-progress: false'
    ) {
      problems.push(
        `${workflow}: job-level cancel-in-progress must be literal false: ${line.trim()}`,
      );
    }
  }
  return problems;
}

export function validateE2EWorkflow(workflowText) {
  const workflow = '.github/workflows/e2e.yml';
  const problems = [];
  if (
    String(workflowText)
      .split(/\r?\n/)
      .some((line) => line === 'concurrency:')
  ) {
    problems.push(
      `${workflow}: exhaustive qualification must not have workflow-level cancellation`,
    );
  }
  problems.push(
    ...validateRunBoundNestedConcurrency(workflow, workflowText),
  );
  return problems;
}

export function validateQuickstartWorkflow(workflowText) {
  let concurrency;
  try {
    concurrency = parseTopLevelConcurrency(workflowText);
  } catch (caught) {
    return [`.github/workflows/quickstart.yml: ${caught.message}`];
  }
  const problems = [];
  if (concurrency.group !== QUICKSTART_GROUP_EXPRESSION) {
    problems.push(
      '.github/workflows/quickstart.yml: every non-PR run needs a unique run-id group so every main push completes',
    );
  }
  if (concurrency.cancelInProgress !== QUICKSTART_CANCEL_EXPRESSION) {
    problems.push(
      '.github/workflows/quickstart.yml: only a superseded pull-request head may be cancelled',
    );
  }
  if (
    concurrencyGroupValues(workflowText).some((group) =>
      group.includes(LATEST_MAIN_SUFFIX),
    )
  ) {
    problems.push(
      '.github/workflows/quickstart.yml: the published quickstart must never enter a latest-main cancellation group',
    );
  }
  return problems;
}

function readJSON(file) {
  return JSON.parse(readFileSync(file, 'utf8'));
}

function sameMembers(left, right) {
  const a = [...new Set(left)].sort();
  const b = [...new Set(right)].sort();
  return a.length === b.length && a.every((item, index) => item === b[index]);
}

// Every enrolled workflow must sit in exactly one cancellation tier. Without
// this, a fourteenth enrollment would silently inherit mid-flight cancellation
// as a default, which is the shape tsouza/cerberus#2991 and #2994 both took.
export function tierPartitionProblems({
  enrolled: enrolledWorkflows = COALESCED_WORKFLOWS,
  queued: queuedWorkflows = QUEUE_COALESCED_WORKFLOWS,
  cancelling: cancellingWorkflows = CANCEL_COALESCED_WORKFLOWS,
} = {}) {
  const problems = [];
  const queued = new Set(queuedWorkflows);
  const cancelling = new Set(cancellingWorkflows);
  for (const workflow of enrolledWorkflows) {
    const inQueued = queued.has(workflow);
    const inCancelling = cancelling.has(workflow);
    if (inQueued && inCancelling) {
      problems.push(
        `${workflow}: enrolled in both cancellation tiers; a workflow either keeps mid-flight cancellation or refuses it`,
      );
    } else if (!inQueued && !inCancelling) {
      problems.push(
        `${workflow}: latest-main enrolled but in neither cancellation tier — add it to QUEUE_COALESCED_WORKFLOWS or CANCEL_COALESCED_WORKFLOWS with the reason its superseded runs do or do not carry information`,
      );
    }
  }
  for (const workflow of [...queued, ...cancelling]) {
    if (!enrolledWorkflows.includes(workflow)) {
      problems.push(
        `${workflow}: carries a cancellation tier but is absent from the canonical enrollment`,
      );
    }
  }
  return problems;
}

export function auditMainCoalescing(root = process.cwd()) {
  const problems = [...tierPartitionProblems()];
  const enrolled = new Set(COALESCED_WORKFLOWS);

  for (const workflow of COALESCED_WORKFLOWS) {
    let body;
    try {
      body = readFileSync(path.join(root, workflow), 'utf8');
    } catch (caught) {
      problems.push(`${workflow}: cannot read enrolled workflow: ${caught.message}`);
      continue;
    }
    problems.push(...validateEnrolledWorkflow(workflow, body));
  }

  for (const workflow of NEVER_COALESCED_WORKFLOWS) {
    if (enrolled.has(workflow)) {
      problems.push(`${workflow}: explicit non-coalescing workflow is enrolled`);
    }
  }

  const workflowDir = path.join(root, '.github/workflows');
  for (const name of readdirSync(workflowDir).filter((item) => /\.ya?ml$/.test(item))) {
    const workflow = `.github/workflows/${name}`;
    const body = readFileSync(path.join(workflowDir, name), 'utf8');
    if (
      !enrolled.has(workflow) &&
      concurrencyGroupValues(body).some((group) =>
        group.includes(LATEST_MAIN_SUFFIX),
      )
    ) {
      problems.push(
        `${workflow}: contains ${LATEST_MAIN_SUFFIX} but is absent from the canonical enrollment`,
      );
    }
  }

  problems.push(
    ...validateQuickstartWorkflow(
      readFileSync(path.join(root, '.github/workflows/quickstart.yml'), 'utf8'),
    ),
  );
  problems.push(
    ...validateE2EWorkflow(
      readFileSync(path.join(root, '.github/workflows/e2e.yml'), 'utf8'),
    ),
  );

  const registryPath =
    process.env.CI_LANE_REGISTRY || path.join(root, '.github/ci-lanes.json');
  const registry = readJSON(registryPath);
  const coalescedOwners = registry.lanes
    .filter((lane) => lane.main_posture === 'coalesced')
    .map((lane) => lane.owner.workflow);
  if (!sameMembers(coalescedOwners, MAIN_COALESCED_OWNER_WORKFLOWS)) {
    problems.push(
      `registry main-coalesced owner workflows are ${JSON.stringify([...new Set(coalescedOwners)].sort())}, want ${JSON.stringify([...MAIN_COALESCED_OWNER_WORKFLOWS].sort())}`,
    );
  }

  for (const workflow of MAIN_COALESCED_OWNER_WORKFLOWS) {
    const lanes = registry.lanes.filter((lane) => lane.owner.workflow === workflow);
    if (lanes.length === 0) {
      problems.push(`${workflow}: enrolled workflow owns no registry lane`);
      continue;
    }
    for (const lane of lanes) {
      if (NEVER_COALESCED_LANES.has(lane.id)) continue;
      if (lane.main_posture !== 'coalesced') {
        problems.push(
          `${lane.id}: owner ${workflow} is latest-main enrolled but lane posture is ${lane.main_posture}`,
        );
      }
    }
  }

  const benchmark = registry.lanes.filter(
    (lane) => lane.owner.workflow === '.github/workflows/perf-benchmark.yml',
  );
  if (benchmark.length === 0 || benchmark.some((lane) => lane.main_posture !== 'never')) {
    problems.push(
      'perf-benchmark schedule is coalesced, but its no-push main posture must remain never',
    );
  }

  const nightlyNotify = registry.lanes.find((lane) => lane.id === 'perf-nightly.perf-nightly-health-notify');
  if (!nightlyNotify || nightlyNotify.main_posture !== 'never') {
    problems.push(
      'perf-nightly.perf-nightly-health-notify must remain main-never — it runs only on schedule ' +
        "(if: github.event_name == 'schedule'), never on the push-to-main trigger the rest of its " +
        'owning workflow coalesces for',
    );
  }

  const quickstart = registry.lanes.find((lane) => lane.id === 'e2e.quickstart');
  if (!quickstart || quickstart.main_posture !== 'always') {
    problems.push('e2e.quickstart must remain main-always, never main-coalesced');
  }
  const postMerge = registry.lanes.find(
    (lane) => lane.id === 'quality.post-merge-drift',
  );
  if (!postMerge || postMerge.main_posture !== 'always') {
    problems.push(
      'quality.post-merge-drift must remain main-always because every landed tree is distinct',
    );
  }

  return problems;
}

export function main(root = process.cwd()) {
  const problems = auditMainCoalescing(root);
  if (problems.length > 0) {
    for (const problem of problems) error(problem, { title: 'main coalescing' });
    return 1;
  }
  notice(
    `latest-main grouping covers ${COALESCED_WORKFLOWS.length} deep-test workflow(s): ` +
      `${QUEUE_COALESCED_WORKFLOWS.length} queue-coalesced (grouped, never killed mid-flight), ` +
      `${CANCEL_COALESCED_WORKFLOWS.length} still cancelling; distinct events remain unique`,
    { title: 'main coalescing' },
  );
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(SCRIPT)) {
  const status = main();
  if (status === 0) log('main-coalescing: OK');
  process.exitCode = status;
}
