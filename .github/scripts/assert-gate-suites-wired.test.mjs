// Tests for the gate-suite wiring check.
//
// This suite has to exist for the gate to be honest about itself: a checker
// that requires every suite to be wired, while shipping without one, would be
// the first violation of its own rule.

import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { bareInvocations, invokedSuites, scan } from './assert-gate-suites-wired.mjs';

// tree builds a throwaway repo with the given scripts and workflow contents.
function tree({ scripts = {}, workflows = {} }) {
  const root = mkdtempSync(join(tmpdir(), 'gate-wiring-'));
  mkdirSync(join(root, '.github/scripts/lib'), { recursive: true });
  mkdirSync(join(root, '.github/workflows'), { recursive: true });
  for (const [name, body] of Object.entries(scripts)) {
    writeFileSync(join(root, '.github/scripts', name), body);
  }
  for (const [name, body] of Object.entries(workflows)) {
    writeFileSync(join(root, '.github/workflows', name), body);
  }
  return root;
}

test('a suite no workflow invokes is reported', () => {
  const root = tree({
    scripts: { 'a.test.mjs': '', 'b.test.mjs': '' },
    workflows: { 'ci.yml': 'steps:\n  - run: node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs\n' },
  });
  const { unwired } = scan({ root });
  assert.deepEqual(unwired, ['.github/scripts/b.test.mjs']);
});

test('a fully wired tree is clean', () => {
  const root = tree({
    scripts: { 'a.test.mjs': '' },
    workflows: { 'ci.yml': 'steps:\n  - run: node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs\n' },
  });
  assert.deepEqual(scan({ root }).unwired, []);
});

test('suites in subdirectories are covered', () => {
  // lib/ carries its own suites; a scan that only read the top level would
  // report a satisfied-looking zero while missing them.
  const root = tree({ scripts: {}, workflows: { 'ci.yml': '' } });
  writeFileSync(join(root, '.github/scripts/lib/x.test.mjs'), '');
  const { suites, unwired } = scan({ root });
  assert.deepEqual(suites, ['.github/scripts/lib/x.test.mjs']);
  assert.deepEqual(unwired, ['.github/scripts/lib/x.test.mjs']);
});

test('an invocation in any workflow file counts', () => {
  const root = tree({
    scripts: { 'a.test.mjs': '' },
    workflows: {
      'ci.yml': 'steps:\n  - run: echo nothing\n',
      'coverage.yml': 'steps:\n  - run: node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs\n',
    },
  });
  assert.deepEqual(scan({ root }).unwired, []);
});

test('invokedSuites reads multiple invocations from one run block', () => {
  const found = invokedSuites(
    'run: |\n  node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs\n  node .github/scripts/run-node-tests.mjs .github/scripts/lib/b.test.mjs\n',
  );
  assert.deepEqual([...found].sort(), ['.github/scripts/a.test.mjs', '.github/scripts/lib/b.test.mjs']);
});

test('invokedSuites does not credit a non-suite target', () => {
  assert.deepEqual([...invokedSuites('run: node .github/scripts/run-node-tests.mjs somewhere/else.mjs\n')], []);
});

test('invokedSuites reads every suite one wrapper invocation names', () => {
  const found = invokedSuites('run: node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs .github/scripts/b.test.mjs\n');
  assert.deepEqual([...found].sort(), ['.github/scripts/a.test.mjs', '.github/scripts/b.test.mjs']);
});

test('a bare node --test does not wire a suite and is reported', () => {
  // A bare invocation runs the suite but lets the ::error:: lines its failure
  // paths print reach the runner as annotations on a green run.
  const root = tree({
    scripts: { 'a.test.mjs': '' },
    workflows: { 'ci.yml': 'steps:\n  - run: node --test .github/scripts/a.test.mjs\n' },
  });
  const { unwired, bare } = scan({ root });
  assert.deepEqual(unwired, ['.github/scripts/a.test.mjs']);
  assert.deepEqual(bare, ['.github/workflows/ci.yml: node --test .github/scripts/a.test.mjs']);
});

test('a bare node --test in a just recipe is reported', () => {
  const root = tree({
    scripts: { 'a.test.mjs': '' },
    workflows: { 'ci.yml': 'steps:\n  - run: node .github/scripts/run-node-tests.mjs .github/scripts/a.test.mjs\n' },
  });
  mkdirSync(join(root, 'just'));
  writeFileSync(join(root, 'just/test.just'), 'recipe:\n    node --test .github/scripts/a.test.mjs\n');
  assert.deepEqual(scan({ root }).bare, ['just/test.just: node --test .github/scripts/a.test.mjs']);
});

test('bareInvocations ignores prose that names node --test without a suite', () => {
  assert.deepEqual(bareInvocations('# runs the gate scripts\' own `node --test` suites\n'), []);
});
