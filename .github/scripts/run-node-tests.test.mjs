// Tests for run-node-tests.mjs: the stop/resume markers bracket the suites
// under Actions, a suite's own workflow commands pass through as text, and
// the exit code of `node --test` is the wrapper's.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import test from 'node:test';

import { stopCommandsMarkers } from './run-node-tests.mjs';

const wrapper = join(import.meta.dirname, 'run-node-tests.mjs');

// suite writes a throwaway suite that prints an ::error:: line on stderr, as a
// gate script does on a failure path, and then passes or fails.
function suite(passes) {
  const dir = mkdtempSync(join(tmpdir(), 'run-node-tests-'));
  const file = join(dir, 'probe.test.mjs');
  writeFileSync(
    file,
    "import test from 'node:test';\n" +
      "test('probe', () => {\n" +
      "  process.stderr.write('::error::boom\\n');\n" +
      (passes ? '' : "  throw new Error('suite failure');\n") +
      '});\n',
  );
  return file;
}

function runWrapper(args, inActions) {
  const env = { ...process.env, GITHUB_ACTIONS: inActions ? 'true' : '' };
  return spawnSync(process.execPath, [wrapper, ...args], { env, encoding: 'utf8' });
}

test('under Actions the suites run between a stop marker and its own resume marker', () => {
  const res = runWrapper([suite(true)], true);
  assert.equal(res.status, 0, res.stderr);
  const lines = res.stdout.split('\n');
  const stop = lines.findIndex((l) => l.startsWith('::stop-commands::'));
  assert.notEqual(stop, -1, `no stop marker in:\n${res.stdout}`);
  const token = lines[stop].slice('::stop-commands::'.length);
  assert.ok(token.length > 0, 'empty token');
  const resume = lines.indexOf(`::${token}::`);
  // node --test relays a suite's stderr onto its own stdout, so the suite's
  // ::error:: line must sit between the two markers there. The spec reporter
  // relays it as is and the TAP reporter (Node 20's default off a TTY) as a
  // `# ` comment, so the line is found by its text, not its start.
  const command = lines.findIndex((l) => l.includes('::error::boom'));
  assert.ok(stop < command && command < resume, `want stop < ::error::boom < resume, got ${stop}, ${command}, ${resume}:\n${res.stdout}`);
});

test('a failing suite fails the wrapper and names it after the resume marker', () => {
  const failing = suite(false);
  const res = runWrapper([failing], true);
  assert.notEqual(res.status, 0);
  const lines = res.stdout.split('\n');
  const stop = lines.findIndex((l) => l.startsWith('::stop-commands::'));
  assert.notEqual(stop, -1, `no stop marker in:\n${res.stdout}`);
  const resume = lines.indexOf(`::${lines[stop].slice('::stop-commands::'.length)}::`);
  const named = lines.findIndex((l) => l.startsWith('::error::run-node-tests:') && l.includes(failing));
  assert.ok(resume !== -1 && named > resume, `want the failure named after the resume marker, got resume ${resume}, error ${named}:\n${res.stdout}`);
});

test('a passing suite prints no failure annotation', () => {
  const res = runWrapper([suite(true)], true);
  assert.equal(res.status, 0, res.stderr);
  assert.doesNotMatch(res.stdout, /^::error::run-node-tests:/m);
});

test('outside Actions no markers are printed', () => {
  const res = runWrapper([suite(true)], false);
  assert.equal(res.status, 0, res.stderr);
  assert.doesNotMatch(res.stdout, /stop-commands/);
});

test('naming no suite is an error', () => {
  const res = runWrapper([], false);
  assert.equal(res.status, 1);
  assert.match(res.stdout, /no suite named/);
});

test('each invocation gets a fresh token', () => {
  assert.notEqual(stopCommandsMarkers().stop, stopCommandsMarkers().stop);
  assert.deepEqual(stopCommandsMarkers('t'), { stop: '::stop-commands::t', resume: '::t::' });
});
