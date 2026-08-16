// main-coalescing.mjs — structural guard for replaceable deep-main work.
//
// GitHub evaluates workflow concurrency before any job can run, so the policy
// has two halves: the exact expressions enrolled workflows carry and this
// executable model that proves which event/ref pairs those expressions may
// cancel. Only a push or scheduled run bound to refs/heads/main is replaceable.
// Pull requests, merge groups, manual qualification, reusable-workflow calls,
// maintenance branches, tags, releases, and unknown inputs receive a unique
// run group and can never cancel one another.
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
  "${{ github.workflow }}-${{ ((github.event_name == 'push' || github.event_name == 'schedule') && github.ref == 'refs/heads/main') && 'latest-main' || github.run_id }}";
export const CANCEL_EXPRESSION =
  "${{ (github.event_name == 'push' || github.event_name == 'schedule') && github.ref == 'refs/heads/main' }}";

// These workflows contain replaceable deep-main, soak, or scheduled test work.
// A schedule and a push on main intentionally share one group per workflow, so
// the newest run is the only one allowed to consume the deep-test budget.
export const COALESCED_WORKFLOWS = Object.freeze([
  '.github/workflows/agpl-oracle.yml',
  '.github/workflows/chdb.yml',
  '.github/workflows/codeql.yml',
  '.github/workflows/compatibility.yml',
  '.github/workflows/coverage.yml',
  '.github/workflows/migration-e2e.yml',
  '.github/workflows/mutation.yml',
  '.github/workflows/perf-benchmark.yml',
  '.github/workflows/perf-profile.yml',
  '.github/workflows/property.yml',
  '.github/workflows/schema-integration.yml',
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

export const QUICKSTART_GROUP_EXPRESSION =
  "quickstart-${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}";
export const QUICKSTART_CANCEL_EXPRESSION =
  "${{ github.event_name == 'pull_request' }}";

export function coalescingDecision({
  eventName,
  ref,
  workflow = 'workflow',
  runId = 'run',
} = {}) {
  const event = String(eventName ?? '');
  const boundRef = String(ref ?? '');
  const replaceable =
    (event === 'push' || event === 'schedule') && boundRef === MAIN_REF;
  return Object.freeze({
    cancelInProgress: replaceable,
    group: `${workflow}-${replaceable ? LATEST_MAIN_SUFFIX : runId}`,
    reason: replaceable ? 'latest_main' : 'distinct_run',
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

export function validateEnrolledWorkflow(workflow, workflowText) {
  const problems = [];
  let concurrency;
  try {
    concurrency = parseTopLevelConcurrency(workflowText);
  } catch (caught) {
    return [`${workflow}: ${caught.message}`];
  }
  if (concurrency.group !== GROUP_EXPRESSION) {
    problems.push(
      `${workflow}: concurrency group is ${JSON.stringify(concurrency.group)}, want the canonical latest-main/unique-run expression`,
    );
  }
  if (concurrency.cancelInProgress !== CANCEL_EXPRESSION) {
    problems.push(
      `${workflow}: cancel-in-progress is ${JSON.stringify(concurrency.cancelInProgress)}, want the canonical main-push/schedule-only expression`,
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

export function auditMainCoalescing(root = process.cwd()) {
  const problems = [];
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
    `latest-main cancellation is confined to ${COALESCED_WORKFLOWS.length} deep-test workflow(s); distinct events remain unique`,
    { title: 'main coalescing' },
  );
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(SCRIPT)) {
  const status = main();
  if (status === 0) log('main-coalescing: OK');
  process.exitCode = status;
}
