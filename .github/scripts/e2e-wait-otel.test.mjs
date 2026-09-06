// e2e-wait-otel.test.mjs — node:test guard for `pipelineReady`, the exact
// success predicate `just e2e-wait-otel`'s inline bash checked before this
// extraction (cerberus issue #3096). The kubectl/ClickHouse polling loop
// around it is exercised live by `just e2e-up && just e2e-seed-rolling &&
// just e2e-wait-otel` in the real `e2e` CI job, which is the only place a
// real OTel collector + ClickHouse pair exists to poll.

import assert from 'node:assert/strict';
import test from 'node:test';

import { pipelineReady } from './e2e-wait-otel.mjs';

// A fully-populated, ready snapshot — every assertion below starts from
// this and breaks exactly one field, so each test isolates one condition.
function readySnapshot(overrides = {}) {
  return {
    logs: 10,
    chlogs: 3,
    traces: 5,
    sum: 4,
    gauge: 0,
    histogram: 2,
    histSpread: 90,
    spread: 90,
    ...overrides,
  };
}

test('pipelineReady is true once every signal has data and both spreads clear the floor', () => {
  assert.equal(pipelineReady(readySnapshot()), true);
});

test('pipelineReady accepts gauge-only metrics when sum is empty', () => {
  assert.equal(pipelineReady(readySnapshot({ sum: 0, gauge: 4 })), true);
});

test('pipelineReady is false when logs are still empty', () => {
  assert.equal(pipelineReady(readySnapshot({ logs: 0 })), false);
});

test('pipelineReady is false when the clickhouse-service log stream is still empty', () => {
  assert.equal(pipelineReady(readySnapshot({ chlogs: 0 })), false);
});

test('pipelineReady is false when traces are still empty', () => {
  assert.equal(pipelineReady(readySnapshot({ traces: 0 })), false);
});

test('pipelineReady is false when neither sum nor gauge has data', () => {
  assert.equal(pipelineReady(readySnapshot({ sum: 0, gauge: 0 })), false);
});

test('pipelineReady is false when the metric spread has not reached the 60s floor yet', () => {
  assert.equal(pipelineReady(readySnapshot({ spread: 59 })), false);
  assert.equal(pipelineReady(readySnapshot({ spread: 60 })), true);
});

test('pipelineReady is false when the histogram companion is still empty', () => {
  assert.equal(pipelineReady(readySnapshot({ histogram: 0 })), false);
});

test('pipelineReady is false when the histogram spread has not reached the 60s floor yet', () => {
  assert.equal(pipelineReady(readySnapshot({ histSpread: 59 })), false);
  assert.equal(pipelineReady(readySnapshot({ histSpread: 60 })), true);
});
