// e2e-seed-stop.test.mjs — node:test guard for e2e-seed-stop.mjs's
// idempotent-teardown contract (cerberus issue #3096): a missing PID file
// is a silent no-op, a present one is signalled and removed, and the
// port-forward supervisor is signalled by PROCESS GROUP (negative PID)
// first, falling back to a plain signal only if the group form is
// rejected. Everything here runs against fake `existsSync`/`readFileSync`/
// `unlinkSync`/`kill` seams — no real process is ever signalled.

import assert from 'node:assert/strict';
import test from 'node:test';

import { stopRollingSeeder, stopPortForwardSupervisor } from './e2e-seed-stop.mjs';

function fakeFs(pidFileContent) {
  const unlinked = [];
  return {
    existsFn: () => pidFileContent !== null,
    readFn: () => `${pidFileContent}\n`,
    unlinkFn: (p) => unlinked.push(p),
    unlinked,
  };
}

test('stopRollingSeeder is a silent no-op when the pid file is missing', () => {
  const killed = [];
  const fs = fakeFs(null);
  stopRollingSeeder({ pidFile: '/tmp/x.pid', killFn: (...a) => killed.push(a), ...fs });
  assert.deepEqual(killed, []);
  assert.deepEqual(fs.unlinked, []);
});

test('stopRollingSeeder SIGTERMs the plain pid and removes the pid file', () => {
  const killed = [];
  const fs = fakeFs('4242');
  stopRollingSeeder({ pidFile: '/tmp/x.pid', killFn: (...a) => killed.push(a), ...fs });
  assert.deepEqual(killed, [[4242, 'SIGTERM']]);
  assert.deepEqual(fs.unlinked, ['/tmp/x.pid']);
});

test('stopRollingSeeder swallows a kill failure (process already gone) without throwing', () => {
  const fs = fakeFs('4242');
  assert.doesNotThrow(() => {
    stopRollingSeeder({
      pidFile: '/tmp/x.pid',
      killFn: () => {
        throw new Error('ESRCH');
      },
      ...fs,
    });
  });
  assert.deepEqual(fs.unlinked, ['/tmp/x.pid']);
});

test('stopPortForwardSupervisor signals the NEGATIVE pid (process group) first', () => {
  const killed = [];
  const fs = fakeFs('9000');
  stopPortForwardSupervisor({ pidFile: '/tmp/pf.pid', killFn: (...a) => killed.push(a), ...fs });
  assert.deepEqual(killed, [[-9000, 'SIGTERM']]);
  assert.deepEqual(fs.unlinked, ['/tmp/pf.pid']);
});

test('stopPortForwardSupervisor falls back to the plain pid when the group signal is rejected', () => {
  const killed = [];
  const fs = fakeFs('9000');
  const killFn = (pid, sig) => {
    killed.push([pid, sig]);
    if (pid < 0) throw new Error('EPERM');
  };
  stopPortForwardSupervisor({ pidFile: '/tmp/pf.pid', killFn, ...fs });
  assert.deepEqual(killed, [[-9000, 'SIGTERM'], [9000, 'SIGTERM']]);
});

test('stopPortForwardSupervisor is a silent no-op when the pid file is missing', () => {
  const killed = [];
  const fs = fakeFs(null);
  stopPortForwardSupervisor({ pidFile: '/tmp/pf.pid', killFn: (...a) => killed.push(a), ...fs });
  assert.deepEqual(killed, []);
  assert.deepEqual(fs.unlinked, []);
});
