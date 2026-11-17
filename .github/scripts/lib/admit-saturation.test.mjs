// admit-saturation.test.mjs — node:test guard for the load-admit-saturation
// verdict (lib/admit-saturation.mjs). The burst itself runs live in the e2e
// workflow's `chaos` job; these cases pin that the verdict passes the burst a
// working limiter produces and fails the bursts a disabled, over-admitting or
// never-admitting limiter, or an inert chaos sleep, produce.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

import { parseEnvFile } from '../e2e-chaos-overlay.mjs';
import {
  ADMIT_BURST_SIZE,
  ADMIT_CAP_ENV,
  admitCapFromEnvArgs,
  isAdmitShed,
  judgeAdmitSaturation,
  statusTally,
} from './admit-saturation.mjs';

const chaosOverlayEnvPath = fileURLToPath(
  new URL('../../../test/e2e/chaos/manifests/chaos-overlay.env', import.meta.url),
);
const e2eCerberusValuesPath = fileURLToPath(new URL('../../../test/e2e/k3s/cerberus-values.yaml', import.meta.url));

// The responses cerberus gives a burst request, by path through the stack.
// shed: internal/api/admit/admit.go's Middleware rejection.
const shed = () => ({
  ok: true,
  status: 503,
  headers: { 'retry-after': '1', 'content-type': 'text/plain; charset=utf-8' },
  body: 'admission control: server saturated\n',
  elapsedMs: 4,
});
// held: admitted, then ClickHouse aborts the chaos sleep at 3s (code 159).
const held = () => ({
  ok: true,
  status: 503,
  headers: { 'content-type': 'application/json' },
  body: '{"status":"error","errorType":"timeout","error":"query execution timeout exceeded"}',
  elapsedMs: 3_020,
});
// fast: admitted and answered at once — the header was inert.
const fast = () => ({ ok: true, status: 200, headers: {}, body: '{"status":"success"}', elapsedMs: 12 });

const burst = (counts) => Object.entries(counts).flatMap(([kind, n]) => Array.from({ length: n }, { shed, held, fast }[kind]));

const lane = { replicas: 2, capPerReplica: 2, minHoldMs: 2_000 };

test('a working limiter under a held burst passes: 4 slots admitted, 12 shed', () => {
  const v = judgeAdmitSaturation({ results: burst({ held: 4, shed: 12 }), ...lane });
  assert.deepEqual(v.failures, []);
  assert.equal(v.shed, 12);
  assert.equal(v.admitted, 4);
  assert.equal(v.capacity, 4);
  assert.equal(v.minShed, 12);
});

test('extra sheds from an uneven load-balancer spread still pass', () => {
  const v = judgeAdmitSaturation({ results: burst({ held: 2, shed: 14 }), ...lane });
  assert.deepEqual(v.failures, []);
});

test('admission control disabled: every request is held, none shed, the verdict fails', () => {
  const v = judgeAdmitSaturation({ results: burst({ held: 16 }), ...lane });
  assert.equal(v.shed, 0);
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /expected at least 12 of 16 held requests to shed/);
  assert.match(v.failures[0], /0 did \[503×16\]/);
});

test('a replica admitting past its cap fails the verdict', () => {
  const v = judgeAdmitSaturation({ results: burst({ held: 5, shed: 11 }), ...lane });
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /expected at least 12 of 16/);
  assert.match(v.failures[0], /11 did/);
});

test('an inert chaos sleep fails on the hold even when the shed floor happens to be met', () => {
  const v = judgeAdmitSaturation({ results: burst({ fast: 4, shed: 12 }), ...lane });
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /4 admitted request\(s\) returned before the 2000 ms hold \(fastest 12 ms\)/);
});

test('an inert chaos sleep with few sheds reports both the hold and the floor', () => {
  const v = judgeAdmitSaturation({ results: burst({ fast: 16 }), ...lane });
  assert.equal(v.failures.length, 2);
  assert.match(v.failures[0], /returned before the 2000 ms hold/);
  assert.match(v.failures[1], /expected at least 12 of 16/);
});

test('a request with no HTTP response fails the verdict and is neither shed nor admitted', () => {
  const dropped = { ok: false, status: 0, headers: {}, body: '', err: 'ECONNRESET', elapsedMs: 30 };
  const v = judgeAdmitSaturation({ results: [...burst({ held: 3, shed: 12 }), dropped], ...lane });
  assert.equal(v.shed, 12);
  assert.equal(v.admitted, 3);
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /1 burst request\(s\) got no HTTP response \(ECONNRESET\) \[503×3 shed×12 transport-error×1\]/);
});

test('an admitted response without a measured elapsedMs fails the hold check', () => {
  const unmeasured = { ...held(), elapsedMs: undefined };
  const v = judgeAdmitSaturation({ results: [...burst({ held: 3, shed: 12 }), unmeasured], ...lane });
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /1 admitted request\(s\) returned before the 2000 ms hold/);
});

test('a limiter that sheds every request fails: nothing was admitted', () => {
  const v = judgeAdmitSaturation({ results: burst({ shed: 16 }), ...lane });
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /no request of 16 was admitted/);
});

test('a burst that cannot oversubscribe the slots is a failure, not a pass', () => {
  const v = judgeAdmitSaturation({ results: burst({ held: 16 }), replicas: 8, capPerReplica: 2, minHoldMs: 2_000 });
  assert.equal(v.failures.length, 1);
  assert.match(v.failures[0], /cannot oversubscribe 8 replica\(s\) × cap 2 = 16 slots/);
});

test('a missing replica count or cap fails before judging the burst', () => {
  const noReplica = judgeAdmitSaturation({ results: burst({ held: 4, shed: 12 }), replicas: 0, capPerReplica: 2, minHoldMs: 2_000 });
  assert.match(noReplica.failures.join('\n'), /no Ready cerberus replica/);
  const noCap = judgeAdmitSaturation({ results: burst({ held: 4, shed: 12 }), replicas: 2, capPerReplica: null, minHoldMs: 2_000 });
  assert.match(noCap.failures.join('\n'), /prom admit cap must be a positive integer/);
});

test('isAdmitShed tells the admit 503 apart from the handler 503s', () => {
  assert.equal(isAdmitShed(shed()), true);
  assert.equal(isAdmitShed(held()), false);
  const breakerOpen = { ok: true, status: 503, headers: { 'retry-after': '5' }, body: '{"errorType":"unavailable"}' };
  assert.equal(isAdmitShed(breakerOpen), false);
  const saturatedWrongRetry = { ...shed(), headers: { 'retry-after': '5' } };
  assert.equal(isAdmitShed(saturatedWrongRetry), false);
  // A breaker opened with CERBERUS_CH_BREAKER_OPEN_INTERVAL<=1s stamps
  // Retry-After: 1 too; only the body marker tells it apart.
  const breakerOneSecond = { ...breakerOpen, headers: { 'retry-after': '1' } };
  assert.equal(isAdmitShed(breakerOneSecond), false);
  const saturatedWrongStatus = { ...shed(), status: 429 };
  assert.equal(isAdmitShed(saturatedWrongStatus), false);
});

test('statusTally names every outcome in first-seen order', () => {
  assert.equal(statusTally(burst({ held: 2, shed: 3 })), '503×2 shed×3');
});

test('admitCapFromEnvArgs reads positive integers only', () => {
  assert.equal(admitCapFromEnvArgs(['CERBERUS_ADMIT_PROM=2'], 'CERBERUS_ADMIT_PROM'), 2);
  assert.equal(admitCapFromEnvArgs(['CERBERUS_ADMIT_PROMX=3', 'CERBERUS_ADMIT_PROM=7'], 'CERBERUS_ADMIT_PROM'), 7);
  assert.equal(admitCapFromEnvArgs(['CERBERUS_ADMIT_LOKI=2'], 'CERBERUS_ADMIT_PROM'), null);
  assert.equal(admitCapFromEnvArgs(['CERBERUS_ADMIT_PROM=0'], 'CERBERUS_ADMIT_PROM'), null);
  assert.equal(admitCapFromEnvArgs(['CERBERUS_ADMIT_PROM=two'], 'CERBERUS_ADMIT_PROM'), null);
});

test('the burst oversubscribes the prom budget at the e2e HPA ceiling', () => {
  const cap = admitCapFromEnvArgs(parseEnvFile(readFileSync(chaosOverlayEnvPath, 'utf8')), ADMIT_CAP_ENV);
  assert.ok(Number.isInteger(cap) && cap >= 1, `${ADMIT_CAP_ENV} missing from the chaos overlay`);
  const autoscaling = readFileSync(e2eCerberusValuesPath, 'utf8').match(/^autoscaling:\n(?:[ \t].*\n)*?[ \t]+maxReplicas:[ \t]*(\d+)/m);
  assert.ok(autoscaling, 'autoscaling.maxReplicas missing from the e2e cerberus values');
  const maxReplicas = Number(autoscaling[1]);
  const v = judgeAdmitSaturation({
    results: burst({ held: ADMIT_BURST_SIZE }),
    replicas: maxReplicas,
    capPerReplica: cap,
    minHoldMs: 2_000,
  });
  assert.ok(v.minShed >= 1, `burst ${ADMIT_BURST_SIZE} cannot oversubscribe ${maxReplicas} × ${cap} slots`);
  // Every request held and none shed must still fail at the ceiling.
  assert.match(v.failures.join('\n'), /held requests to shed/);
});
