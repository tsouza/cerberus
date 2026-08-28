// Tests for the Go-setup ratchet.
//
// The point of every case below is that the gate CAN FAIL, and fails for the
// right reason. "The tree is clean" is not evidence on its own: it is equally
// consistent with a scanner that never read the workflows. So each rule is
// proved against the REAL text of this repository with exactly one thing broken
// in it — a call site reverted to `actions/setup-go@v7`, the warm step deleted,
// the warm step made conditional — rather than against a hand-written fixture
// that merely resembles a workflow.
//
// The one case that runs the other way is `cache: false`. update-golden.yml's
// three jobs run target-branch code and must not persist bytes into later
// workflows, so the value is load-bearing; a gate that forced `cache: true`
// would break that isolation. That it stays green is asserted directly, because
// "the gate does not overreach" is as easy to lose in an edit as "the gate
// fires".

import assert from 'node:assert/strict';
import { cpSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { compositeSteps, directSetupGoUses, scan, usesValue, wrapperUses } from './assert-go-setup-hardened.mjs';

const repoRoot = process.cwd();
const workflows = join(repoRoot, '.github/workflows');
const actions = join(repoRoot, '.github/actions');
const wrapperFile = join(actions, 'setup-go/action.yml');

// A workflow whose Go setup is unconditional and whose `with:` block was
// dropped as default — the ordinary shape, and so the honest specimen.
const specimenWorkflow = 'ci.yml';

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
  const { violations, wrapperCallSites } = realScan();
  assert.deepEqual(violations, []);
  assert.ok(wrapperCallSites > 0, 'no call site was found — the gate is not reading the workflows');
});

test('every Go setup in the tree goes through the wrapper', () => {
  // Counted independently of the gate's own bookkeeping, so a scanner that
  // silently stopped reading files would fail here rather than report a
  // satisfied-looking zero.
  let direct = 0;
  let wrapped = 0;
  for (const file of readdirSync(workflows).filter((f) => /\.ya?ml$/.test(f))) {
    const text = readFileSync(join(workflows, file), 'utf8');
    direct += directSetupGoUses(text).length;
    wrapped += wrapperUses(text);
  }
  assert.equal(direct, 0);
  assert.ok(wrapped >= 50, `expected the whole fleet of call sites, saw ${wrapped}`);
});

test('`cache: false` is permitted — the gate never inspects the input', () => {
  // update-golden.yml keeps `cache: false` on all three of its jobs. If the
  // gate ever started demanding `true`, this is the assertion that says so
  // before the isolation those jobs depend on is broken.
  const text = readFileSync(join(workflows, 'update-golden.yml'), 'utf8');
  assert.ok(/cache: false/.test(text), 'the specimen no longer carries the value under test');
  assert.ok(wrapperUses(text) >= 3, 'update-golden should reach Go setup through the wrapper');

  const { violations } = scanWithWorkflow('update-golden.yml', (t) => `${t}\n# doctored: no change of substance\n`);
  assert.deepEqual(violations, []);
});

// ---------------------------------------------------------------------------
// R1 — the direct call site.
// ---------------------------------------------------------------------------

test('a call site reverted to actions/setup-go fails the gate', () => {
  const { violations } = scanWithWorkflow(specimenWorkflow, (t) =>
    t.replace('uses: ./.github/actions/setup-go', 'uses: actions/setup-go@v7'),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /uses `actions\/setup-go@v7` directly/);
});

test('an unversioned actions/setup-go is caught too', () => {
  const { violations } = scanWithWorkflow(specimenWorkflow, (t) =>
    t.replace('uses: ./.github/actions/setup-go', 'uses: actions/setup-go'),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
});

test('a composite action that reaches for setup-go directly fails the gate', () => {
  // The wrapper is exempt by IDENTITY, not by name: any OTHER action.yml under
  // .github/actions/ naming the upstream Action reopens the hole, so the gate
  // has to read that tree as well as the workflows.
  const dir = mkdtempSync(join(tmpdir(), 'go-setup-gate-sibling-'));
  cpSync(actions, dir, { recursive: true });
  const sibling = join(dir, 'free-disk-space/action.yml');
  const text = readFileSync(sibling, 'utf8');
  writeFileSync(sibling, `${text}\n    - uses: actions/setup-go@v7\n`);

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
  // compatibility.yml's header lists the Actions it runs. A grep would read
  // that inventory as a violation, and a gate that fired on a comment is a gate
  // somebody writes an exemption for.
  assert.equal(usesValue('#   * actions/setup-go@v7           — driver builds.'), null);
  assert.equal(directSetupGoUses('# uses: actions/setup-go@v7\n').length, 0);
  assert.equal(directSetupGoUses('      - name: replace actions/setup-go@v7\n').length, 0);
  assert.equal(directSetupGoUses('      - uses: actions/setup-go@v7\n').length, 1);
});

// ---------------------------------------------------------------------------
// R2 / R3 — the wrapper is real, and its warm step is unconditional.
// ---------------------------------------------------------------------------

test('a wrapper that no longer warms the module cache fails the gate', () => {
  // Only the RUN line, not the header prose that also names the module: the
  // rule is about what the step executes.
  const { violations } = scanWithWrapper((t) =>
    t.replace('node "$GITHUB_WORKSPACE/.github/scripts/go-module-fetch.mjs"', 'echo skipped'),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /runs no `go-module-fetch\.mjs` step/);
});

test('a warm step made conditional fails the gate', () => {
  const { violations } = scanWithWrapper((t) =>
    t.replace('    - name: Warm the Go module cache', "    - if: inputs.cache == 'true'\n      name: Warm the Go module cache"),
  );
  assert.equal(violations.length, 1, violations.join('\n'));
  assert.match(violations[0], /carries an `if:`/);
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
// R4 — vacuity.
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
// The step reader, pinned directly.
// ---------------------------------------------------------------------------

test('the wrapper parses into its two steps, in order', () => {
  const steps = compositeSteps(readFileSync(wrapperFile, 'utf8'));
  assert.equal(steps.length, 2);
  assert.ok(steps[0].some((l) => l.includes('actions/setup-go@v7')));
  assert.ok(steps[1].some((l) => l.includes('go-module-fetch.mjs')));
});
