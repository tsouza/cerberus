import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import { byteSize, goDurationSeconds, residentBytes } from './mutant-memory-guard.mjs';

const guard = '.github/scripts/mutant-memory-guard.mjs';

// A child that allocates past the ceiling the tests set, retaining every buffer
// so the growth is LIVE — the shape of the mutant this guard exists for
// (`i += size` -> `i = size` pins a scanner index and appends per iteration).
//
// It stops at 4x the ceiling under test rather than running away for real. That
// self-limit is not decoration: this suite runs on a shared CI runner, and a
// child that allocated without bound would take the runner down whenever the
// guard is BROKEN — turning a test failure into the very outage the guard
// exists to prevent, with no assertion to read afterwards. A working guard kills
// it at the 256MiB ceiling long before the self-limit; a broken one lets the
// child stop on its own and leaves the ledger empty, which is what the
// assertions below read.
const runawayChildCeilingBytes = 4 * 256 * 1024 * 1024;
const runawayChild = `
const held = [];
while (held.length * 8 * 1024 * 1024 < ${runawayChildCeilingBytes}) {
  held.push(Buffer.alloc(8 * 1024 * 1024, 1));
}
setTimeout(() => process.exit(0), 30_000);
`;

function workspace(t) {
  const root = mkdtempSync(join(tmpdir(), 'mutant-memory-guard-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return root;
}

function child(root, source) {
  const path = join(root, 'child.cjs');
  writeFileSync(path, source);
  return path;
}

function guardEnv(root, overrides = {}) {
  return {
    ...process.env,
    MUTANT_MEMORY_MAX: '256MiB',
    MUTANT_MEMORY_HOLD: '2s',
    MUTANT_MEMORY_LEDGER: join(root, 'ledger.jsonl'),
    ...overrides,
  };
}

function ledgerLines(root) {
  const path = join(root, 'ledger.jsonl');
  if (!existsSync(path)) return [];
  return readFileSync(path, 'utf8')
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map(JSON.parse);
}

test('byteSize demands an explicit unit and a positive value', () => {
  assert.equal(byteSize('X', '1GiB'), 1024 ** 3);
  assert.equal(byteSize('X', '256MiB'), 256 * 1024 ** 2);
  assert.equal(byteSize('X', '1GB'), 1000 ** 3);
  assert.throws(() => byteSize('X', '1024'), /not a byte size/);
  assert.throws(() => byteSize('X', '1gib'), /not a byte size/);
  assert.throws(() => byteSize('X', '0GiB'), /must be positive/);
});

test('goDurationSeconds parses the bound the runner writes', () => {
  assert.equal(goDurationSeconds('X', '150s'), 150);
  assert.equal(goDurationSeconds('X', '2m30s'), 150);
  assert.throws(() => goDurationSeconds('X', '150'), /not a Go duration/);
  assert.throws(() => goDurationSeconds('X', '0s'), /must be a positive duration/);
});

// A pid that has already exited is a RACE with the exit handler, not a failure.
// Reporting it as 0 rather than throwing is what keeps the sampler from turning
// a finished mutant into a breach.
test('residentBytes reads VmRSS and reports a vanished pid as zero', () => {
  assert.equal(
    residentBytes(1, () => 'Name:\tx\nVmRSS:\t   4096 kB\nThreads:\t1\n'),
    4096 * 1024,
  );
  assert.equal(
    residentBytes(1, () => {
      throw new Error('ENOENT');
    }),
    0,
  );
  assert.equal(residentBytes(1, () => 'Name:\tx\n'), 0);
  assert.ok(residentBytes(process.pid) > 0, 'the guard cannot read its own RSS from /proc');
});

// The passthrough contract. A mutant that stays inside the ceiling must reach
// gremlins with the verdict it earned — the guard is in the path for EVERY
// mutant on every leg, so a wrapper that perturbed an ordinary exit code would
// silently rewrite every leg's efficacy.
for (const code of [0, 1, 2, 3]) {
  test(`an unbreached child's exit ${code} is forwarded verbatim`, (t) => {
    const root = workspace(t);
    const result = spawnSync(
      process.execPath,
      [guard, process.execPath, child(root, `process.stdout.write('hello\\n'); process.exit(${code});`)],
      { cwd: process.cwd(), encoding: 'utf8', env: guardEnv(root) },
    );
    assert.equal(result.status, code, result.stderr);
    assert.match(result.stdout, /hello/, 'the child was not given the guard’s stdio');
    assert.deepEqual(ledgerLines(root), [], 'an unbreached mutant was recorded as a breach');
  });
}

test('the child receives the arguments go test handed the wrapper', (t) => {
  const root = workspace(t);
  const result = spawnSync(
    process.execPath,
    [guard, process.execPath, child(root, 'console.log(JSON.stringify(process.argv.slice(2)));'), '-test.v=true', '-test.count=1'],
    { cwd: process.cwd(), encoding: 'utf8', env: guardEnv(root) },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout.trim()), ['-test.v=true', '-test.count=1']);
});

// THE load-bearing test. A breach must produce, in this order: the child dead,
// the breach on the ledger, and the guard STILL RUNNING — because holding is
// what routes the verdict to gremlins' deadline (TIMED OUT, unadjudicated)
// instead of to an exit code. `go test` collapses every test-binary failure to
// its own exit 1, so any non-zero exit here would be recorded as a KILL: the
// suite credited for a mutant it never adjudicated.
test('a runaway child is killed, recorded, and then HELD rather than exited', async (t) => {
  const root = workspace(t);
  const proc = spawn(process.execPath, [guard, process.execPath, child(root, runawayChild)], {
    cwd: process.cwd(),
    stdio: 'ignore',
    env: guardEnv(root, { MUTANT_MEMORY_HOLD: '3600s' }),
  });
  t.after(() => proc.kill('SIGKILL'));

  const exited = new Promise((resolve) => proc.on('exit', (code, signal) => resolve({ code, signal })));
  const deadline = Date.now() + 60_000;
  while (ledgerLines(root).length === 0 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50));
  }

  const [breach] = ledgerLines(root);
  assert.ok(breach !== undefined, 'the runaway child was never recorded as a breach');
  assert.equal(breach.limit_bytes, 256 * 1024 ** 2);
  assert.ok(
    breach.resident_bytes > breach.limit_bytes,
    `a breach must record the RSS that crossed the line, got ${breach.resident_bytes}`,
  );
  // The sampling interval bounds the overshoot; a guard that only noticed the
  // runaway after it had taken the machine would pass the line above and still
  // be useless.
  assert.ok(
    breach.resident_bytes < 2 * breach.limit_bytes,
    `overshoot past the ceiling must stay small, got ${breach.resident_bytes} over ${breach.limit_bytes}`,
  );

  const raced = await Promise.race([
    exited,
    new Promise((resolve) => setTimeout(() => resolve('still-holding'), 1500)),
  ]);
  assert.equal(raced, 'still-holding', `the guard exited after a breach instead of holding: ${JSON.stringify(raced)}`);
});

// The hold is bounded, and its expiry is the ONE remaining exit — 0, which
// gremlins records as LIVED. A survivor is not a kill: it stays in the
// denominator and credits nobody, so the fallback can only ever lower a leg's
// efficacy. Preferring TIMED OUT and falling back to LIVED is the whole
// ordering; KILLED is not in it.
test('the hold expires into exit 0, never a non-zero exit', async (t) => {
  const root = workspace(t);
  const proc = spawn(process.execPath, [guard, process.execPath, child(root, runawayChild)], {
    cwd: process.cwd(),
    stdio: 'ignore',
    env: guardEnv(root, { MUTANT_MEMORY_HOLD: '2s' }),
  });
  t.after(() => proc.kill('SIGKILL'));
  const { code } = await new Promise((resolve) =>
    proc.on('exit', (code, signal) => resolve({ code, signal })),
  );
  assert.equal(code, 0, 'a breached mutant must never leave this guard with a non-zero status');
  assert.equal(ledgerLines(root).length, 1);
});

for (const [name, env] of [
  ['MUTANT_MEMORY_MAX', { MUTANT_MEMORY_MAX: '' }],
  ['MUTANT_MEMORY_HOLD', { MUTANT_MEMORY_HOLD: '' }],
  ['MUTANT_MEMORY_LEDGER', { MUTANT_MEMORY_LEDGER: '' }],
]) {
  test(`a missing ${name} fails closed rather than running unguarded`, (t) => {
    const root = workspace(t);
    const result = spawnSync(
      process.execPath,
      [guard, process.execPath, child(root, 'process.exit(0);')],
      { cwd: process.cwd(), encoding: 'utf8', env: guardEnv(root, env) },
    );
    assert.equal(result.status, 1);
    assert.match(result.stdout, new RegExp(`::error::${name} is required`));
  });
}

test('a malformed ceiling fails closed rather than defaulting', (t) => {
  const root = workspace(t);
  const result = spawnSync(
    process.execPath,
    [guard, process.execPath, child(root, 'process.exit(0);')],
    { cwd: process.cwd(), encoding: 'utf8', env: guardEnv(root, { MUTANT_MEMORY_MAX: 'lots' }) },
  );
  assert.equal(result.status, 1);
  assert.match(result.stdout, /::error::MUTANT_MEMORY_MAX is not a byte size/);
});
