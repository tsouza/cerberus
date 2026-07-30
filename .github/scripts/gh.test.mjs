// Unit tests for lib/gh.mjs — the module every other script in here runs its
// subprocesses through.
//
// The decision pinned here is capture()'s option contract. capture() does not
// spread its `opts` into spawnSync; it names the keys it forwards. That is the
// right shape — it keeps the surface small — but it made an unsupported key a
// SILENT no-op, and the failure that produced was invisible: a caller passing
// `{ shell: true }` got its whole command string handed to spawnSync as argv[0],
// which never resolves, so the gate reading that status saw a plain non-zero
// exit and concluded the command had answered "no". The brew formula-to-cask
// migration check ran that way and carried zero signal until an audit read it.
// Rejecting unknown keys turns a permanently-wrong gate into a first-run crash.

import assert from 'node:assert/strict';
import test from 'node:test';

import { capture } from './lib/gh.mjs';

test('capture forwards the options it documents', () => {
  const res = capture('sh', ['-c', 'printf "%s" "$MARKER"'], {
    env: { MARKER: 'forwarded' },
    timeout: 30_000,
  });
  assert.equal(res.status, 0);
  assert.equal(res.stdout, 'forwarded');
});

test('capture rejects an option it would otherwise drop', () => {
  // `shell` is the one that actually bit us; the assertion is about the class,
  // so an arbitrary key has to be rejected the same way.
  for (const opts of [{ shell: true }, { stdio: 'inherit' }, { cwd: '.', shell: false }]) {
    assert.throws(
      () => capture('true', [], opts),
      (err) => err instanceof TypeError && /not forwarded to spawnSync/.test(err.message),
      `capture() must reject ${JSON.stringify(opts)} rather than silently ignore it`,
    );
  }
});

test('capture still accepts an empty options object', () => {
  // The guard must not turn the common no-options call into a throw.
  const res = capture('sh', ['-c', 'exit 3']);
  assert.equal(res.status, 3);
});

test('capture reports an unspawnable command as a status, not a throw', () => {
  // The whole point of capture() is that the caller decides what failure means,
  // so a missing binary must come back as data. This is also what the `shell`
  // bug looked like from the outside — which is why the guard above exists.
  const res = capture('cerberus-no-such-binary-4f3a', []);
  assert.notEqual(res.status, 0);
  assert.match(res.stderr, /ENOENT|not found/i);
});
