// Tests for the Go module-fetch retry wrapper.
//
// Every assertion here is about the ATTEMPT COUNT, not merely the exit code,
// and that is the whole point. "Retry does not mask a real failure" is a claim
// about how many times the command ran: a wrapper that dutifully retried a
// compile error five times and then reported it would be indistinguishable, on
// the exit status alone, from one that refused to retry it at all. Asserting
// the count is what turns the claim into a test.
//
// The positive fixtures are the VERBATIM stderr captured from the runs that
// motivated this wrapper — 33080257221 (coverage-plan), 33084966918 (mutation:
// `install gremlins` and two `gremlins unleash` legs) and 33093118010 (chdb
// `probe`). They are quoted rather than paraphrased so a future edit to the
// classification in lib/registry.mjs that stopped matching the real text would
// fail here rather than silently stop retrying the only failure this exists for.

import assert from 'node:assert/strict';
import test from 'node:test';

import { isTransientRegistryFailure } from './lib/registry.mjs';
import {
  commandFrom,
  goFetchBackoffStepSeconds,
  goFetchNeverRetryable,
  isFetchOnly,
  runWithRetry,
  warmCommand,
} from './go-module-fetch.mjs';

// ---------------------------------------------------------------------------
// The four failures this wrapper was built for, exactly as CI printed them.
// ---------------------------------------------------------------------------

const gremlinsInstallFailure =
  'go: github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-timeout-max-consume: verifying module: ' +
  'github.com/tsouza/gremlins@v0.6.0-cerberus-timeout-max-consume: reading ' +
  'https://sum.golang.org/tile/8/0/x227/070: stream error: stream ID 3; INTERNAL_ERROR; received from peer\n';

const autoexportModFailure =
  'go: go.opentelemetry.io/contrib/exporters/autoexport@v0.70.0: read ' +
  '"https://proxy.golang.org/go.opentelemetry.io/contrib/exporters/autoexport/@v/v0.70.0.mod": ' +
  'stream error: stream ID 523; INTERNAL_ERROR; received from peer\n';

const stdoutlogZipFailure =
  'go: go.opentelemetry.io/otel/exporters/stdout/stdoutlog@v0.21.0: read ' +
  '"https://proxy.golang.org/go.opentelemetry.io/otel/exporters/stdout/stdoutlog/@v/v0.21.0.zip": ' +
  'stream error: stream ID 2319; INTERNAL_ERROR; received from peer\n';

const sqlbuilderZipFailure =
  '../../../go/pkg/mod/github.com/chdb-io/chdb-go@v1.12.0/chdb/driver/driver.go:15:2: ' +
  'github.com/huandu/go-sqlbuilder@v1.27.3: read ' +
  '"https://proxy.golang.org/github.com/huandu/go-sqlbuilder/@v/v1.27.3.zip": ' +
  'stream error: stream ID 115; INTERNAL_ERROR; received from peer\n';

const transportFailures = {
  'gremlins install (run 33084966918)': gremlinsInstallFailure,
  'autoexport .mod (run 33084966918)': autoexportModFailure,
  'stdoutlog .zip (run 33084966918)': stdoutlogZipFailure,
  'go-sqlbuilder .zip (run 33093118010)': sqlbuilderZipFailure,
};

// ---------------------------------------------------------------------------
// The failures that must NOT be retried.
// ---------------------------------------------------------------------------

const notFound =
  'go: github.com/tsouza/nonexistent@v1.2.3: reading ' +
  'https://proxy.golang.org/github.com/tsouza/nonexistent/@v/v1.2.3.info: 404 Not Found\n';

const gone =
  'go: github.com/tsouza/withdrawn@v1.2.3: reading ' +
  'https://proxy.golang.org/github.com/tsouza/withdrawn/@v/v1.2.3.info: 410 Gone\n';

const unknownRevision =
  'go: github.com/tsouza/cerberus@v9.9.9: invalid version: unknown revision v9.9.9\n';

const noMatchingVersions = 'go: github.com/tsouza/cerberus@v9: no matching versions for query "v9"\n';

const checksumMismatch =
  'go: downloading github.com/huandu/go-sqlbuilder v1.27.3\n' +
  'verifying github.com/huandu/go-sqlbuilder@v1.27.3: checksum mismatch\n' +
  '\tdownloaded: h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n' +
  '\tgo.sum:     h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n' +
  'SECURITY ERROR\n' +
  'This download does NOT match the one reported by the checksum server.\n';

const missingGoSumEntry =
  'go: github.com/tsouza/cerberus/internal/promql: no required module provides package ' +
  'github.com/huandu/go-sqlbuilder; missing go.sum entry for module providing package ' +
  'github.com/huandu/go-sqlbuilder\n';

const goCompileError = './internal/chsql/builder.go:31:2: undefined: notAFunction\n';

const neverRetried = {
  '404 Not Found': notFound,
  '410 Gone': gone,
  'unknown revision': unknownRevision,
  'no matching versions': noMatchingVersions,
  'checksum mismatch / SECURITY ERROR': checksumMismatch,
  'missing go.sum entry': missingGoSumEntry,
};

// ---------------------------------------------------------------------------
// A recording spawn, so every assertion below is about how many times the
// command actually ran.
// ---------------------------------------------------------------------------

function recorder({ stderr = '', failFor = Infinity } = {}) {
  const calls = [];
  const spawn = (program, args) => {
    calls.push([program, ...args]);
    if (calls.length > failFor) return { status: 0, stdout: 'ok\n', stderr: '' };
    return { status: 1, stdout: '', stderr };
  };
  return { calls, spawn };
}

const silent = () => {};

function run(argv, options) {
  const { calls, spawn } = recorder(options);
  const result = runWithRetry({ argv, spawn, sleep: silent, onLog: silent });
  return { ...result, calls };
}

// ---------------------------------------------------------------------------
// Layer (b): a transport fault is retried, and the retry is what rescues it.
// ---------------------------------------------------------------------------

for (const [label, stderr] of Object.entries(transportFailures)) {
  test(`a module-proxy transport fault retries and then succeeds — ${label}`, () => {
    // lib/registry.mjs is the classifier; if its pattern list ever stops
    // matching the real text, this is the assertion that says so directly
    // rather than through a count that happens to be 1 for two reasons.
    assert.equal(isTransientRegistryFailure(stderr), true, 'the shared classifier no longer matches this failure');

    const result = run(['go', 'mod', 'download'], { stderr, failFor: 2 });
    assert.equal(result.status, 0);
    assert.equal(result.verdict, 'succeeded');
    assert.equal(result.attempts, 3, 'the third attempt is the one that succeeded');
    assert.equal(result.calls.length, 3);
    assert.deepEqual(result.calls[0], ['go', 'mod', 'download']);
  });

  test(`a persistent transport fault spends the whole budget and still fails — ${label}`, () => {
    const result = run(['go', 'mod', 'download'], { stderr });
    assert.notEqual(result.status, 0);
    assert.equal(result.verdict, 'exhausted');
    assert.equal(result.attempts, 5, 'the shared attempt budget is five');
    assert.equal(result.calls.length, 5);
  });
}

// ---------------------------------------------------------------------------
// Layer (c): the never-retry set, and layer (b)'s floor for anything unmatched.
// ---------------------------------------------------------------------------

for (const [label, stderr] of Object.entries(neverRetried)) {
  test(`${label} exits non-zero after exactly one attempt`, () => {
    assert.equal(goFetchNeverRetryable(stderr), true, `${label} is not in the never-retry set`);

    const result = run(['go', 'mod', 'download'], { stderr });
    assert.notEqual(result.status, 0);
    assert.equal(result.verdict, 'never-retryable');
    assert.equal(result.attempts, 1, `${label} was retried — a retry cannot fix it and must not be spent`);
    assert.equal(result.calls.length, 1);
  });
}

test('a Go compile error exits non-zero after exactly one attempt', () => {
  // Deliberately NOT in the never-retry set: it is refused by layer (b),
  // because nothing on the transport list describes it. Both floors are
  // asserted so a compile error cannot slip through by being neither.
  assert.equal(goFetchNeverRetryable(goCompileError), false);
  assert.equal(isTransientRegistryFailure(goCompileError), false);

  const result = run(['go', 'install', 'example.com/tool@v1.0.0'], { stderr: goCompileError });
  assert.notEqual(result.status, 0);
  assert.equal(result.verdict, 'not-transient');
  assert.equal(result.attempts, 1);
});

test('a checksum refusal that ALSO names a transport fault is still refused outright', () => {
  // The ordering rule, mirroring lib/registry.mjs's rate-limit-wins rule. A
  // second attempt against a different proxy replica is precisely how a
  // poisoned module would eventually be accepted, so the dangerous class has
  // to win even when the text would otherwise look retryable.
  const mixed = `${checksumMismatch}${autoexportModFailure}`;
  assert.equal(isTransientRegistryFailure(mixed), true, 'the fixture must genuinely look transient too');

  const result = run(['go', 'mod', 'download'], { stderr: mixed });
  assert.equal(result.verdict, 'never-retryable');
  assert.equal(result.attempts, 1);
});

test('a fault reported on stdout rather than stderr is classified the same way', () => {
  const seen = [];
  const result = runWithRetry({
    argv: ['go', 'mod', 'download'],
    spawn: () => {
      seen.push(1);
      return { status: 1, stdout: autoexportModFailure, stderr: '' };
    },
    sleep: silent,
    onLog: silent,
  });
  assert.equal(result.verdict, 'exhausted');
  assert.equal(seen.length, 5);
});

// ---------------------------------------------------------------------------
// Layer (a): what may be wrapped at all.
// ---------------------------------------------------------------------------

test('only fetch-only commands are eligible for the retry loop', () => {
  assert.equal(isFetchOnly(['go', 'mod', 'download']), true);
  assert.equal(isFetchOnly(['go', 'install', 'github.com/tsouza/gremlins/cmd/gremlins@v0.6.0']), true);

  // The commands whose failure carries real information. Each one must be
  // unwrappable by construction, so it can only ever fail on attempt 1.
  for (const argv of [
    ['go', 'test', './...'],
    ['go', 'build', './...'],
    ['go', 'list', '-tags', 'chdb', './...'],
    ['go', 'vet', './...'],
    ['go', 'mod', 'tidy'],
    ['just', 'test'],
    ['gremlins', 'unleash'],
    ['node', '.github/scripts/coverage-package-floor.mjs'],
  ]) {
    assert.equal(isFetchOnly(argv), false, `${argv.join(' ')} must not be wrappable`);
  }
});

test('backoff is a linear multiple of the named step', () => {
  const slept = [];
  runWithRetry({
    argv: ['go', 'mod', 'download'],
    spawn: () => ({ status: 1, stdout: '', stderr: autoexportModFailure }),
    sleep: (s) => slept.push(s),
    onLog: silent,
  });
  // Four sleeps for five attempts: the last failure is not followed by a wait.
  assert.deepEqual(slept, [1, 2, 3, 4].map((n) => n * goFetchBackoffStepSeconds));
});

test('a bare invocation warms the module cache; `--` hands over an explicit command', () => {
  assert.deepEqual(commandFrom([]), warmCommand);
  assert.deepEqual(warmCommand, ['go', 'mod', 'download']);
  assert.deepEqual(commandFrom(['--', 'go', 'install', 'example.com/tool@v1.0.0']), [
    'go',
    'install',
    'example.com/tool@v1.0.0',
  ]);
});
