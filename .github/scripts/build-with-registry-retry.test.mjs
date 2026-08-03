// build-with-registry-retry.test.mjs — node:test guard for runTeed's argv
// handling.
//
// The module has no exported entrypoint (it runs its retry loop at module
// top level off process.argv, by design — see its own header), so these
// tests invoke the real script as a subprocess rather than importing its
// internals. That is also the more meaningful test for a security-shaped
// fix: it exercises exactly what CI actually runs, not a refactored stand-in.
//
// GO_IMAGE is set on every invocation so buildBaseImageArgs's "resolve the
// mirror" step (a real `docker buildx imagetools inspect` probe) short-
// circuits — these tests assert argv-safety, not mirror resolution, and
// should not need Docker or network access to run.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const SCRIPT = fileURLToPath(new URL('./build-with-registry-retry.mjs', import.meta.url));

function run(argv) {
  return spawnSync('node', [SCRIPT, ...argv], {
    encoding: 'utf8',
    env: { ...process.env, GO_IMAGE: 'golang:1.26' },
  });
}

test('a normal command runs, streams, and exits with its own status', () => {
  const res = run(['echo', 'hello world']);
  assert.equal(res.status, 0);
  assert.match(res.stdout, /hello world/);
});

// The load-bearing case: an argv element shaped like a shell-metacharacter
// payload must reach the command as a single literal argument, never as
// shell syntax runTeed's own script text could be tricked into evaluating.
// This is what the switch from a hand-quoted script string to bash's "$@"
// positional-parameter form (this fix) makes true by construction, not by
// the correctness of an escaping function.
test('an argv element containing shell metacharacters is passed through literally, not interpreted', () => {
  const payload = "it's a $(touch /tmp/should-not-exist-$$) `echo pwned` && echo also-pwned; echo semicolon";
  const res = run(['echo', payload]);
  assert.equal(res.status, 0);
  // The whole payload comes back verbatim on one line — nothing in it ran.
  assert.match(res.stdout, /it's a \$\(touch \/tmp\/should-not-exist-\$\$\) `echo pwned` && echo also-pwned; echo semicolon/);
  assert.doesNotMatch(res.stdout, /^pwned$/m);
  assert.doesNotMatch(res.stdout, /^also-pwned$/m);
});

test('a failing command exits with ITS status, not a masking failure from the tee pipeline', () => {
  const res = run(['bash', '-c', 'echo partial-output; exit 42']);
  assert.equal(res.status, 42);
  assert.match(res.stdout, /partial-output/);
});

test('a genuine failure with no registry/network signal in its output fails on the first attempt', () => {
  // isTransientRegistryFailure(output) is false for this output, so the
  // module must not retry — a real bug must surface on attempt 1/N, not be
  // hidden behind a wasted retry budget.
  const res = run(['bash', '-c', 'echo not a registry problem; exit 3']);
  assert.equal(res.status, 3);
  // error() (lib/gh.mjs) writes the ::error:: workflow-command line to stdout.
  assert.match(res.stdout, /failed with status 3.*no registry or network fault/s);
});
