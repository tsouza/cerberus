// Pins how the commitlint gates get their toolchain.
//
// The required `pr-body` job has a 5-minute budget. Installing commitlint from
// the registry on every run (`npm install @commitlint/cli ...` after a fresh
// Node download) used most of that budget and timed the job out under load.
// The toolchain is pinned in package-lock.json and restored
// from a cache by the `setup-commitlint` composite action; these tests fail if
// any workflow goes back to a per-run registry install, or runs a commitlint
// gate without that action.

import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { COMMITLINT_CLI } from './commitlint-range.mjs';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const WORKFLOWS = join(ROOT, '.github/workflows');
const ACTION = '.github/actions/setup-commitlint';
const GATES = ['commitlint-range.mjs', 'commitlint-pr-title.mjs'];

function workflows() {
  return readdirSync(WORKFLOWS)
    .filter((f) => f.endsWith('.yml') || f.endsWith('.yaml'))
    .map((f) => ({ file: f, text: readFileSync(join(WORKFLOWS, f), 'utf8') }));
}

// jobs splits a workflow into its jobs by the two-space job header.
function jobs(text) {
  const body = text.slice(text.indexOf('\njobs:\n'));
  return body.split(/\n(?=  [A-Za-z0-9_-]+:\n)/).slice(1);
}

test('no workflow installs commitlint from the registry', () => {
  for (const { file, text } of workflows()) {
    assert.doesNotMatch(text, /npm (install|i)\b[^\n]*@commitlint\//, `${file} installs commitlint per run`);
    assert.doesNotMatch(text, /npx (?!--no)[^\n]*commitlint/, `${file} lets npx fetch commitlint`);
  }
});

test('every job that runs a commitlint gate sets up the cached toolchain first', () => {
  let gatedJobs = 0;
  for (const { file, text } of workflows()) {
    for (const job of jobs(text)) {
      const gate = Math.min(...GATES.map((g) => job.indexOf(`node .github/scripts/${g}`)).filter((i) => i >= 0));
      if (!Number.isFinite(gate)) continue;
      gatedJobs++;
      const setup = job.indexOf(`uses: ./${ACTION}`);
      assert.ok(setup >= 0 && setup < gate, `${file}: job "${job.split(':')[0].trim()}" runs a commitlint gate without ${ACTION} before it`);
    }
  }
  assert.ok(gatedJobs >= GATES.length, `expected every commitlint gate to be wired, found ${gatedJobs} job(s)`);
});

test('the action restores node_modules keyed on the lockfile and installs with npm ci on a miss', () => {
  const action = readFileSync(join(ROOT, ACTION, 'action.yml'), 'utf8');
  assert.match(action, /uses: actions\/cache@/);
  assert.match(action, /path: node_modules\n/);
  assert.match(action, /key: [^\n]*hashFiles\('package-lock\.json'\)/);
  assert.match(action, /npm ci --no-audit/);
  assert.doesNotMatch(action, /npm install/);
  const skips = action.match(/if: steps\.cache\.outputs\.cache-hit != 'true'/g) ?? [];
  assert.equal(skips.length, 1, 'only the package install must be skipped on a cache hit');
});

test('Node itself is set up unconditionally, even on a cache hit', () => {
  // These self-hosted runner pods are ephemeral and do not ship Node
  // preinstalled: gating setup-node on the SAME cache-hit condition as
  // npm ci leaves npm/node off PATH entirely whenever node_modules is
  // restored from cache, which is every run after the first.
  const action = readFileSync(join(ROOT, ACTION, 'action.yml'), 'utf8');
  const setupNode = action.indexOf('uses: actions/setup-node@');
  assert.ok(setupNode >= 0, 'setup-commitlint must set up Node');
  const precedingLines = action.slice(0, setupNode).split('\n');
  const stepStart = precedingLines.lastIndexOf(
    precedingLines.slice().reverse().find((l) => /^\s*- name:/.test(l)),
  );
  const stepText = precedingLines.slice(stepStart).join('\n');
  assert.doesNotMatch(
    stepText,
    /if: steps\.cache\.outputs\.cache-hit/,
    'setup-node must not be gated on the node_modules cache-hit — it installs the Node runtime, not the packages',
  );
});

test('the gates run the CLI from the pinned lockfile', () => {
  assert.equal(relative(ROOT, COMMITLINT_CLI), 'node_modules/@commitlint/cli/cli.js');
  const lock = JSON.parse(readFileSync(join(ROOT, 'package-lock.json'), 'utf8'));
  const pkg = JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8'));
  for (const name of ['@commitlint/cli', '@commitlint/config-conventional']) {
    const pinned = pkg.devDependencies[name];
    assert.match(pinned, /^\d+\.\d+\.\d+$/, `${name} must be an exact version`);
    assert.equal(lock.packages[`node_modules/${name}`].version, pinned, `${name} lockfile drifted from package.json`);
  }
});
