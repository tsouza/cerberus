import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

// The runner's process boundary is intentional: it must prove the exact env
// contract and two-invocation fallback, not a helper that bypasses main().
import { spawnSync } from 'node:child_process';

// fixture builds a self-contained leg: a fake `gremlins` that records its argv
// and writes the report totals it is told to, a fake `go` that records its argv
// and burns `probeSeconds` of wall clock, and a scope directory holding the
// production source the budget probe edits.
//
// `go` is faked rather than skipped because the probe is the thing under test:
// a probe that silently measured nothing would fall back to the floor and every
// budget assertion below would still pass against a script that never probed.
function fixture(t, totals, { probeSeconds = 0, goExits = 0, goMissing = false } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'mutation-run-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const calls = join(root, 'calls.jsonl');
  const envCalls = join(root, 'env-calls.jsonl');
  const goCalls = join(root, 'go-calls.jsonl');
  const bin = join(root, 'bin');
  const scope = join(root, 'scope');
  mkdirSync(bin);
  mkdirSync(scope);
  // Two files, so "largest non-test .go, ties broken by name" has something to
  // choose between, and a _test.go the probe must not touch.
  writeFileSync(join(scope, 'a_small.go'), 'package scope\n');
  writeFileSync(join(scope, 'b_large.go'), `package scope\n\n// ${'x'.repeat(200)}\n`);
  writeFileSync(join(scope, 'z_test.go'), `package scope\n\n// ${'y'.repeat(4000)}\n`);
  writeFileSync(calls, '');
  writeFileSync(envCalls, '');
  writeFileSync(goCalls, '');
  writeFileSync(
    join(bin, 'gremlins'),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(calls)}, JSON.stringify(args) + '\\n');
// The env gremlins actually receives is half the contract: the memory guard is
// wired ONLY through it, so an argv-only assertion would pass over a leg whose
// mutants run unbounded.
fs.appendFileSync(${JSON.stringify(envCalls)}, JSON.stringify({
  GOFLAGS: process.env.GOFLAGS,
  MUTANT_MEMORY_MAX: process.env.MUTANT_MEMORY_MAX,
  MUTANT_MEMORY_HOLD: process.env.MUTANT_MEMORY_HOLD,
  MUTANT_MEMORY_LEDGER: process.env.MUTANT_MEMORY_LEDGER,
}) + '\\n');
const report = args[args.indexOf('--output') + 1];
const totals = ${JSON.stringify(totals)};
const call = fs.readFileSync(${JSON.stringify(calls)}, 'utf8').trim().split('\\n').length - 1;
fs.writeFileSync(report, JSON.stringify({ mutants_total: totals[call] }));
`,
    { mode: 0o755 },
  );
  if (!goMissing) {
    writeFileSync(
      join(bin, 'go'),
      `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
// Record what the tree looked like WHILE the probe was running, so the test can
// prove the probe actually edited the source rather than timing a cache hit.
const seen = {};
for (const f of fs.readdirSync(${JSON.stringify(scope)})) {
  seen[f] = fs.readFileSync(require('node:path').join(${JSON.stringify(scope)}, f), 'utf8');
}
fs.appendFileSync(${JSON.stringify(goCalls)}, JSON.stringify({ args, seen }) + '\\n');
Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ${probeSeconds} * 1000);
process.exit(${goExits});
`,
      { mode: 0o755 },
    );
  }
  return { root, bin, scope, calls, envCalls, goCalls };
}

function run(f, env = {}) {
  return spawnSync(process.execPath, ['.github/scripts/mutation-run.mjs'], {
    cwd: process.cwd(),
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: f.bin,
      SCOPE: f.scope,
      REPORT: join(f.root, 'gremlins.json'),
      MUTANT_TIMEOUT_MIN: '15s',
      MUTANT_TIMEOUT_MAX: '120s',
      WORKERS: '1',
      EXCLUDE_FILES: '',
      ...env,
    },
  });
}

function invocations(f) {
  const raw = readFileSync(f.calls, 'utf8').trim();
  return raw === '' ? [] : raw.split('\n').map(JSON.parse);
}

function budgetOf(args) {
  return args[args.indexOf('--timeout-max') + 1];
}

function environments(f) {
  const raw = readFileSync(f.envCalls, 'utf8').trim();
  return raw === '' ? [] : raw.split('\n').map(JSON.parse);
}

test('changed-line execution stays incremental when it executes mutants', (t) => {
  const f = fixture(t, [3]);
  const result = run(f, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 0, result.stderr);
  const calls = invocations(f);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].slice(-3), ['--diff', 'a'.repeat(40), f.scope]);
});

test('zero changed-line mutants trigger one full fallback', (t) => {
  const f = fixture(t, [0, 7]);
  const result = run(f, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 0, result.stderr);
  const calls = invocations(f);
  assert.equal(calls.length, 2);
  assert.equal(calls[0].includes('--diff'), true);
  assert.equal(calls[1].includes('--diff'), false);
});

test('zero mutants after full fallback fail closed', (t) => {
  const f = fixture(t, [0, 0]);
  const result = run(f, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 1);
  assert.match(`${result.stdout}\n${result.stderr}`, /executed zero mutants after full fallback/);
});

// The probe has to force the recompile a mutant forces. Go's build cache is
// keyed on CONTENT, so timing an unedited package times a cache hit and
// measures nothing — which is the mismeasurement this whole change exists to
// remove. Proven from inside the probe's own `go` child.
test('the probe edits the largest production file, and restores it exactly', (t) => {
  const f = fixture(t, [4]);
  const before = readFileSync(join(f.scope, 'b_large.go'), 'utf8');
  assert.equal(run(f, { DIFF_REF: '' }).status, 0);

  const [probe] = readFileSync(f.goCalls, 'utf8').trim().split('\n').map(JSON.parse);
  assert.deepEqual(probe.args, ['test', '-count=1', '-failfast', '-timeout', '120s', f.scope]);
  assert.notEqual(probe.seen['b_large.go'], before, 'the probe timed an unedited package');
  assert.ok(probe.seen['b_large.go'].startsWith(before), 'the probe rewrote the file wholesale');
  assert.equal(probe.seen['a_small.go'], 'package scope\n', 'the probe edited more than one file');
  assert.match(probe.seen['z_test.go'], /^package scope\n\n\/\/ y+\n$/, 'the probe edited a test file');

  // Restored before gremlins ever sees the tree.
  assert.equal(readFileSync(join(f.scope, 'b_large.go'), 'utf8'), before);
});

// The measurement is the point: a slower cycle must buy a bigger budget.
test('the budget scales with the measured cycle and the concurrent-mutant count', (t) => {
  const slow = fixture(t, [4], { probeSeconds: 2 });
  assert.equal(run(slow, { DIFF_REF: '', WORKERS: '1', MUTANT_TIMEOUT_MIN: '1s' }).status, 0);
  const serial = Number(budgetOf(invocations(slow)[0]).replace('s', ''));
  assert.ok(serial >= 4, `a 2s cycle at 1 worker must buy at least 2x2s, got ${serial}s`);

  const fanned = fixture(t, [4], { probeSeconds: 2 });
  assert.equal(run(fanned, { DIFF_REF: '', WORKERS: '4', MUTANT_TIMEOUT_MIN: '1s' }).status, 0);
  const parallel = Number(budgetOf(invocations(fanned)[0]).replace('s', ''));
  assert.ok(
    parallel >= serial * 3,
    `4 concurrent mutants must buy ~4x the budget 1 does, got ${parallel}s vs ${serial}s`,
  );
});

// The starved case, in the direction that matters: MIN is a floor the
// measurement may raise but never lower, so a leg whose cycle measures as
// nearly free still gets the budget the lane ran on before it was measured.
test('a fast cycle floors at MUTANT_TIMEOUT_MIN rather than shrinking the budget', (t) => {
  const f = fixture(t, [4], { probeSeconds: 0 });
  assert.equal(run(f, { DIFF_REF: '' }).status, 0);
  assert.equal(budgetOf(invocations(f)[0]), '15s');
});

test('a cycle larger than the ceiling clamps to MUTANT_TIMEOUT_MAX', (t) => {
  const f = fixture(t, [4], { probeSeconds: 1 });
  const env = { DIFF_REF: '', WORKERS: '2', MUTANT_TIMEOUT_MIN: '1s', MUTANT_TIMEOUT_MAX: '3s' };
  assert.equal(run(f, env).status, 0);
  assert.equal(budgetOf(invocations(f)[0]), '3s');
});

// Degrading safely is a requirement, not a nicety: an unmeasurable probe must
// leave the lane exactly where it was, never at some accidental smaller number.
test('an unmeasurable cycle falls back to the floor instead of guessing', (t) => {
  const f = fixture(t, [4], { goMissing: true });
  const result = run(f, { DIFF_REF: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(budgetOf(invocations(f)[0]), '15s');
  assert.match(`${result.stdout}\n${result.stderr}`, /cannot measure the per-mutant cycle/);
});

test('a scope with no production source falls back to the floor', (t) => {
  const f = fixture(t, [4]);
  rmSync(join(f.scope, 'a_small.go'));
  rmSync(join(f.scope, 'b_large.go'));
  const result = run(f, { DIFF_REF: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(budgetOf(invocations(f)[0]), '15s');
  assert.equal(readFileSync(f.goCalls, 'utf8'), '', 'nothing to edit, so nothing to time');
});

// The per-mutant budget must be the value this script derived, not an accident
// of build-cache warmth. gremlins computes `min(coefficient * max(coverage
// _elapsed, 1s), timeout-max)`, so unless the coefficient alone carries the
// whole budget across gremlins' own 1s floor, the hot-cache fallback invocation
// silently gets a smaller budget than the cold-cache first one (#2692).
test('both invocations pin the per-mutant budget to the derived value', (t) => {
  const f = fixture(t, [0, 7]);
  const result = run(f, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 0, result.stderr);
  const calls = invocations(f);
  assert.equal(calls.length, 2);
  for (const args of calls) {
    assert.equal(budgetOf(args), '15s');
    const at = args.indexOf('--timeout-coefficient');
    assert.notEqual(at, -1, `--timeout-coefficient missing from ${JSON.stringify(args)}`);
    // gremlins clamps its measured coverage time up to a 1s floor, so
    // `coefficient * 1s` is the smallest budget the coefficient can yield; it
    // must already reach the derived budget for that to be the sole budget.
    assert.ok(Number(args[at + 1]) * 1 >= 15, `coefficient ${args[at + 1]} is below the budget`);
  }
});

test('the probe is timed once and reused across the fallback invocation', (t) => {
  const f = fixture(t, [0, 7]);
  assert.equal(run(f, { DIFF_REF: 'a'.repeat(40) }).status, 0);
  assert.equal(
    readFileSync(f.goCalls, 'utf8').trim().split('\n').length,
    1,
    'a second measurement would be taken against the cache the first invocation warmed',
  );
});

test('the derived coefficient rounds a compound bound up, and rejects a bad one', (t) => {
  const f = fixture(t, [4]);
  let result = run(f, { DIFF_REF: '', MUTANT_TIMEOUT_MIN: '1m30s' });
  assert.equal(result.status, 0, result.stderr);
  const args = invocations(f)[0];
  assert.equal(args[args.indexOf('--timeout-coefficient') + 1], '90');

  const bad = fixture(t, [4]);
  result = run(bad, { DIFF_REF: '', MUTANT_TIMEOUT_MAX: '15 seconds' });
  assert.equal(result.status, 1);
  assert.equal(readFileSync(bad.calls, 'utf8'), '');
  assert.match(`${result.stdout}\n${result.stderr}`, /MUTANT_TIMEOUT_MAX is not a Go duration/);
});

test('a ceiling below the floor is rejected rather than silently inverted', (t) => {
  const f = fixture(t, [4]);
  const result = run(f, { DIFF_REF: '', MUTANT_TIMEOUT_MIN: '30s', MUTANT_TIMEOUT_MAX: '15s' });
  assert.equal(result.status, 1);
  assert.equal(readFileSync(f.calls, 'utf8'), '');
  assert.match(`${result.stdout}\n${result.stderr}`, /MUTANT_TIMEOUT_MAX is below MUTANT_TIMEOUT_MIN/);
});

test('invalid diff refs and stale reports fail before gremlins runs', (t) => {
  const f = fixture(t, [3]);
  let result = run(f, { DIFF_REF: 'main' });
  assert.equal(result.status, 1);
  assert.equal(readFileSync(f.calls, 'utf8'), '');

  writeFileSync(join(f.root, 'gremlins.json'), '{}');
  result = run(f, { DIFF_REF: '' });
  assert.equal(result.status, 1);
  assert.match(`${result.stdout}\n${result.stderr}`, /refusing stale gremlins report/);
});

// The memory bound reaches gremlins ONLY through the environment, and only as a
// `go test -exec` hook. A leg that ran without it would look exactly like a leg
// that ran with it — same argv, same report — right up to the run where a
// runaway mutant takes the runner down with no verdict at all (#2919). So the
// env contract is pinned as tightly as the argv one.
test('every gremlins invocation runs its mutants under the memory guard', (t) => {
  const f = fixture(t, [0, 7]);
  assert.equal(run(f, { DIFF_REF: 'a'.repeat(40) }).status, 0);
  const envs = environments(f);
  assert.equal(envs.length, 2, 'the fallback invocation must be guarded too');
  for (const env of envs) {
    assert.match(
      env.GOFLAGS,
      /(^| )-exec=\/.*\/\.github\/scripts\/mutant-memory-guard\.mjs$/,
      `GOFLAGS must put the guard in front of every test binary, got ${env.GOFLAGS}`,
    );
    assert.equal(env.MUTANT_MEMORY_MAX, '1GiB');
    assert.ok(
      env.MUTANT_MEMORY_LEDGER.endsWith('gremlins.json.memory-breaches.jsonl'),
      `the ledger must sit beside the report, got ${env.MUTANT_MEMORY_LEDGER}`,
    );
  }
  assert.equal(envs[0].MUTANT_MEMORY_LEDGER, envs[1].MUTANT_MEMORY_LEDGER, 'a leg keeps one ledger');
});

// The hold has to outlast the deadline it defers to. If it expired FIRST the
// guard would exit 0 while gremlins was still waiting, and a memory-runaway
// mutant would be booked LIVED-by-accident instead of TIMED OUT-on-purpose.
test('the guard holds strictly longer than the per-mutant budget', (t) => {
  const f = fixture(t, [4], { probeSeconds: 0 });
  assert.equal(run(f, { DIFF_REF: '' }).status, 0);
  const budget = Number(budgetOf(invocations(f)[0]).replace('s', ''));
  const hold = Number(environments(f)[0].MUTANT_MEMORY_HOLD.replace('s', ''));
  assert.ok(hold > budget, `the hold (${hold}s) must outlast the budget (${budget}s)`);
});

// An existing GOFLAGS belongs to the toolchain setup, not to us. Replacing it
// would silently drop whatever the composite setup-go action put there.
test('an existing GOFLAGS is appended to, not replaced', (t) => {
  const f = fixture(t, [4]);
  assert.equal(run(f, { DIFF_REF: '', GOFLAGS: '-mod=mod' }).status, 0);
  assert.match(environments(f)[0].GOFLAGS, /^-mod=mod -exec=\//);
});

// A breach is the bound WORKING, so it is reported rather than fatal — but it
// must be reported, because `go test` discards a passing binary's output and
// the guard's own annotation dies with it.
test('memory breaches recorded by the guard are surfaced by the runner', (t) => {
  const f = fixture(t, [4]);
  const ledger = join(f.root, 'gremlins.json.memory-breaches.jsonl');
  writeFileSync(
    join(f.bin, 'gremlins'),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(f.calls)}, JSON.stringify(args) + '\\n');
fs.appendFileSync(process.env.MUTANT_MEMORY_LEDGER,
  JSON.stringify({ binary: '/tmp/x.test', resident_bytes: 1181116006, limit_bytes: 1073741824 }) + '\\n');
fs.writeFileSync(args[args.indexOf('--output') + 1], JSON.stringify({ mutants_total: 4 }));
`,
    { mode: 0o755 },
  );
  const result = run(f, { DIFF_REF: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /::notice::1 mutant\(s\) hit the 1GiB per-mutant memory ceiling/);
  assert.match(result.stdout, /recorded as TIMED OUT/);
  assert.ok(existsSync(ledger), 'the ledger the runner read must be the one the guard wrote');
});

// A stale ledger from a previous leg in the same workspace would report
// breaches this run never had.
test('a stale breach ledger is discarded before the run', (t) => {
  const f = fixture(t, [4]);
  const ledger = join(f.root, 'gremlins.json.memory-breaches.jsonl');
  writeFileSync(ledger, JSON.stringify({ binary: '/stale', resident_bytes: 1, limit_bytes: 1 }) + '\n');
  const result = run(f, { DIFF_REF: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.doesNotMatch(result.stdout, /hit the 1GiB per-mutant memory ceiling/);
});
