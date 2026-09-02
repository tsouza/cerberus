import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import { byteSize, goDurationSeconds, residentBytes } from './mutant-memory-guard.mjs';

const guard = '.github/scripts/mutant-memory-guard.mjs';

// The ceiling every test in this file sets, in the two forms it is needed in.
// One source, because a self-limit or an assertion that drifted from the
// ceiling the guard was actually given would be silently comparing two
// different runs.
const ceilingMiB = 256;
const ceilingSpec = `${ceilingMiB}MiB`;
const ceilingBytes = ceilingMiB * 1024 ** 2;

// The runaway child grows at a rate THIS TEST SETS: one allocationStepBytes
// buffer per allocationStepIntervalMs tick, every one retained so the growth is
// LIVE — the shape of the mutant this guard exists for (`i += size` ->
// `i = size` pins a scanner index and appends per iteration).
//
// The rate is set here rather than left to a tight allocation loop because the
// overshoot assertion below reads back as a LATENCY, and a tight loop runs at
// whatever rate the machine happens to have left over. That made the assertion
// measure runner load as much as it measured the guard: a full 30-leg mutation
// matrix on the same runner was enough to fail it with no defect in the guard
// (#2955). A timer-driven child stretches under load in the same direction as
// the guard's own timer, so the ratio between them — which is what the bound is
// about — survives a busy machine.
const allocationStepBytes = 8 * 1024 ** 2;
const allocationStepIntervalMs = 50;

// It stops at 4x the ceiling under test rather than running away for real. That
// self-limit is not decoration: this suite runs on a shared CI runner, and a
// child that allocated without bound would take the runner down whenever the
// guard is BROKEN — turning a test failure into the very outage the guard
// exists to prevent, with no assertion to read afterwards. A working guard kills
// it at the ceiling long before the self-limit; a broken one lets the child stop
// on its own and leaves the ledger empty, which is what the assertions below
// read.
const runawayChildCeilingBytes = 4 * ceilingBytes;
const runawayChild = `
const held = [];
const step = () => {
  held.push(Buffer.alloc(${allocationStepBytes}, 1));
  if (held.length * ${allocationStepBytes} >= ${runawayChildCeilingBytes}) {
    setTimeout(() => process.exit(0), 30_000);
    return;
  }
  setTimeout(step, ${allocationStepIntervalMs});
};
step();
`;

// maxNoticeLatencyMs is the property the overshoot assertion pins: the guard
// must kill within this long of the child's RSS crossing the ceiling. The guard
// samples /proc every 50ms, so this grants it two sampling intervals of
// lateness.
//
// Two intervals is headroom measured rather than guessed. Against this child a
// healthy guard notices in ~1ms, on an idle machine and under 4x CPU
// oversubscription alike: child and guard are both timer-driven, so load
// stretches them together and the GAP between them — the only thing this bound
// is about — does not move. Two orders of magnitude of headroom, and a guard
// whose sampling interval is widened to 250ms still records ~160ms of lateness
// and fails.
//
// It is deliberately NOT derived from the guard's own sampling interval. A bound
// that moved with the number it is auditing could not fail when that number is
// widened, which is the single defect this assertion exists to catch.
const maxNoticeLatencyMs = 100;
const maxOvershootBytes = (maxNoticeLatencyMs / allocationStepIntervalMs) * allocationStepBytes;

// How long the runaway subtests wait for the guard to record a breach. At the
// rate set above the child takes ~1.7s to cross the ceiling, so this is generous
// by more than an order of magnitude: it detects a wedged guard, and a slow
// runner cannot walk it into a failure the way a bound on the MEASUREMENT can.
const breachRecordedTimeoutMs = 60_000;

// How long the guard must still be running after a breach for the hold to be
// real. Load can only ever make it MORE likely to still be there, so this bound
// cannot fail on a busy runner; only a guard that exited instead of holding
// trips it.
const holdObservedMs = 1500;

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
    MUTANT_MEMORY_MAX: ceilingSpec,
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
  const deadline = Date.now() + breachRecordedTimeoutMs;
  while (ledgerLines(root).length === 0 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50));
  }

  const [breach] = ledgerLines(root);
  assert.ok(breach !== undefined, 'the runaway child was never recorded as a breach');
  assert.equal(breach.limit_bytes, ceilingBytes);
  assert.ok(
    breach.resident_bytes > breach.limit_bytes,
    `a breach must record the RSS that crossed the line, got ${breach.resident_bytes}`,
  );
  // A guard that only noticed the runaway after it had taken the machine would
  // pass the line above and still be useless, so the recorded overshoot is read
  // back as the guard's NOTICE LATENCY. That conversion is exact because the
  // child's rate is set by this test: every allocationStepIntervalMs of lateness
  // costs exactly allocationStepBytes, so `resident_bytes - limit_bytes` over
  // that rate IS the milliseconds the guard took, and the bound is a statement
  // about the guard rather than about how much CPU the runner had spare.
  const overshootBytes = breach.resident_bytes - breach.limit_bytes;
  assert.ok(
    overshootBytes <= maxOvershootBytes,
    `the guard must notice a crossing within ${maxNoticeLatencyMs}ms, which at this child's ` +
      `${allocationStepBytes}B/${allocationStepIntervalMs}ms rate is ${maxOvershootBytes} bytes of ` +
      `overshoot; it recorded ${overshootBytes} bytes past the ${breach.limit_bytes} ceiling ` +
      `(~${Math.round((overshootBytes / allocationStepBytes) * allocationStepIntervalMs)}ms late)`,
  );

  const raced = await Promise.race([
    exited,
    new Promise((resolve) => setTimeout(() => resolve('still-holding'), holdObservedMs)),
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
