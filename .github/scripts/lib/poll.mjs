// poll.mjs — the generic deadline+interval polling primitive shared across
// the e2e/chaos Node scripts (cerberus issue #3172). Started as a private
// helper inside chaos-run.mjs; promoted here because a second script
// (e2e-datashard-replica-affinity-verify.mjs) had already re-derived the
// same deadline+poll+sleep loop by hand rather than reusing it, and nothing
// stopped a third from doing the same — a bug fixed in one hand-rolled copy
// would not have propagated to the others.

import { setTimeout as sleep } from 'node:timers/promises';

import { log } from './gh.mjs';

// DEFAULT_POLL_INTERVAL_MS matches chaos-run.mjs's own pre-promotion
// POLL_INTERVAL_MS exactly, so moving pollUntil here changes nothing for
// any of its existing call sites that rely on the default.
export const DEFAULT_POLL_INTERVAL_MS = 2_000;

// pollUntil invokes `fn` (async, returns truthy on success) every
// intervalMs until it succeeds or deadlineMs elapses. Returns true on
// success, false on timeout. A thrown `fn` counts as one failed attempt
// (logged, not re-thrown) rather than aborting the poll — the ASSERT-side
// retry primitive: faults are one-shot + idempotent, recovery checks retry
// to their deadline.
export async function pollUntil(fn, { deadlineMs, intervalMs = DEFAULT_POLL_INTERVAL_MS, label = '' } = {}) {
  const start = Date.now();
  let attempt = 0;
  while (Date.now() - start < deadlineMs) {
    attempt += 1;
    let ok = false;
    try {
      ok = await fn(attempt);
    } catch (e) {
      ok = false;
      log(`    [poll ${label}] attempt ${attempt} threw: ${String(e?.message || e)}`);
    }
    if (ok) return true;
    await sleep(intervalMs);
  }
  return false;
}
