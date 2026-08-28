// Tests for the Go-setup ratchet.
//
// The point of every case below is that the gate CAN FAIL, and fails for the
// right reason. "The tree is clean" is not evidence on its own: it is equally
// consistent with a scanner that never read the workflows. So each rule is
// proved against the REAL text of this repository with exactly one thing broken
// in it — a call site reverted to `actions/setup-go@v7`, a `cache: false` step
// stripped of its warm step, the wrapper's warm step deleted or made
// conditional — rather than against a hand-written fixture that merely
// resembles a workflow.
//
// The cases that run the other way matter just as much. `update-golden.yml`'s
// three jobs reach the Action directly, on purpose: they check out
// target-branch code under the default branch's privileges, and a literal
// `cache: false` is what keeps that code from persisting bytes into later
// workflows — legibly enough for CodeQL's cache-poisoning query to agree. A
// gate that forced them through the composite would break that boundary, so
// "the gate does not overreach" is asserted directly, twice.

import assert from 'node:assert/strict';
import { cpSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
  expressionsOutsideRuns,
  hasKeyAtStepLevel,
  isUpstreamSetupGo,
  scan,
  stepBlocks,
  stepUses,
  usesValue,
  withEntry,
} from './assert-go-setup-hardened.mjs';

const repoRoot = process.cwd();
const workflows = join(repoRoot, '.github/workflows');
const actions = join(repoRoot, '.github/actions');
const wrapperFile = join(actions, 'setup-go/action.yml');

// A workflow whose Go setup is unconditional and whose `with:` block was
// dropped as default — the ordinary shape, and so the honest specimen.
const specimenWorkflow = 'ci.yml';

// The one workflow that reaches the Action directly, under R1's conditions.
const directWorkflow = 'update-golden.yml';

function realScan() {
  return scan({ root: repoRoot, workflowDir: workflows, actionsDir: actions, wrapperPath: join(actions, 'setup-go') });
}

// scanWithWorkflow — the real tree, with ONE workflow replaced by a doctored
// copy. The actions directory and the wrapper still resolve against the real
// tree, so what is under test is the real gate reading real text.
function scanWithWorkflow(file, edit) {
  const dir = mkdtempSync(join(tmpdir(), 'go-setup-gate-wf-'));
  const original = readFileSync(join(workflows, file), 'utf8');
  const doctored = edit(original);
  assert.notEqual(doctored, original, 'the negative control did not change the workflow text');
  writeFileSync(join(dir, file), doctored);
  return scan({ root: repoRoot, workflowDir: dir, actionsDir: actions, wrapperPath: join(actions, 'setup-go') });
}

// scanWithWrapper — the real workflows, with the wrapper's own action.yml
// doctored in a copy of the actions tree.
function scanWithWrapper(edit) {
  const dir = mkdtempSync(join(tmpdir(), 'go-setup-gate-actions-'));
  cpSync(actions, dir, { recursive: true });
  const target = join(dir, 'setup-go/action.yml');
  const original = readFileSync(target, 'utf8');
  const doctored = edit(original);
  assert.notEqual(doctored, original, 'the negative control did not change the action text');
  writeFileSync(target, doctored);
  return scan({ root: repoRoot, workflowDir: workflows, actionsDir: dir, wrapperPath: join(dir, 'setup-go') });
}

// ---------------------------------------------------------------------------
// The tree as it stands.
// ---------------------------------------------------------------------------

test('the repository as it stands passes every rule', () => {
  const { violations, wrapperCallSites, directCallSites } = realScan();
  assert.deepEqual(violations, []);
  assert.ok(wrapperCallSites >= 45, `expected the whole fleet of call sites, saw ${wrapperCallSites}`);
  assert.equal(directCallSites, 3, 'only update-golden.yml may reach the Action directly');
});

test('no Go setup in the tree can archive an empty module cache', () => {
  // Counted independently of the gate's own bookkeeping, so a scanner that
  // silently stopped reading files would fail here rather than report a
  // satisfied-looking zero.
  let cacheWriters = 0;
  let direct = 0;
  for (const file of ['ci.yml', 'chdb.yml', 'coverage.yml', 'e2e.yml', directWorkflow]) {
    for (const block of stepBlocks(readFileSync(join(workflows, file), 'utf8'))) {
      for (const step of block.steps) {
        if (!isUpstreamSetupGo(stepUses(step) ?? '')) continue;
        direct++;
        if (withEntry(step, 'cache') !== 'false') cacheWriters++;
      }
    }
  }
  assert.equal(cacheWriters, 0);
  assert.equal(direct, 3);
});

// ---------------------------------------------------------------------------
// R1 — a step that can SAVE the cache must go through the composite.
// ---------------------------------------------------------------------------

test('a call site reverted to actions/setup-go with caching fails the gate', () => {
  const { violations } = scanWithWorkflow(specimenWorkflow, (t) =>
    t.replace(
      '      - uses: ./.github/actions/setup-go\n',
      "      - uses: actions/setup-go@v7\n        with:\n          go-version-file: 'go.mod'\n          cache: true\n",
    ),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /uses `actions\/setup-go@v7` with `cache: true`/);
});

test('a bare actions/setup-go with no `with:` at all fails the gate', () => {
  const { violations } = scanWithWorkflow(specimenWorkflow, (t) =>
    t.replace('      - uses: ./.github/actions/setup-go\n', '      - uses: actions/setup-go@v7\n'),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /`cache: \(unset\)`/);
});

test('a templated cache value does not satisfy the literal rule', () => {
  // The exact shape that made CodeQL report update-golden as poisonable: a
  // value no static reader can resolve to false.
  const { violations } = scanWithWorkflow(directWorkflow, (t) =>
    t.replace('          cache: false\n', '          cache: ${{ inputs.cache }}\n'),
  );
  assert.ok(violations.length >= 1, 'a non-literal cache value must not pass');
  assert.match(violations[0], /cache-poisoning|must go through/);
});

// ---------------------------------------------------------------------------
// R2 — opting out of the cache is not opting out of the fetch.
// ---------------------------------------------------------------------------

test('a direct, cache-less setup with no warm step in its job fails the gate', () => {
  const { violations } = scanWithWorkflow(directWorkflow, (t) =>
    t.replaceAll('        run: node "$GITHUB_WORKSPACE/.github/scripts/go-module-fetch.mjs"\n', '        run: true\n'),
  );
  // Three jobs, three violations. (The scan also reports R5, because this
  // single-file scan contains no wrapper call site at all — that is the
  // vacuity rule doing its job, and it is asserted on its own below.)
  const unwarmed = violations.filter((v) => /never runs `go-module-fetch\.mjs`/.test(v));
  assert.equal(unwarmed.length, 3, violations.join('\n'));
  for (const label of ['regenerate', 'cardinality-leg', 'cardinality-seal']) {
    assert.ok(
      unwarmed.some((v) => v.includes(`:${label}:`)),
      `${label} was not reported: ${unwarmed.join('\n')}`,
    );
  }
});

test('update-golden pairs every direct setup with a warm step in the same job', () => {
  // The over-reach control, asserted on the real text: three direct call
  // sites, each in a job that also warms. A gate that forced these through the
  // composite would take the literal `cache: false` away from the repository's
  // most privileged workflow.
  const text = readFileSync(join(workflows, directWorkflow), 'utf8');
  let paired = 0;
  for (const block of stepBlocks(text)) {
    const direct = block.steps.filter((s) => isUpstreamSetupGo(stepUses(s) ?? ''));
    if (direct.length === 0) continue;
    assert.equal(withEntry(direct[0], 'cache'), 'false', `${block.label} must opt out of the shared cache`);
    assert.ok(
      block.steps.some((s) => s.some((l) => l.includes('go-module-fetch.mjs'))),
      `${block.label} must warm the module cache itself`,
    );
    paired++;
  }
  assert.equal(paired, 3);
  assert.deepEqual(realScan().violations, []);
});

// ---------------------------------------------------------------------------
// R3 — composite actions have no job scope, so they get no escape.
// ---------------------------------------------------------------------------

test('a composite action that reaches for setup-go directly fails the gate', () => {
  // The wrapper is exempt by IDENTITY, not by name: any OTHER action.yml under
  // .github/actions/ naming the upstream Action reopens the hole, so the gate
  // has to read that tree as well as the workflows. Even `cache: false` does
  // not save it — there is no job here in which R2 could be satisfied.
  const dir = mkdtempSync(join(tmpdir(), 'go-setup-gate-sibling-'));
  cpSync(actions, dir, { recursive: true });
  const sibling = join(dir, 'free-disk-space/action.yml');
  const text = readFileSync(sibling, 'utf8');
  writeFileSync(sibling, `${text}\n    - uses: actions/setup-go@v7\n      with:\n        cache: false\n`);

  const { violations } = scan({
    root: repoRoot,
    workflowDir: workflows,
    actionsDir: dir,
    wrapperPath: join(dir, 'setup-go'),
  });
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /free-disk-space\/action\.yml/);
});

test('a prose mention of the Action is not a call site', () => {
  // compatibility.yml's header lists the Actions it runs, and update-golden's
  // own comment explains why it names the Action. A grep would read both as
  // violations, and a gate that fired on a comment is a gate somebody writes an
  // exemption for.
  assert.equal(usesValue('#   * actions/setup-go@v7           — driver builds.'), null);
  assert.equal(usesValue('      - name: replace actions/setup-go@v7'), null);
  assert.equal(usesValue('      - uses: actions/setup-go@v7'), 'actions/setup-go@v7');
  assert.equal(isUpstreamSetupGo('actions/setup-go'), true);
  assert.equal(isUpstreamSetupGo('./.github/actions/setup-go'), false);
});

// ---------------------------------------------------------------------------
// R4 — the wrapper is real, and its warm step is unconditional.
// ---------------------------------------------------------------------------

test('a wrapper that no longer warms the module cache fails the gate', () => {
  // Only the RUN line, not the header prose that also names the module: the
  // rule is about what the step executes.
  const { violations } = scanWithWrapper((t) =>
    t.replace('node "$GITHUB_WORKSPACE/.github/scripts/go-module-fetch.mjs"', 'echo nothing'),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /runs no `go-module-fetch\.mjs` step/);
});

test('a warm step made conditional fails the gate', () => {
  const { violations } = scanWithWrapper((t) =>
    t.replace(
      '    - name: Warm the Go module cache',
      "    - if: inputs.cache == 'true'\n      name: Warm the Go module cache",
    ),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /carries an `if:`/);
});

test('an expression in the wrapper metadata fails the gate', () => {
  // Not a style rule. GitHub template-evaluates an action manifest's metadata,
  // and `inputs` is not a valid named-value there — so a `${{ inputs.cache }}`
  // written into an input DESCRIPTION as prose does not render, it fails the
  // manifest to LOAD. That is not hypothetical: it took 26 jobs red on this
  // very branch with `Unrecognized named-value: 'inputs'`, every one of them
  // dying before its first Go command.
  const { violations } = scanWithWrapper((t) =>
    t.replace(
      '      go.sum. Setting it to `false` opts a job out of the SHARED cache',
      '      go.sum, i.e. `${{ inputs.cache }}`. Setting it to `false` opts out',
    ),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /outside `runs:`/);

  // And the reader itself: an expression BELOW `runs:` is the ordinary,
  // required way to forward an input, and must never be reported.
  assert.deepEqual(expressionsOutsideRuns(readFileSync(wrapperFile, 'utf8')), []);
  assert.ok(
    readFileSync(wrapperFile, 'utf8').includes('cache: ${{ inputs.cache }}'),
    'the wrapper must still forward the input below `runs:`',
  );
});

test('a wrapper that installs no Go toolchain fails the gate', () => {
  const { violations } = scanWithWrapper((t) => t.replace('    - uses: actions/setup-go@v7', '    - name: nothing'));
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /does not delegate/);
});

test('a missing wrapper fails the gate rather than passing vacuously', () => {
  const empty = mkdtempSync(join(tmpdir(), 'go-setup-gate-empty-'));
  const { violations } = scan({
    root: repoRoot,
    workflowDir: workflows,
    actionsDir: empty,
    wrapperPath: empty,
  });
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /does not exist/);
});

// ---------------------------------------------------------------------------
// R5 — vacuity.
// ---------------------------------------------------------------------------

test('a tree where nothing sets Go up fails rather than reporting clean', () => {
  const dir = mkdtempSync(join(tmpdir(), 'go-setup-gate-vacuous-'));
  writeFileSync(join(dir, 'nothing.yml'), 'jobs:\n  x:\n    steps:\n      - run: echo hi\n');
  const { violations } = scan({
    root: repoRoot,
    workflowDir: dir,
    actionsDir: mkdtempSync(join(tmpdir(), 'go-setup-gate-noactions-')),
    wrapperPath: join(actions, 'setup-go'),
  });
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /no workflow or action uses/);
});

// ---------------------------------------------------------------------------
// The readers, pinned directly.
// ---------------------------------------------------------------------------

test('the wrapper parses into its two steps, in order', () => {
  const steps = stepBlocks(readFileSync(wrapperFile, 'utf8')).flatMap((b) => b.steps);
  assert.equal(steps.length, 2);
  assert.equal(stepUses(steps[0]), 'actions/setup-go@v7');
  assert.ok(steps[1].some((l) => l.includes('go-module-fetch.mjs')));
  assert.equal(hasKeyAtStepLevel(steps[1], 'if'), false);
});

test('a step block is scoped to its own job', () => {
  // R2 asks "does the SAME job warm", so a reader that merged two jobs into one
  // block would let a warm step in job A satisfy a direct setup in job B.
  const blocks = stepBlocks(readFileSync(join(workflows, directWorkflow), 'utf8'));
  assert.ok(blocks.length >= 5, `expected one block per job, saw ${blocks.length}`);
  assert.ok(blocks.some((b) => b.label === 'regenerate'));
  assert.ok(blocks.some((b) => b.label === 'plan'));
  // `plan` sets Go up not at all, so it must contribute no direct call site.
  const plan = blocks.find((b) => b.label === 'plan');
  assert.equal(
    plan.steps.filter((s) => isUpstreamSetupGo(stepUses(s) ?? '')).length,
    0,
  );
});
