// k8s.test.mjs — node:test guard for lib/k8s.mjs's kubectl/ClickHouse
// helpers (cerberus issue #3096). Everything here is exercised against a
// FAKE kubectl closure, never a real cluster — the real-cluster contract
// (a namespace flag, an exec target, clickhouse-client's own flags) is
// covered live by `just e2e-*` in the real `e2e` CI job, which is the only
// place a real kubectl/ClickHouse exists.
//
// The failure paths of `clickhousePodName`/`chQuery` call the real
// `process.exit` directly (a pre-existing shape from `lib/bwc-k8s.mjs`, kept
// as-is rather than a DI refactor out of this issue's scope) and so are not
// exercised here — a real `process.exit(1)` inside a `node --test` process
// would kill the whole test run, not just fail one assertion. `chQueryRaw`'s
// own never-exits contract is what e2e-wait-otel.mjs actually relies on, and
// is covered below.

import assert from 'node:assert/strict';
import test from 'node:test';

import { makeKubectl, clickhousePodName, chQuery, chQueryRaw } from './k8s.mjs';

// fakeCapture — records every invocation and answers from a queue of
// canned { status, stdout, stderr } results, FIFO.
function fakeCapture(results) {
  const calls = [];
  const queue = [...results];
  const capture = (cmd, args) => {
    calls.push([cmd, ...args]);
    return queue.shift() ?? { status: 0, stdout: '', stderr: '' };
  };
  return { capture, calls };
}

test('makeKubectl prefixes every call with -n <namespace>', () => {
  const { capture, calls } = fakeCapture([{ status: 0, stdout: 'ok', stderr: '' }]);
  const kubectl = makeKubectl(capture, 'cerberus');
  kubectl(['get', 'pods']);
  assert.deepEqual(calls, [['kubectl', '-n', 'cerberus', 'get', 'pods']]);
});

test('clickhousePodName returns the trimmed pod name on success', () => {
  const { capture } = fakeCapture([{ status: 0, stdout: 'cerberus-clickhouse-0\n', stderr: '' }]);
  const kubectl = makeKubectl(capture, 'cerberus');
  assert.equal(clickhousePodName(kubectl, 'cerberus'), 'cerberus-clickhouse-0');
});

test('chQueryRaw builds the clickhouse-client invocation and never exits on failure', () => {
  const { capture, calls } = fakeCapture([{ status: 1, stdout: '', stderr: 'boom' }]);
  const kubectl = makeKubectl(capture, 'cerberus');
  const res = chQueryRaw(kubectl, 'deploy/clickhouse', { database: 'otel' }, 'SELECT 1');
  assert.equal(res.status, 1);
  assert.deepEqual(calls, [[
    'kubectl', '-n', 'cerberus', 'exec', 'deploy/clickhouse', '--',
    'clickhouse-client', '--user', 'cerberus', '--password', 'cerberus',
    '--database', 'otel', '--query', 'SELECT 1',
  ]]);
});

test('chQueryRaw omits --database and --format when not given', () => {
  const { capture, calls } = fakeCapture([{ status: 0, stdout: '', stderr: '' }]);
  const kubectl = makeKubectl(capture, 'cerberus');
  chQueryRaw(kubectl, 'chpod', {}, 'SELECT 1');
  assert.deepEqual(calls, [[
    'kubectl', '-n', 'cerberus', 'exec', 'chpod', '--',
    'clickhouse-client', '--user', 'cerberus', '--password', 'cerberus', '--query', 'SELECT 1',
  ]]);
});

test('chQuery returns trimmed stdout on success', () => {
  const { capture } = fakeCapture([{ status: 0, stdout: '  42\n', stderr: '' }]);
  const kubectl = makeKubectl(capture, 'cerberus');
  assert.equal(chQuery(kubectl, 'chpod', { database: 'otel' }, 'SELECT count()'), '42');
});
