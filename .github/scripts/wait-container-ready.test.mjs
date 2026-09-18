// wait-container-ready.test.mjs — node:test guard for the container
// readiness wait: the seconds parsing and the fail-closed CLI contract
// (missing inputs, and a container that never answers, which is driven with
// a stub `docker` on PATH so the poll loop and the log-tail diagnosis run
// for real without a daemon).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { DEFAULT_DEADLINE_SECONDS, DEFAULT_POLL_INTERVAL_SECONDS, readSeconds } from './wait-container-ready.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'wait-container-ready.mjs');

test('readSeconds takes the default on an empty value and rejects a non-positive one', () => {
  assert.equal(readSeconds(undefined, DEFAULT_DEADLINE_SECONDS), DEFAULT_DEADLINE_SECONDS);
  assert.equal(readSeconds('', DEFAULT_POLL_INTERVAL_SECONDS), DEFAULT_POLL_INTERVAL_SECONDS);
  assert.equal(readSeconds('7', 1), 7);
  assert.throws(() => readSeconds('0', 1), /positive number of seconds/);
  assert.throws(() => readSeconds('soon', 1), /positive number of seconds/);
});

test('the CLI fails closed on missing inputs', () => {
  const res = spawnSync(process.execPath, [CLI], { encoding: 'utf8', env: { ...process.env, CONTAINER: '', URL: '' } });
  assert.equal(res.status, 1);
  assert.match(res.stdout + res.stderr, /CONTAINER and URL are required/);
});

// stubDocker — a `docker` on PATH whose `exec` fails until a marker file
// exists and whose `logs` prints a recognisable tail.
function stubDocker(dir, { readyAfterCalls }) {
  const counter = join(dir, 'calls');
  const script = `#!/bin/sh
case "$1" in
  exec)
    n=0; [ -f "${counter}" ] && n=$(cat "${counter}")
    n=$((n+1)); echo "$n" > "${counter}"
    [ "$n" -ge ${readyAfterCalls} ] && exit 0
    exit 1 ;;
  logs) echo "stub log tail for $4"; exit 0 ;;
esac
exit 2
`;
  const bin = join(dir, 'docker');
  writeFileSync(bin, script);
  chmodSync(bin, 0o755);
  return { PATH: `${dir}:${process.env.PATH}` };
}

test('the CLI polls until the in-container probe answers', () => {
  const dir = mkdtempSync(join(tmpdir(), 'wait-ready-'));
  try {
    const env = stubDocker(dir, { readyAfterCalls: 3 });
    const res = spawnSync(process.execPath, [CLI], {
      encoding: 'utf8',
      env: { ...process.env, ...env, CONTAINER: 'ch', URL: 'http://localhost:8123/ping', DEADLINE_SECONDS: '10', POLL_INTERVAL_SECONDS: '0.05' },
    });
    assert.equal(res.status, 0, res.stderr);
    assert.match(res.stdout, /ch ready after \d+s/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the CLI fails on the deadline and prints the container log tail', () => {
  const dir = mkdtempSync(join(tmpdir(), 'wait-ready-'));
  try {
    const env = stubDocker(dir, { readyAfterCalls: 1_000_000 });
    const res = spawnSync(process.execPath, [CLI], {
      encoding: 'utf8',
      env: { ...process.env, ...env, CONTAINER: 'ch', URL: 'http://localhost:8123/ping', DEADLINE_SECONDS: '0.2', POLL_INTERVAL_SECONDS: '0.05' },
    });
    assert.equal(res.status, 1);
    assert.match(res.stdout, /stub log tail for ch/);
    assert.match(res.stdout + res.stderr, /ch did not answer http:\/\/localhost:8123\/ping within 0\.2s/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
