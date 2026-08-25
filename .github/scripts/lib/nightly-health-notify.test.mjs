// lib/nightly-health-notify.test.mjs — node:test guard for runNotifyMain(),
// the create/comment/close/noop orchestration every nightly-health lane's
// main() drives (notify-nightly-failure.mjs, notify-perf-nightly-failure.mjs,
// notify-perf-nightly-selfcheck-failure.mjs). The lane wrappers' own
// `.test.mjs` files pin the PURE helpers (classifyNightlyHealth,
// findTrackingIssue, decideNotifyAction, buildFailureBody/buildRecoveryBody);
// this file is the one that actually drives runNotifyMain itself, mocking
// the `gh` shell-out via the captureImpl/exit seams it accepts for exactly
// this purpose.
//
// The `exit` stub THROWS rather than returning normally, matching what the
// real `process.exit` does for this function's own control flow: code after
// an exit() call is never meant to run, so a non-throwing stub would let
// execution fall through into statements written assuming it never will.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { runNotifyMain } from './nightly-health-notify.mjs';

class ExitSignal extends Error {
  constructor(code) {
    super(`process would exit(${code})`);
    this.code = code;
  }
}

function makeExit() {
  const calls = [];
  const exit = (code) => {
    calls.push(code);
    throw new ExitSignal(code);
  };
  exit.calls = calls;
  return exit;
}

// Queue-based `gh` stub: each call consumes the next scripted response, so a
// test's response list doubles as an assertion that no MORE gh calls happen
// than the ones it names.
function makeCapture(responses) {
  const calls = [];
  const captureImpl = (cmd, args) => {
    calls.push({ cmd, args });
    if (responses.length === 0) {
      throw new Error(`makeCapture: no scripted response left for call ${calls.length} (${cmd} ${args.join(' ')})`);
    }
    return responses.shift();
  };
  captureImpl.calls = calls;
  return captureImpl;
}

const ok = (stdout = '') => ({ status: 0, stdout, stderr: '' });
const fail = (stderr = 'boom') => ({ status: 1, stdout: '', stderr });

function argAfter(args, flag) {
  const i = args.indexOf(flag);
  return i === -1 ? undefined : args[i + 1];
}

const base = {
  repo: 'tsouza/cerberus',
  runId: '123',
  runUrl: 'https://github.com/tsouza/cerberus/actions/runs/123',
  trackingLabels: ['automated', 'area/ci'],
  trackingTitle: 'nightly e2e run did not reach a clean pass',
  laneLabel: 'e2e',
  issueRef: 'tsouza/cerberus#1861',
  contextTitle: 'nightly-health-notify',
  failureNoticeTitle: 'nightly e2e run failed',
};

test('clean night, no existing tracking issue -> noop: one gh call, never exits', () => {
  const captureImpl = makeCapture([ok('[]')]);
  const exit = makeExit();
  runNotifyMain({ ...base, jobResults: { a: 'success' }, captureImpl, exit });
  assert.equal(captureImpl.calls.length, 1);
  assert.equal(captureImpl.calls[0].cmd, 'gh');
  assert.deepEqual(captureImpl.calls[0].args.slice(0, 2), ['issue', 'list']);
  assert.equal(argAfter(captureImpl.calls[0].args, '--limit'), '30');
  assert.equal(exit.calls.length, 0);
});

test('clean night with an existing tracking issue -> closes it, never exits', () => {
  const captureImpl = makeCapture([ok(JSON.stringify([{ number: 42, title: base.trackingTitle }])), ok('')]);
  const exit = makeExit();
  runNotifyMain({ ...base, jobResults: { a: 'success' }, captureImpl, exit });
  assert.equal(captureImpl.calls.length, 2);
  assert.deepEqual(captureImpl.calls[1].args.slice(0, 2), ['issue', 'close']);
  assert.equal(captureImpl.calls[1].args[2], '42');
  assert.equal(exit.calls.length, 0);
});

test('not-clean night, no existing issue -> files a new one, carries the failed job into the body, exits 1', () => {
  const captureImpl = makeCapture([ok('[]'), ok('https://github.com/tsouza/cerberus/issues/99\n')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'failure' }, captureImpl, exit }), ExitSignal);
  assert.equal(captureImpl.calls.length, 2);
  const createArgs = captureImpl.calls[1].args;
  assert.deepEqual(createArgs.slice(0, 2), ['issue', 'create']);
  assert.equal(argAfter(createArgs, '--title'), base.trackingTitle);
  assert.match(argAfter(createArgs, '--body'), /a: failure/);
  assert.deepEqual(exit.calls, [1]);
});

test('not-clean night with an existing issue -> comments on it, exits 1', () => {
  const captureImpl = makeCapture([ok(JSON.stringify([{ number: 7, title: base.trackingTitle }])), ok('')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'failure' }, captureImpl, exit }), ExitSignal);
  assert.equal(captureImpl.calls.length, 2);
  assert.deepEqual(captureImpl.calls[1].args.slice(0, 2), ['issue', 'comment']);
  assert.equal(captureImpl.calls[1].args[2], '7');
  assert.deepEqual(exit.calls, [1]);
});

test('a tracking issue with a DIFFERENT title is not mistaken for this lane\'s own', () => {
  const captureImpl = makeCapture([ok(JSON.stringify([{ number: 1, title: 'some other tracking issue' }])), ok('id')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'failure' }, captureImpl, exit }), ExitSignal);
  // Falls through to 'create', not 'comment', since findTrackingIssue found no match.
  assert.deepEqual(captureImpl.calls[1].args.slice(0, 2), ['issue', 'create']);
});

test('a failing `gh issue list` exits 1 without attempting any further gh call', () => {
  const captureImpl = makeCapture([fail('rate limited')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'success' }, captureImpl, exit }), ExitSignal);
  assert.equal(captureImpl.calls.length, 1);
  assert.deepEqual(exit.calls, [1]);
});

test('unparsable JSON from `gh issue list` exits 1', () => {
  const captureImpl = makeCapture([ok('not json')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'success' }, captureImpl, exit }), ExitSignal);
  assert.equal(captureImpl.calls.length, 1);
  assert.deepEqual(exit.calls, [1]);
});

test('a failing `gh issue create` exits 1 from inside the gh wrapper, with no second exit call', () => {
  const captureImpl = makeCapture([ok('[]'), fail('422 already exists')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'failure' }, captureImpl, exit }), ExitSignal);
  assert.equal(captureImpl.calls.length, 2);
  assert.deepEqual(exit.calls, [1]);
});

test('a failing `gh issue close` exits 1 even though the night was clean', () => {
  const captureImpl = makeCapture([ok(JSON.stringify([{ number: 42, title: base.trackingTitle }])), fail('locked')]);
  const exit = makeExit();
  assert.throws(() => runNotifyMain({ ...base, jobResults: { a: 'success' }, captureImpl, exit }), ExitSignal);
  assert.deepEqual(exit.calls, [1]);
});
