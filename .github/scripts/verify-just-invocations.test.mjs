// verify-just-invocations.test.mjs — node:test guard for the Justfile-split
// CI-safety gate (#3093).
//
// Two layers, matching repo-hygiene.test.mjs's own split:
//   1. Unit tests against the pure extraction functions — the part a naive
//      whole-file grep gets wrong (YAML comments, prose inside an echoed
//      shell string, a GitHub Actions `${{ }}` expression as one shell
//      word, and — the regression this file exists to pin — a SECOND `just`
//      invocation on the very next line of the same `run: |` block, which an
//      earlier version of findInvocations() silently dropped by walking
//      backward across the separating newline in search of an operator).
//   2. End-to-end CLI tests against a real throwaway git repo with a real
//      Justfile, proving the exit code and the named failure, exactly as
//      repo-hygiene.test.mjs does for its own gate.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  collectInvocations,
  extractRunBlocks,
  findInvocations,
  lineForOffset,
  neutralizeShellText,
} from './verify-just-invocations.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'verify-just-invocations.mjs');

// ---- extractRunBlocks -------------------------------------------------

test('extractRunBlocks: single-line run: value', () => {
  const yaml = ['jobs:', '  x:', '    steps:', '      - name: a', '        run: just build', ''].join('\n');
  const blocks = extractRunBlocks(yaml);
  assert.deepEqual(blocks, [{ text: 'just build', startLine: 5 }]);
});

test('extractRunBlocks: block-scalar run: | value, stops at dedent', () => {
  const yaml = [
    'jobs:',
    '  x:',
    '    steps:',
    '      - name: a',
    '        run: |',
    '          just build',
    '          just test-unit',
    '      - name: b',
    '        run: just lint',
    '',
  ].join('\n');
  const blocks = extractRunBlocks(yaml);
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].text, 'just build\njust test-unit');
  assert.equal(blocks[0].startLine, 6);
  assert.equal(blocks[1].text, 'just lint');
});

test('extractRunBlocks: never captures a YAML comment line', () => {
  const yaml = ['jobs:', '  x:', '    steps:', '      # run: just not-a-real-step', '      - name: a', '        run: just build', ''].join(
    '\n',
  );
  const blocks = extractRunBlocks(yaml);
  assert.deepEqual(blocks, [{ text: 'just build', startLine: 6 }]);
});

// ---- neutralizeShellText ------------------------------------------------

test('neutralizeShellText: preserves length and newline positions', () => {
  const text = 'just _pull-retry "$CH_IMAGE"\njust build';
  const out = neutralizeShellText(text);
  assert.equal(out.length, text.length);
  assert.equal(out.indexOf('\n'), text.indexOf('\n'));
});

test('neutralizeShellText: quoted echo prose no longer spells "just"', () => {
  const text = 'echo "run \'just gen-opt-docs\' and commit the result"';
  const out = neutralizeShellText(text);
  assert.equal(/\bjust\b/.test(out), false);
});

test('neutralizeShellText: a GitHub Actions expression collapses to one word', () => {
  const text = "just e2e-bwc-up ${{ matrix.scenario == 'x' && 'object-storage' || matrix.scenario }}";
  const out = neutralizeShellText(text);
  const tokens = out.trim().split(/\s+/);
  assert.deepEqual(tokens.slice(0, 2), ['just', 'e2e-bwc-up']);
  assert.equal(tokens.length, 3); // "just", "e2e-bwc-up", and the whole expression as ONE token
});

test('neutralizeShellText: strips an unquoted inline shell comment', () => {
  const text = 'just build # this comment says just do it twice';
  const out = neutralizeShellText(text);
  assert.equal(out.trim(), 'just build');
});

// ---- findInvocations -----------------------------------------------------

test('findInvocations: two invocations on consecutive lines of one block (regression)', () => {
  // Pinned regression: an earlier version walked backward across the
  // newline separating these two lines in search of a preceding shell
  // operator, found none, and silently dropped the second invocation.
  const text = 'just test-chaos-sleep\njust test-unit';
  const invocations = findInvocations(text);
  assert.deepEqual(
    invocations.map((i) => i.recipe),
    ['test-chaos-sleep', 'test-unit'],
  );
});

test('findInvocations: a quoted argument counts as exactly one argument', () => {
  const text = neutralizeShellText('just _pull-retry "$CH_IMAGE"');
  const invocations = findInvocations(text);
  assert.equal(invocations.length, 1);
  assert.equal(invocations[0].recipe, '_pull-retry');
  assert.equal(invocations[0].args.length, 1);
});

test('findInvocations: "just" as the second word of its own command is not flagged', () => {
  // No real call site in this repo wraps `just` in another command; this
  // pins that findInvocations would correctly ignore one if it appeared.
  const invocations = findInvocations('time just build');
  assert.deepEqual(invocations, []);
});

test('findInvocations: a bare `just --list` (flag, not a recipe) is ignored', () => {
  const invocations = findInvocations('just --list');
  assert.deepEqual(invocations, []);
});

test('findInvocations: invocations chained with && are both found', () => {
  const invocations = findInvocations('just build && just test-unit');
  assert.deepEqual(
    invocations.map((i) => i.recipe),
    ['build', 'test-unit'],
  );
});

// ---- lineForOffset / collectInvocations -----------------------------------

test('lineForOffset: counts newlines within the block relative to its start line', () => {
  const text = 'just build\njust test-unit\njust lint';
  assert.equal(lineForOffset(text, 0, 10), 10);
  assert.equal(lineForOffset(text, text.indexOf('test-unit'), 10), 11);
  assert.equal(lineForOffset(text, text.indexOf('lint'), 10), 12);
});

test('collectInvocations: end-to-end over a small synthetic workflow source', () => {
  const yaml = [
    'jobs:',
    '  x:',
    '    steps:',
    '      # not just prose, this line is a YAML comment',
    '      - name: a',
    '        run: |',
    '          just build',
    '          || { echo "run \'just gen-opt-docs\' and commit the result"; exit 1; }',
    '      - name: b',
    '        run: just _pull-retry "$CH_IMAGE"',
    '',
  ].join('\n');
  const found = collectInvocations('fake.yml', yaml);
  assert.deepEqual(
    found.map((f) => `${f.file}:${f.line} ${f.recipe} ${f.args.length}`),
    ['fake.yml:7 build 0', 'fake.yml:10 _pull-retry 1'],
  );
});

// ---- CLI end-to-end --------------------------------------------------------

// newFixtureRepo — a throwaway repo carrying a minimal real Justfile (no
// COMPOSE_PROJECT_SUFFIX shell-out, so it runs anywhere `just` is on PATH)
// plus one workflow file, matching the shape repo-hygiene.test.mjs uses for
// its own CLI-level assertions.
function newFixtureRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'verify-just-invocations-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(
    join(dir, 'Justfile'),
    ['build:', '    echo building', '', 'mutate-pkg PATH:', '    echo "{{PATH}}"', ''].join('\n'),
  );
  return {
    dir,
    writeWorkflow(content) {
      writeFileSync(join(dir, '.github', 'workflows', 'fake.yml'), content);
    },
  };
}

function runCli(cwd) {
  return spawnSync(process.execPath, [CLI], { cwd, encoding: 'utf8', env: { ...process.env, REPO_ROOT: cwd } });
}

test('CLI: exits 0 on a workflow whose invocations all resolve, prose included', () => {
  const fx = newFixtureRepo();
  fx.writeWorkflow(
    [
      'jobs:',
      '  x:',
      '    steps:',
      '      # not just string-asserted, a comment mentioning "just" as prose',
      '      - name: a',
      '        run: |',
      '          just build',
      '          || { echo "run \'just gen-opt-docs\' and commit the result"; exit 1; }',
      '',
    ].join('\n'),
  );
  const res = runCli(fx.dir);
  assert.equal(res.status, 0, `expected clean exit, got: ${res.stdout}\n${res.stderr}`);
});

test('CLI: exits non-zero and names a missing recipe', () => {
  const fx = newFixtureRepo();
  fx.writeWorkflow(['jobs:', '  x:', '    steps:', '      - name: a', '        run: just this-recipe-does-not-exist', ''].join('\n'));
  const res = runCli(fx.dir);
  assert.notEqual(res.status, 0);
  assert.match(res.stdout, /this-recipe-does-not-exist/);
  assert.match(res.stdout, /fake\.yml:5/);
});

test('CLI: exits non-zero and names an arity mismatch', () => {
  const fx = newFixtureRepo();
  fx.writeWorkflow(['jobs:', '  x:', '    steps:', '      - name: a', '        run: just mutate-pkg', ''].join('\n'));
  const res = runCli(fx.dir);
  assert.notEqual(res.status, 0);
  assert.match(res.stdout, /mutate-pkg/);
});
