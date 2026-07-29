// repo-hygiene.test.mjs — node:test guard for the committed-artefact gate.
//
// Runs on the CHEAP forbid-skip lane (`node --test`) — no setup-node, no
// deps, no substrate — alongside the other discipline self-tests.
//
// A gate that has never been shown to FAIL is indistinguishable from a gate
// that does nothing, so the end-to-end tests below build a real throwaway git
// repository, plant the exact artefacts this gate exists to catch, and run
// the real CLI against it as a subprocess:
//
//   1. classifyBlob detects executables by magic and text by absence of NUL,
//      and honours the sniff-window bound.
//   2. diffAllowlist reports a stray root entry AND a rotted allow-list entry.
//   3. the CLI exits 0 on a clean fixture tree (both scans).
//   4. the CLI exits non-zero, with an ::error:: naming the offender, on a
//      fixture carrying a synthetic ELF blob.
//   5. the CLI exits non-zero, with an ::error:: naming the offender, on a
//      fixture carrying a stray root file.
//
// The live tree is deliberately NOT asserted here: the workflow runs the real
// CLI against it in the two steps that follow this one, so duplicating that
// assertion would report one violation as three red steps and make a genuine
// artefact look like broken test infrastructure.
//
// Every byte sequence below is built from numeric literals rather than
// written as a raw character: a source file containing a literal NUL is
// itself a blob this gate rejects.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  ROOT_ALLOWLIST,
  classifyBlob,
  diffAllowlist,
  gitBinarySniffBytes,
  rootEntriesOf,
} from './repo-hygiene.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'repo-hygiene.mjs');

// Every fixture file gets comment-shaped content: harmless as prose, and
// harmless when the fixture's allow-listed `.gitignore` / `.gitmodules`
// entries are parsed by git as the config files they are named after.
const fixtureContent = '# repo-hygiene fixture\n';

// A minimal ELF header — the exact shape of the artefact that motivated this
// gate (a compiled Go binary committed at the repository root).
function syntheticElf() {
  return Buffer.concat([Buffer.from([0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01, 0x01]), Buffer.alloc(64)]);
}

// newFixtureRepo — a throwaway git repo whose root satisfies ROOT_ALLOWLIST
// exactly. Each allow-list entry is created as a plain file: the gate reduces
// tracked paths to their first path component, so a file named `internal`
// yields the same root entry a directory named `internal` would.
function newFixtureRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'repo-hygiene-'));
  const run = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  run(['init', '--quiet']);
  for (const entry of ROOT_ALLOWLIST) writeFileSync(join(dir, entry), fixtureContent);
  run(['add', '-A']);
  return { dir, stage: () => run(['add', '-A']) };
}

function runGate(check, cwd) {
  const res = spawnSync(process.execPath, [CLI], {
    encoding: 'utf8',
    env: { ...process.env, CHECK: check, REPO_ROOT: cwd },
  });
  return { status: res.status, out: `${res.stdout}${res.stderr}` };
}

test('classifyBlob flags executables by magic, not by extension', () => {
  assert.deepEqual(classifyBlob(syntheticElf()), { binary: true, format: 'ELF' });
  assert.equal(classifyBlob(Buffer.from([0x4d, 0x5a, 0x90, 0x00])).format, 'PE / DOS executable');
  assert.equal(
    classifyBlob(Buffer.from([0xcf, 0xfa, 0xed, 0xfe, 0x0c])).format,
    'Mach-O (MH_CIGAM_64)',
  );
  assert.equal(
    classifyBlob(Buffer.from([0x00, 0x61, 0x73, 0x6d, 0x01])).format,
    'WebAssembly module',
  );
});

test('classifyBlob flags a magic-less blob carrying a NUL byte, and passes text', () => {
  // 'ab' NUL 'c' — no executable magic, but binary by git's own heuristic.
  assert.deepEqual(classifyBlob(Buffer.from([0x61, 0x62, 0x00, 0x63])), {
    binary: true,
    format: null,
  });
  assert.deepEqual(classifyBlob(Buffer.from('package main\n\n// ordinary source\n')), {
    binary: false,
    format: null,
  });
});

test('classifyBlob only sniffs the leading window (the bound is real)', () => {
  const beyond = Buffer.concat([Buffer.alloc(gitBinarySniffBytes, 0x61), Buffer.from([0x00])]);
  assert.equal(classifyBlob(beyond).binary, false, 'a NUL past the sniff window must not be read');
  const inside = Buffer.concat([Buffer.alloc(gitBinarySniffBytes - 1, 0x61), Buffer.from([0x00])]);
  assert.equal(classifyBlob(inside).binary, true, 'a NUL at the last sniffed byte must be read');
});

test('diffAllowlist reports a stray root entry and a rotted allow-list entry', () => {
  const entries = rootEntriesOf(['internal/promql/lower.go', 'docs/engine.md', 'perf-profile']);
  const { unexpected, stale } = diffAllowlist(entries, ['internal', 'docs', 'Justfile']);
  assert.deepEqual(unexpected, ['perf-profile']);
  assert.deepEqual(stale, ['Justfile']);
});

test('diffAllowlist is silent on an exact match', () => {
  const entries = rootEntriesOf(['internal/a.go', 'docs/b.md']);
  assert.deepEqual(diffAllowlist(entries, ['internal', 'docs']), { unexpected: [], stale: [] });
});

test('the CLI passes on a clean fixture tree (both scans)', () => {
  const { dir } = newFixtureRepo();
  for (const check of ['binary', 'root-allowlist']) {
    const { status, out } = runGate(check, dir);
    assert.equal(status, 0, `CHECK=${check} must pass on a clean tree; got:\n${out}`);
    assert.match(out, /::notice::/, `CHECK=${check} must annotate the pass`);
  }
});

test('the CLI FAILS on a committed binary, naming the file and the format', () => {
  const { dir, stage } = newFixtureRepo();
  writeFileSync(join(dir, 'perf-profile'), syntheticElf());
  stage();
  const { status, out } = runGate('binary', dir);
  assert.notEqual(status, 0, `the binary scan must fail; got:\n${out}`);
  assert.match(out, /::error::/);
  assert.match(out, /perf-profile: ELF executable/);
});

test('the CLI FAILS on a stray root file, naming it', () => {
  const { dir, stage } = newFixtureRepo();
  writeFileSync(join(dir, 'release-pr-body.md'), fixtureContent);
  stage();
  const { status, out } = runGate('root-allowlist', dir);
  assert.notEqual(status, 0, `the root scan must fail; got:\n${out}`);
  assert.match(out, /::error::/);
  assert.match(out, /release-pr-body\.md/);
});

test('the CLI rejects an unknown CHECK rather than passing silently', () => {
  const { dir } = newFixtureRepo();
  const { status, out } = runGate('', dir);
  assert.notEqual(status, 0);
  assert.match(out, /unknown CHECK/);
});
