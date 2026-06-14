// chaos-run.mjs — the live-stack chaos-engineering lane driver.
//
// Fault-injects against the running k3d e2e stack (cerberus + ClickHouse +
// Grafana + OTel collector that `just e2e-up` stood up) and asserts the
// gateway's landed resilience contracts hold under REAL faults:
//
//   #1 circuit breaker  (internal/chclient/breaker.go) — a CH outage trips
//      the shared breaker -> 503 Retry-After:5 every head, /readyz red,
//      /healthz green (no restart), auto-recovers via the HALF-OPEN probe.
//   #2 per-query wall-clock timeout (#886) — a slow query -> clean 503
//      errorType=timeout, breaker-NEUTRAL (CH code-159 coerced to success),
//      admit slot + pooled conn released.
//   #4 admission control + pool — over-cap concurrency shed cleanly with
//      503 Retry-After:1 'server saturated', breaker stays CLOSED.
//   #3 handler-panic envelope is covered DETERMINISTICALLY by Layer 10 unit
//      chaos; the live lane only corroborates no lingering 5xx after the
//      cumulative fault storm (a passive end-of-run health gate).
//
// This is an INFORMATIONAL lane (the `chaos` job in e2e.yml; push-to-main +
// nightly + manual ONLY, never a PR gate). Heavy + chaos flakes, so the
// design is flake-resistant: GENEROUS bounded recovery polls, ACCEPT the
// documented transient orderings (502-then-503), retry ASSERTS (never
// faults), heal-between-each scenario, and metric-based asserts are
// POST-recovery corroboration with a settle poll (OTLP -> collector -> CH ->
// Prom head lags the fault by seconds; during-fault asserts key on
// immediate HTTP status + /readyz body + kubectl state).
//
// Env contract:
//   CERBERUS_URL    cerberus base URL          (default http://localhost:8080)
//   CHAOS_NS        k8s namespace              (default cerberus)
//   CHAOS_PHASE     'phase-1' | 'all'          (default phase-1)
//   CHAOS_SCENARIOS comma list to run a subset (default: all in the phase)
//   CHAOS_MANIFESTS dir of chaos manifests     (default test/e2e/chaos/manifests)
//
// Exit codes: 0 = every selected scenario passed (or was recorded
// not-applicable with a ::notice::), 1 = any contract assertion failed.

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';

// ---- env / constants -------------------------------------------------

const CERBERUS_URL = (process.env.CERBERUS_URL || 'http://localhost:8080').replace(/\/$/, '');
const NS = process.env.CHAOS_NS || 'cerberus';
const PHASE = process.env.CHAOS_PHASE || 'phase-1';
const MANIFESTS = process.env.CHAOS_MANIFESTS || 'test/e2e/chaos/manifests';
const ONLY = (process.env.CHAOS_SCENARIOS || '')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);

// Recovery / fault-window budgets (ms). Generous by design — chaos lanes
// flake on tight deadlines, so every recovery check polls to a bounded
// deadline rather than asserting immediately after a fault.
const CH_RECOVERY_DEADLINE_MS = 120_000; // CH pod recreate + rollout + breaker close
const CERBERUS_RECOVERY_DEADLINE_MS = 90_000; // replica reschedule + readyz green
const BREAKER_TRIP_DEADLINE_MS = 30_000; // drive volume until 503+Retry-After:5 lands
const PARTITION_TRIP_DEADLINE_MS = 60_000; // slower: each dial blocks up to DialTimeout
const METRIC_SETTLE_DEADLINE_MS = 90_000; // OTLP flush lag before self-metrics corroborate
const POLL_INTERVAL_MS = 2_000;
const SETTLE_INTERVAL_MS = 3_000;

// Breaker HALF-OPEN -> CLOSED drive (ch-pod-kill heal). When CH comes back the
// breaker is OPEN; after its OPEN_INTERVAL cooldown the NEXT query through it
// transitions OPEN -> HALF-OPEN and admits exactly one recovery probe, and a
// SUCCESSFUL probe is what closes it (internal/chclient/breaker.go record():
// stateHalfOpen + err==nil -> stateClosed). /readyz going green and a single
// "all 3 heads 200" sweep do NOT guarantee the close: /readyz keys on the
// SEPARATE per-head probe breaker (#904), and with >=2 replicas each carries
// its OWN main query breaker, so one sweep may close one replica's breaker
// while another stays HALF-OPEN. We therefore DRIVE sustained successful
// CH-touching queries (prom head -> real CH round-trip through the main
// breaker) and POLL cerberus_ch_breaker_state until it reads CLOSED, so the
// post-recovery assertion (and every downstream scenario that reads the gauge)
// sees a genuinely-CLOSED breaker. This drives the legitimate transition; it
// never weakens the assertion.
const BREAKER_CLOSE_DEADLINE_MS = 90_000; // bound the drive-to-CLOSED poll (OTLP gauge lag + per-replica probes)
const BREAKER_CLOSE_DRIVE_QUERIES = 8; // successful CH-touching queries to fire per poll tick (covers >=2 replicas' breakers)

// cerberus_ch_breaker_state gauge encoding — mirrors the breakerGauge* consts
// in internal/chclient/breaker_metrics.go (closed=0, open=1, half-open=2). The
// chaos asserts key on CLOSED (0) as the only correct steady state for a
// breaker-neutral fault; the labels make a failure message read truthfully
// (a bare "got 2" misleads — 2 is HALF-OPEN, not a fresh trip).
const BREAKER_STATE_CLOSED = 0;
const BREAKER_STATE_OPEN = 1;
const BREAKER_STATE_HALF_OPEN = 2;

function breakerStateName(v) {
  switch (v) {
    case BREAKER_STATE_CLOSED:
      return 'CLOSED';
    case BREAKER_STATE_OPEN:
      return 'OPEN';
    case BREAKER_STATE_HALF_OPEN:
      return 'HALF-OPEN';
    default:
      return `unknown(${v})`;
  }
}

// ---- deliberately-expensive query (slow-query / admit-saturation) ----
//
// Both the ch-slow-query-timeout and load-admit-saturation scenarios need a
// query that is GENUINELY expensive to evaluate — heavy enough to blow the
// (calibrated) CERBERUS_QUERY_TIMEOUT the chaos overlay sets (slow-query) or
// to hold an admit slot long enough to overlap a concurrent burst (saturation).
//
// The cost MUST come from COMPUTE PER ANCHOR, not from anchor count. An
// earlier version drove cost with a 30-day range at step=1s (~2.6M anchors),
// but cerberus's resolution guard (internal/api/prom/handler.go:
// maxResolutionPoints = 11000) rejects any range query whose
// (end-start)/step exceeds 11000 points with a 400 BEFORE the wall-clock
// timeout is even armed — so the "slow" query 400'd instead of timing out,
// and the scenario asserted a 503 it could never see. The 11000-point cap is
// an intentional Prometheus-compat invariant, so the query has to stay UNDER
// it while still costing real compute to evaluate.
//
// The cost MUST ALSO be UNCOLLAPSIBLE. An earlier version used
//   max_over_time( sum(rate(<counter>[INNER])) [SUBQ_RANGE:SUBQ_STEP] )
// but the seed is deliberately thin — http_server_request_duration_count is
// a SINGLE low-cardinality series of 600 samples whose Count grows linearly
// (test/e2e/seed/cmd/seed/main.go). Over uniform, linearly-growing data the
// optimizer + the subquery folding collapse max_over_time(sum(rate(...))) to
// a CHEAP FLAT CONSTANT (~hundred ms, returning the same value at every
// timestamp). It never exceeded the cap -> 200 instead of the asserted 503
// (dispatch run 27505378745). Reordering / fuller data didn't help: the
// problem was the query is cheap on cerberus, not the data being thin.
//
// The fix: stddev_over_time over a sub-stepped rate() subquery. A per-anchor
// dispersion CANNOT fold to a constant (each outer anchor must materialise
// its SUBQ_RANGE/SUBQ_STEP inner rate() samples and compute their stddev),
// so the cost is real CH compute regardless of optimizer or uniform data.
// Measured on the live compose stack (2026-06-14): ~650 ms wall-clock on the
// seed-only {job=api} series vs ~8 ms for a trivial query=up. The chaos
// overlay then calibrates CERBERUS_QUERY_TIMEOUT to 250ms (test-only) — it
// sits cleanly between the two (heavy ~650 ms > cap 250 ms > trivial ~8 ms),
// so the heavy query reliably 503s errorType=timeout while up stays 200.
const SLOW_QUERY_RANGE_SECONDS = 2 * 3600; // 2h outer window
const SLOW_QUERY_STEP_SECONDS = 1; // 1s outer step => 7200 outer anchors (< 11000 cap)
const SLOW_QUERY_SUBQ_RANGE = '1h'; // each anchor fans out a 1h subquery
const SLOW_QUERY_SUBQ_STEP = '1s'; // at 1s => 3600 inner sub-anchors per outer anchor
const SLOW_QUERY_INNER_RANGE = '5m'; // each sub-anchor rate()s 5m of raw counter rows
// The seeded counter the rate() scans (test/e2e/seed: 600 samples in
// otel_metrics_sum -> histogram-routed under this name). Scoped to the seed
// series {job="api"} so the cost is independent of any self-telemetry the
// dogfood loop happens to add — the seed shape is guaranteed in the k3d lane.
const SLOW_QUERY_COUNTER = 'http_server_request_duration_count';
const SLOW_QUERY_SERIES_MATCHER = '{job="api"}';

// slowQueryPath builds the /api/v1/query_range path for the expensive
// nested-subquery query. anchored to `now` so the outer window overlaps the
// rolling-seeded data. The point count is SLOW_QUERY_RANGE_SECONDS /
// SLOW_QUERY_STEP_SECONDS = 7200, comfortably under the 11000-point
// resolution cap, so the request passes the guard and reaches the
// wall-clock-timeout path the slow-query contract pins.
function slowQueryPath() {
  const now = Math.floor(Date.now() / 1000);
  const start = now - SLOW_QUERY_RANGE_SECONDS;
  const expr =
    `stddev_over_time(` +
    `rate(${SLOW_QUERY_COUNTER}${SLOW_QUERY_SERIES_MATCHER}[${SLOW_QUERY_INNER_RANGE}])` +
    `[${SLOW_QUERY_SUBQ_RANGE}:${SLOW_QUERY_SUBQ_STEP}])`;
  return (
    '/api/v1/query_range?query=' +
    encodeURIComponent(expr) +
    `&start=${start}&end=${now}&step=${SLOW_QUERY_STEP_SECONDS}`
  );
}

// The three data-plane head probes. Each is a cheap query the seeded +
// rolling OTel data answers with a 200 in steady state.
const HEAD_PROBES = [
  { name: 'prom', path: '/api/v1/query?query=up' },
  {
    name: 'loki',
    path:
      '/loki/api/v1/query?query=' +
      encodeURIComponent('{service_name=~".+"}') +
      '&limit=1',
  },
  { name: 'tempo', path: '/api/search?limit=1' },
];

// ---- HTTP helpers ----------------------------------------------------

// httpGet — fetch with a per-request timeout, never throwing. Returns
// { ok, status, headers, body, err }. A transport failure (dial reset,
// timeout) surfaces as ok:false + err set + status:0, which the callers
// treat as a fault-window signal, not a crash.
async function httpGet(path, { timeoutMs = 10_000, headers = {} } = {}) {
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const res = await fetch(CERBERUS_URL + path, { signal: ctrl.signal, headers });
    const body = await res.text();
    const h = {};
    for (const [k, v] of res.headers.entries()) h[k.toLowerCase()] = v;
    return { ok: true, status: res.status, headers: h, body, err: null };
  } catch (e) {
    return { ok: false, status: 0, headers: {}, body: '', err: String(e?.message || e) };
  } finally {
    clearTimeout(t);
  }
}

function jsonField(body, field) {
  try {
    return JSON.parse(body)?.[field];
  } catch {
    return undefined;
  }
}

// ---- kubectl helpers -------------------------------------------------

function kubectl(args, opts = {}) {
  return capture('kubectl', ['-n', NS, ...args], opts);
}

function kubectlOut(args) {
  const res = kubectl(args);
  return res.status === 0 ? res.stdout.trim() : '';
}

// firstPodName — the name of the first pod matching a label selector, or
// '' if none. Used to scope cerberus-pod-kill to ONE replica by name.
function firstPodName(selector) {
  const out = kubectlOut(['get', 'pods', '-l', selector, '-o', 'jsonpath={.items[0].metadata.name}']);
  return out;
}

function readyPodCount(selector) {
  // Count pods whose Ready condition is True.
  const out = kubectlOut([
    'get',
    'pods',
    '-l',
    selector,
    '-o',
    'jsonpath={range .items[*]}{range @.status.conditions[?(@.type=="Ready")]}{.status}{"\\n"}{end}{end}',
  ]);
  if (!out) return 0;
  return out.split('\n').filter((s) => s.trim() === 'True').length;
}

function restartSum(selector) {
  const out = kubectlOut([
    'get',
    'pods',
    '-l',
    selector,
    '-o',
    'jsonpath={range .items[*]}{.status.containerStatuses[0].restartCount}{"\\n"}{end}',
  ]);
  if (!out) return 0;
  return out
    .split('\n')
    .map((s) => parseInt(s.trim(), 10))
    .filter((n) => Number.isFinite(n))
    .reduce((a, b) => a + b, 0);
}

// ---- bounded poll ----------------------------------------------------

// pollUntil — invoke `fn` (async, returns truthy on success) every
// intervalMs until it succeeds or deadlineMs elapses. Returns true on
// success, false on timeout. The ASSERT-side retry primitive: faults are
// one-shot + idempotent, recovery checks retry to their deadline.
async function pollUntil(fn, { deadlineMs, intervalMs = POLL_INTERVAL_MS, label = '' } = {}) {
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

// ---- shared assertions -----------------------------------------------

// assertHeadsHealthy — poll until all 3 heads return 200. The canonical
// post-recovery green gate.
async function assertHeadsHealthy(deadlineMs) {
  return pollUntil(
    async () => {
      for (const head of HEAD_PROBES) {
        const r = await httpGet(head.path);
        if (r.status !== 200) return false;
      }
      return true;
    },
    { deadlineMs, label: 'heads-healthy' },
  );
}

async function assertReadyzGreen(deadlineMs) {
  return pollUntil(
    async () => {
      const r = await httpGet('/readyz');
      return r.status === 200;
    },
    { deadlineMs, label: 'readyz-green' },
  );
}

// queryBreakerMetric — read a cerberus self-metric back through the Prom
// head. POST-recovery corroboration ONLY (OTLP flush lags). Returns the
// latest scalar value, or null if the series isn't present yet.
async function queryBreakerMetric(metric) {
  const r = await httpGet('/api/v1/query?query=' + encodeURIComponent(metric));
  if (r.status !== 200) return null;
  try {
    const data = JSON.parse(r.body)?.data?.result;
    if (!Array.isArray(data) || data.length === 0) return null;
    // Take the max across series (breaker_state is a single gauge;
    // trips_total a single counter — but be defensive on multi-series).
    let best = null;
    for (const series of data) {
      const v = parseFloat(series?.value?.[1]);
      if (Number.isFinite(v)) best = best === null ? v : Math.max(best, v);
    }
    return best;
  } catch {
    return null;
  }
}

// driveBreakerClosed — after CH recovers, the main query breaker is in
// HALF-OPEN until a SUCCESSFUL CH-touching request flows through it and closes
// it (breaker.go record(): stateHalfOpen + err==nil -> stateClosed). This
// fires a burst of successful prom-head queries (each a real CH round-trip
// through the main breaker) on every poll tick and polls cerberus_ch_breaker_state
// until it reads CLOSED (0) or the deadline elapses. Returns true once CLOSED,
// false on timeout. It is a DRIVE, not a tolerance: callers still bindingly
// assert the gauge reads CLOSED afterwards.
async function driveBreakerClosed(deadlineMs) {
  return pollUntil(
    async () => {
      // Fan out successful CH-touching queries so every replica's main breaker
      // gets a HALF-OPEN probe + a closing success (with >=2 replicas the
      // Service round-robins, so a single query may miss a still-HALF-OPEN
      // replica). A non-200 here is fine — it just means we drive again next
      // tick; the close only lands once a probe succeeds.
      for (let i = 0; i < BREAKER_CLOSE_DRIVE_QUERIES; i += 1) {
        await httpGet('/api/v1/query?query=up', { timeoutMs: 10_000 });
      }
      const state = await queryBreakerMetric('cerberus_ch_breaker_state');
      // null = gauge not yet flushed (OTLP lag); keep driving until it reads a
      // concrete CLOSED. We must not clear on null here — the binding assert
      // that follows tolerates null, but the DRIVE wants a real CLOSED read.
      return state === BREAKER_STATE_CLOSED;
    },
    { deadlineMs, intervalMs: SETTLE_INTERVAL_MS, label: 'breaker-close' },
  );
}

// ---- scenario: ch-pod-kill -------------------------------------------
// CH outage -> shared breaker OPEN -> 503 every head, /readyz red,
// /healthz green (NO restart), auto-recover after CH recreate.
async function scenarioChPodKill() {
  const failures = [];

  log('  fault: kubectl delete pod -l app=clickhouse (Recreate -> clean outage)');
  const baselineRestarts = restartSum('app=cerberus');
  const del = kubectl(['delete', 'pod', '-l', 'app=clickhouse', '--wait=false']);
  if (del.status !== 0) {
    failures.push(`could not delete clickhouse pod: ${del.stderr.trim()}`);
    return failures;
  }

  // During-fault: drive >=THRESHOLD quick queries in a tight loop to force
  // the shared breaker OPEN. Accept the documented 502-then-503 ordering;
  // assert EVENTUAL 503 + Retry-After:5 within a bounded poll.
  log('  driving query volume to force breaker OPEN (accept 502-then-503)...');
  const tripped = await pollUntil(
    async () => {
      // Burst several queries per poll tick to accumulate failures inside
      // the breaker window.
      let last = null;
      for (let i = 0; i < 5; i += 1) {
        last = await httpGet('/api/v1/query?query=up', { timeoutMs: 12_000 });
      }
      if (last && last.status === 503) {
        const retry = last.headers['retry-after'];
        const et = jsonField(last.body, 'errorType');
        if (retry === '5' || et === 'unavailable') return true;
      }
      return false;
    },
    { deadlineMs: BREAKER_TRIP_DEADLINE_MS, label: 'breaker-trip' },
  );
  if (!tripped) {
    failures.push('breaker did not trip to 503+Retry-After:5 within budget during CH outage');
  } else {
    log('    breaker OPEN: 503 + Retry-After:5 observed');
  }

  // /readyz red with 'circuit' substring; /healthz stays 200.
  const readyzRed = await pollUntil(
    async () => {
      const r = await httpGet('/readyz');
      return r.status === 503 && r.body.toLowerCase().includes('circuit');
    },
    { deadlineMs: BREAKER_TRIP_DEADLINE_MS, label: 'readyz-circuit' },
  );
  if (!readyzRed) {
    failures.push("/readyz did not go 503 with body containing 'circuit' during CH outage");
  } else {
    log("    /readyz 503 with 'circuit' in body");
  }

  const health = await httpGet('/healthz');
  if (health.status !== 200) {
    failures.push(`/healthz must stay 200 on CH outage (liveness independent of breaker); got ${health.status}`);
  } else {
    log('    /healthz stayed 200 (no restart-on-CH-outage)');
  }

  // Heal: CH Deployment auto-recreates. Wait for rollout Available.
  log('  heal: waiting for clickhouse rollout Available + breaker close...');
  kubectl(['rollout', 'status', 'deploy/clickhouse', '--timeout=90s']);

  const recovered = await assertReadyzGreen(CH_RECOVERY_DEADLINE_MS);
  if (!recovered) {
    failures.push('/readyz did not return to 200 within CH recovery budget after heal');
  } else {
    log('    /readyz back to 200');
  }

  const headsOk = await assertHeadsHealthy(CH_RECOVERY_DEADLINE_MS);
  if (!headsOk) {
    failures.push('not all 3 heads returned 200 after CH recovery');
  } else {
    log('    all 3 heads 200');
  }

  // Drive the main query breaker HALF-OPEN -> CLOSED before asserting. CH is
  // back, but the breaker only closes when a SUCCESSFUL CH-touching query
  // flows through it (breaker.go: stateHalfOpen + err==nil -> stateClosed).
  // /readyz green + a single heads sweep don't guarantee that across >=2
  // replicas, so push sustained successful prom-head queries (real CH
  // round-trips through the main breaker) and poll the gauge until CLOSED. This
  // closes the legitimate transition so the post-recovery assertion below — and
  // every later scenario that reads cerberus_ch_breaker_state — sees a genuinely
  // CLOSED breaker, with NO assertion weakened.
  log('  driving main breaker HALF-OPEN -> CLOSED with successful CH queries...');
  const breakerClosed = await driveBreakerClosed(BREAKER_CLOSE_DEADLINE_MS);
  if (!breakerClosed) {
    failures.push(
      'main breaker did not return to CLOSED after CH recovery despite sustained successful CH queries — HALF-OPEN -> CLOSED probe never closed (would mean a real breaker bug, not OTLP lag)',
    );
  } else {
    log('    main breaker driven to CLOSED (cerberus_ch_breaker_state == 0)');
  }

  // Post-recovery metric corroboration (settle poll): trips >= 1, state == 0.
  // This is BEST-EFFORT corroboration, not the binding trip signal — the
  // breaker trip is already bindingly asserted DURING the fault via the
  // 503 + Retry-After:5 HTTP path above. The self-metric flows OTLP ->
  // collector -> CH -> Prom head, so it rides the very CH the outage knocked
  // out: CH now persists its data across the pod-kill (a PVC backs
  // /var/lib/clickhouse — test/e2e/k3s/clickhouse.yaml), so the trip counter
  // written mid-outage survives, but it can still be lagging the OTLP flush
  // right after recovery — surfacing transiently as a NULL series (metric
  // absent), not a 0. We therefore tolerate NULL (absent, OTLP-lag — same
  // tolerance the sibling state check already applies) but still FAIL if the
  // series IS present and reads < 1, which would mean the trip genuinely
  // wasn't recorded despite CH being queryable.
  log('  corroborating breaker self-metrics via Prom head (settle poll)...');
  let tripsSeen = null;
  await pollUntil(
    async () => {
      const trips = await queryBreakerMetric('cerberus_ch_breaker_trips_total');
      if (trips !== null) tripsSeen = trips;
      return trips !== null && trips >= 1;
    },
    { deadlineMs: METRIC_SETTLE_DEADLINE_MS, intervalMs: SETTLE_INTERVAL_MS, label: 'trips>=1' },
  );
  if (tripsSeen !== null && tripsSeen < 1) {
    failures.push(`cerberus_ch_breaker_trips_total present but < 1 (${tripsSeen}) — breaker trip not recorded despite CH being queryable`);
  } else {
    log(`    cerberus_ch_breaker_trips_total == ${tripsSeen === null ? '(absent; OTLP lag, tolerated — trip already asserted via 503)' : tripsSeen}`);
  }
  const state = await queryBreakerMetric('cerberus_ch_breaker_state');
  if (state !== null && state !== BREAKER_STATE_CLOSED) {
    failures.push(`cerberus_ch_breaker_state should be ${BREAKER_STATE_CLOSED} (CLOSED) after recovery; got ${state} (${breakerStateName(state)})`);
  } else {
    log(`    cerberus_ch_breaker_state == ${state === null ? '(pending, tolerated)' : `${state} (${breakerStateName(state)})`}`);
  }

  // CH outage must NEVER restart cerberus.
  const restartsNow = restartSum('app=cerberus');
  if (restartsNow > baselineRestarts) {
    failures.push(
      `cerberus restarted during CH outage (baseline ${baselineRestarts} -> ${restartsNow}); a CH outage must drain /readyz, never restart the pod`,
    );
  } else {
    log(`    cerberus restartCount unchanged (${restartsNow}) — no restart-on-CH-outage`);
  }

  return failures;
}

// ---- scenario: ch-slow / query-timeout -------------------------------
// A slow query -> clean 503 errorType=timeout at the cap, breaker-NEUTRAL
// (no trip, no 503 on unrelated heads), /readyz stays 200, slot+conn
// released.
async function scenarioChSlowTimeout() {
  const failures = [];

  // Baseline breaker state before the slow burst.
  const baselineTrips = await queryBreakerMetric('cerberus_ch_breaker_trips_total');

  // A heavy query_range whose cost comes from COMPUTE PER ANCHOR (a nested
  // subquery fanned out at every outer anchor), not from anchor count — so
  // it stays under the 11000-point resolution cap (which would 400 before
  // the timeout arms) yet blows past the small, calibrated
  // CERBERUS_QUERY_TIMEOUT (250ms) the chaos overlay set. The query is an
  // UNCOLLAPSIBLE stddev_over_time (a per-anchor dispersion that can't fold
  // to a constant), so its cost is real CH compute, not optimizer-foldable.
  // See slowQueryPath / the SLOW_QUERY_* constants.
  // ?timeout= mins with the cap; we lean on the configured cap.
  const slowPath = slowQueryPath();

  log('  fault: issuing a deliberately slow query_range (nested subquery, heavy per-anchor compute)...');
  const slow = await httpGet(slowPath, { timeoutMs: 30_000 });
  if (slow.status !== 503) {
    failures.push(`slow query should return 503 at the wall-clock cap; got ${slow.status} (body=${slow.body.slice(0, 200)})`);
  } else {
    const et = jsonField(slow.body, 'errorType');
    if (et !== 'timeout') {
      failures.push(`slow query 503 errorType should be 'timeout'; got '${et}'`);
    } else {
      log("    slow query -> 503 errorType=timeout (clean wall-clock cap)");
    }
  }

  // /readyz STAYS 200 (the 1s ping is not a slow data-plane query).
  const ready = await httpGet('/readyz');
  if (ready.status !== 200) {
    failures.push(`/readyz must stay 200 during a slow data-plane query; got ${ready.status}`);
  } else {
    log('    /readyz stayed 200');
  }

  // A separate FAST query still 200 (heads stay healthy; slot+conn released).
  const fast = await httpGet('/api/v1/query?query=up');
  if (fast.status !== 200) {
    failures.push(`a fast /api/v1/query?query=up must still 200 after the slow query (slot+conn released); got ${fast.status}`);
  } else {
    log('    fast query still 200 (admit slot + pooled conn released)');
  }

  // Burst of slow queries must NOT trip the breaker.
  log('  bursting slow queries to confirm breaker stays CLOSED (breaker-neutral)...');
  await Promise.all(
    Array.from({ length: 4 }, () => httpGet(slowPath, { timeoutMs: 30_000 })),
  );

  // Heads must stay 200 (unrelated heads not 503'd by the timeout burst).
  for (const head of HEAD_PROBES) {
    const r = await httpGet(head.path);
    if (r.status === 503 && (r.headers['retry-after'] === '5' || jsonField(r.body, 'errorType') === 'unavailable')) {
      failures.push(`head ${head.name} returned breaker-503 after a slow-query burst — timeout must be breaker-neutral`);
    }
  }

  // Settle, then corroborate breaker state==0 + trips unchanged.
  await sleep(SETTLE_INTERVAL_MS);
  const stateOk = await pollUntil(
    async () => {
      const state = await queryBreakerMetric('cerberus_ch_breaker_state');
      return state === 0 || state === null;
    },
    { deadlineMs: METRIC_SETTLE_DEADLINE_MS, intervalMs: SETTLE_INTERVAL_MS, label: 'state==0' },
  );
  if (!stateOk) {
    failures.push('cerberus_ch_breaker_state != 0 after slow-query burst — timeout must not trip the breaker');
  } else {
    log('    cerberus_ch_breaker_state == 0 (breaker stayed CLOSED through slow burst)');
  }
  const tripsAfter = await queryBreakerMetric('cerberus_ch_breaker_trips_total');
  if (baselineTrips !== null && tripsAfter !== null && tripsAfter > baselineTrips) {
    failures.push(`cerberus_ch_breaker_trips_total climbed (${baselineTrips} -> ${tripsAfter}) during slow-query burst — timeout must be breaker-neutral`);
  } else {
    log(`    cerberus_ch_breaker_trips_total unchanged (${baselineTrips} -> ${tripsAfter})`);
  }

  return failures;
}

// ---- scenario: cerberus-pod-kill -------------------------------------
// Kill ONE of the >=2 HPA-floor replicas; Service keeps serving from the
// survivor; aggregate success stays high; replacement rejoins.
async function scenarioCerberusPodKill() {
  const failures = [];

  const target = firstPodName('app=cerberus');
  if (!target) {
    failures.push('no cerberus pod found to kill (HPA floor should be >=2)');
    return failures;
  }
  const survivorBaselineRestarts = restartSum('app=cerberus');

  log(`  fault: kubectl delete pod ${target} (one of >=2 replicas, --wait=false)`);

  // Steady low-rate request loop spanning the kill; measure AGGREGATE
  // success (>=95%, retry a connection-reset ONCE). A request landing
  // mid-drain on the dying pod may reset — acceptable, not a failure.
  let total = 0;
  let success = 0;
  const loopDeadline = Date.now() + 25_000;
  const killOnce = (async () => {
    // Kick the delete shortly after the loop starts so it spans the kill.
    await sleep(2_000);
    kubectl(['delete', 'pod', target, '--wait=false']);
    log('    delete issued mid-loop');
  })();

  while (Date.now() < loopDeadline) {
    total += 1;
    let r = await httpGet('/api/v1/query?query=up', { timeoutMs: 8_000 });
    if (r.status === 0 || r.status >= 500) {
      // One retry on a connection reset / transient 5xx (mid-drain).
      r = await httpGet('/api/v1/query?query=up', { timeoutMs: 8_000 });
    }
    if (r.status === 200) success += 1;
    await sleep(500);
  }
  await killOnce;

  const rate = total === 0 ? 0 : success / total;
  log(`    aggregate success ${success}/${total} = ${(rate * 100).toFixed(1)}%`);
  const SUCCESS_FLOOR = 0.95;
  if (rate < SUCCESS_FLOOR) {
    failures.push(`aggregate success rate ${(rate * 100).toFixed(1)}% < ${SUCCESS_FLOOR * 100}% through a single-replica kill — the survivor should keep serving`);
  } else {
    log(`    aggregate success >= ${SUCCESS_FLOOR * 100}% (survivor served through the kill)`);
  }

  // Recovery: >=2 Ready + rollout Available.
  log('  heal: waiting for >=2 cerberus replicas Ready + rollout Available...');
  kubectl(['rollout', 'status', 'deploy/cerberus', '--timeout=90s']);
  const twoReady = await pollUntil(
    async () => readyPodCount('app=cerberus') >= 2,
    { deadlineMs: CERBERUS_RECOVERY_DEADLINE_MS, label: '>=2-ready' },
  );
  if (!twoReady) {
    failures.push('cerberus did not return to >=2 Ready replicas within recovery budget');
  } else {
    log('    >=2 cerberus replicas Ready');
  }

  const headsOk = await assertHeadsHealthy(CERBERUS_RECOVERY_DEADLINE_MS);
  if (!headsOk) {
    failures.push('not all 3 heads returned 200 after cerberus replica recovery');
  } else {
    log('    all 3 heads 200');
  }

  // Chaos-aware restart guard: the KILLED pod is replaced (not restarted
  // in place), so a blanket restartCount==0 across the deployment is wrong
  // here. Guard only against an UNEXPECTED CrashLoop — the new pod set's
  // restart sum should not exceed the pre-kill survivor baseline by more
  // than the single deleted pod could contribute (0; a replacement starts
  // fresh at 0). We assert no restart INCREASE on the surviving set.
  const restartsNow = restartSum('app=cerberus');
  if (restartsNow > survivorBaselineRestarts) {
    failures.push(
      `cerberus restart sum increased (${survivorBaselineRestarts} -> ${restartsNow}) after a single-replica kill — the replacement should start clean, not CrashLoop`,
    );
  } else {
    log(`    no unexpected restarts on the replica set (${restartsNow})`);
  }

  return failures;
}

// ---- scenario: ch-network-partition (phase-2) ------------------------
// Deny-egress NetworkPolicy blackholes cerberus->CH. Gated on a
// kube-router enforcement probe: if NetworkPolicy is not enforced, the
// scenario is recorded not-applicable (::notice::) rather than passing
// vacuously. Same breaker end-state as ch-pod-kill, slower path to trip.
async function scenarioChNetworkPartition() {
  const failures = [];

  // PREREQUISITE GATE: confirm kube-router enforces a deny-egress policy.
  log('  prerequisite: probing NetworkPolicy enforcement (kube-router)...');
  const enforced = await probeNetpolEnforcement();
  if (enforced === null) {
    // Probe itself errored — treat as not-applicable, not a hard fail, so
    // an infra hiccup in the probe doesn't red the informational lane.
    notice(
      'ch-network-partition: NetworkPolicy enforcement probe inconclusive — recording not-applicable; breaker contract is covered by ch-pod-kill.',
    );
    return failures;
  }
  if (!enforced) {
    notice(
      'ch-network-partition: kube-router is NOT enforcing NetworkPolicy in this k3d image — recording not-applicable; breaker contract is covered by ch-pod-kill.',
    );
    return failures;
  }
  log('    NetworkPolicy IS enforced — running the partition scenario');

  const policy = `${MANIFESTS}/deny-egress-clickhouse.yaml`;
  log('  fault: applying deny-egress NetworkPolicy (blackhole cerberus->CH)...');
  const apply = capture('kubectl', ['-n', NS, 'apply', '-f', policy]);
  if (apply.status !== 0) {
    failures.push(`could not apply partition policy: ${apply.stderr.trim()}`);
    return failures;
  }

  try {
    // EVENTUAL trip — slower path (each dial blocks up to DialTimeout).
    log('  driving query volume to force EVENTUAL breaker OPEN (slow path)...');
    let everHung = false;
    const tripped = await pollUntil(
      async () => {
        let last = null;
        for (let i = 0; i < 3; i += 1) {
          last = await httpGet('/api/v1/query?query=up', { timeoutMs: 20_000 });
          // Every response must be a 5xx within the cap — never an
          // indefinite hang (timeoutMs above + the query-timeout cap bound
          // it). A transport timeout (status 0 + abort) IS the hang signal.
          if (last.status === 0) everHung = true;
        }
        if (last && last.status === 503) {
          const retry = last.headers['retry-after'];
          const et = jsonField(last.body, 'errorType');
          if (retry === '5' || et === 'unavailable') return true;
        }
        return false;
      },
      { deadlineMs: PARTITION_TRIP_DEADLINE_MS, label: 'partition-trip' },
    );
    if (everHung) {
      failures.push('a request hung past the client/query-timeout cap during the partition — must always return a 5xx, never an indefinite hang');
    }
    if (!tripped) {
      failures.push('breaker did not eventually trip to 503+Retry-After:5 under the network partition within the generous budget');
    } else {
      log('    breaker OPEN under partition: 503 + Retry-After:5');
    }

    const readyzRed = await pollUntil(
      async () => {
        const r = await httpGet('/readyz');
        return r.status === 503;
      },
      { deadlineMs: PARTITION_TRIP_DEADLINE_MS, label: 'readyz-red' },
    );
    if (!readyzRed) failures.push('/readyz did not go 503 under the network partition');
    else log('    /readyz 503');

    const health = await httpGet('/healthz');
    if (health.status !== 200) failures.push(`/healthz must stay 200 under partition; got ${health.status}`);
    else log('    /healthz stayed 200');
  } finally {
    // Heal: remove the policy regardless of outcome.
    log('  heal: removing partition NetworkPolicy...');
    capture('kubectl', ['-n', NS, 'delete', '-f', policy, '--ignore-not-found']);
  }

  const recovered = await assertReadyzGreen(CH_RECOVERY_DEADLINE_MS);
  if (!recovered) failures.push('/readyz did not return to 200 after the partition healed');
  else log('    /readyz back to 200');

  const headsOk = await assertHeadsHealthy(CH_RECOVERY_DEADLINE_MS);
  if (!headsOk) failures.push('not all 3 heads returned 200 after the partition healed');
  else log('    all 3 heads 200');

  const state = await pollUntil(
    async () => (await queryBreakerMetric('cerberus_ch_breaker_state')) === 0,
    { deadlineMs: METRIC_SETTLE_DEADLINE_MS, intervalMs: SETTLE_INTERVAL_MS, label: 'state==0' },
  );
  if (!state) {
    // state==0 corroboration is best-effort (OTLP lag); the HTTP-green
    // gate above is the binding signal. Note rather than fail.
    notice('ch-network-partition: cerberus_ch_breaker_state did not read back 0 within settle budget (OTLP lag) — HTTP-green recovery already asserted.');
  } else {
    log('    cerberus_ch_breaker_state == 0');
  }

  return failures;
}

// probeNetpolEnforcement — apply a deny-all-egress policy + a throwaway
// probe pod, dial CH from inside it. Returns true if the dial is BLOCKED
// (enforcement present), false if it SUCCEEDS (enforcement absent), null
// on an inconclusive infra error. Cleans up both regardless.
async function probeNetpolEnforcement() {
  const probePolicy = `${MANIFESTS}/netpol-enforcement-probe.yaml`;
  const POD = 'chaos-netpol-probe';
  // Cleanup any stale probe artifacts first.
  capture('kubectl', ['-n', NS, 'delete', 'pod', POD, '--ignore-not-found', '--wait=false']);
  capture('kubectl', ['-n', NS, 'delete', '-f', probePolicy, '--ignore-not-found']);

  const apply = capture('kubectl', ['-n', NS, 'apply', '-f', probePolicy]);
  if (apply.status !== 0) return null;

  // Launch a probe pod labelled app=chaos-netpol-probe that tries to dial
  // CH once. busybox `nc -w` returns non-zero on a blocked/timed-out dial.
  const run = capture('kubectl', [
    '-n',
    NS,
    'run',
    POD,
    '--image=busybox:1.36',
    '--restart=Never',
    '--labels=app=chaos-netpol-probe',
    '--command',
    '--',
    'sh',
    '-c',
    'nc -w 5 -z clickhouse 9000 && echo REACHABLE || echo BLOCKED',
  ]);
  let result = null;
  try {
    if (run.status !== 0) return null;
    // Wait for the pod to complete, then read its logs.
    const done = await pollUntil(
      async () => {
        const phase = kubectlOut(['get', 'pod', POD, '-o', 'jsonpath={.status.phase}']);
        return phase === 'Succeeded' || phase === 'Failed';
      },
      { deadlineMs: 30_000, label: 'netpol-probe-pod' },
    );
    if (!done) return null;
    const logs = kubectlOut(['logs', POD]);
    if (logs.includes('BLOCKED')) result = true;
    else if (logs.includes('REACHABLE')) result = false;
    else result = null;
  } finally {
    capture('kubectl', ['-n', NS, 'delete', 'pod', POD, '--ignore-not-found', '--wait=false']);
    capture('kubectl', ['-n', NS, 'delete', '-f', probePolicy, '--ignore-not-found']);
  }
  return result;
}

// ---- scenario: load / admit-saturation (phase-2) ---------------------
// Concurrency burst beyond the (small overlay) admit cap -> clean shed:
// some 503 + Retry-After:1 'saturated', a concurrent below-cap request
// still 200, breaker stays CLOSED, /readyz green.
async function scenarioLoadAdmitSaturation() {
  const failures = [];

  const baselineState = await queryBreakerMetric('cerberus_ch_breaker_state');

  // Fire N concurrent requests > the (overlay) admit cap. Use the same
  // heavy nested-subquery query (slowQueryPath) so each request holds its
  // admit slot long enough to overlap — and so it passes the 11000-point
  // resolution cap instead of 400'ing before it can occupy a slot.
  const slowPath = slowQueryPath();

  const N = 16; // >> CERBERUS_ADMIT_PROM (2) in the chaos overlay
  log(`  fault: firing ${N} concurrent over-cap requests + one below-cap probe...`);
  const burst = Promise.all(Array.from({ length: N }, () => httpGet(slowPath, { timeoutMs: 30_000 })));

  // Concurrently, a below-cap fast request should still get through.
  await sleep(200); // let the burst occupy the slots first
  const probe = await httpGet('/api/v1/query?query=up', { timeoutMs: 10_000 });

  const results = await burst;
  const shed = results.filter(
    (r) => r.status === 503 && r.headers['retry-after'] === '1' && r.body.toLowerCase().includes('saturated'),
  );
  if (shed.length === 0) {
    failures.push(`expected SOME of ${N} over-cap requests to shed with 503 + Retry-After:1 'saturated'; none did`);
  } else {
    log(`    ${shed.length}/${N} requests shed cleanly (503 + Retry-After:1 'saturated')`);
  }

  // The below-cap probe is allowed to be admitted-and-served (200) OR, if
  // it raced into the saturated window, shed cleanly — what it must NOT do
  // is hang or 5xx-internal. Accept 200 or a clean 503-saturated.
  const probeClean =
    probe.status === 200 ||
    (probe.status === 503 && probe.body.toLowerCase().includes('saturated'));
  if (!probeClean) {
    failures.push(`below-cap probe neither 200 nor cleanly shed; got ${probe.status} (body=${probe.body.slice(0, 160)})`);
  } else {
    log(`    below-cap probe handled cleanly (status ${probe.status})`);
  }

  // /readyz stays green throughout.
  const ready = await httpGet('/readyz');
  if (ready.status !== 200) {
    failures.push(`/readyz must stay 200 under admit saturation; got ${ready.status}`);
  } else {
    log('    /readyz stayed 200');
  }

  // Settle, then corroborate: admit_rejected climbed, breaker still CLOSED.
  await sleep(SETTLE_INTERVAL_MS);
  const rejectedClimbed = await pollUntil(
    async () => {
      const v = await queryBreakerMetric('cerberus_admit_rejected_total');
      return v !== null && v >= 1;
    },
    { deadlineMs: METRIC_SETTLE_DEADLINE_MS, intervalMs: SETTLE_INTERVAL_MS, label: 'admit_rejected>=1' },
  );
  if (!rejectedClimbed) {
    // OTLP lag may delay the counter; the during-fault 503-saturated count
    // above is the binding signal. Note rather than fail.
    notice('load/admit-saturation: cerberus_admit_rejected_total did not read back >=1 within settle budget (OTLP lag) — the during-fault 503-saturated shed was already asserted.');
  } else {
    log('    cerberus_admit_rejected_total >= 1');
  }
  const stateAfter = await queryBreakerMetric('cerberus_ch_breaker_state');
  if (stateAfter !== null && stateAfter !== BREAKER_STATE_CLOSED) {
    // The gauge encodes closed=0 / open=1 / half-open=2 (breaker_metrics.go).
    // Admit/pool rejections shed at the admission layer and never reach the
    // breaker, so the only correct steady state here is CLOSED (0). A 2
    // (HALF-OPEN) or 1 (OPEN) would mean a saturation shed leaked into the
    // breaker — name the observed state so a future failure reads truthfully
    // instead of the misleading bare "got 2".
    failures.push(
      `cerberus_ch_breaker_state must stay ${BREAKER_STATE_CLOSED} (CLOSED) under admit saturation — admit/pool rejections are breaker-neutral; got ${stateAfter} (${breakerStateName(stateAfter)})`,
    );
  } else {
    log(`    cerberus_ch_breaker_state == ${stateAfter === null ? `(pending; baseline ${baselineState})` : `${stateAfter} (${breakerStateName(stateAfter)})`} (breaker CLOSED)`);
  }

  // After load drops, all heads 200.
  const headsOk = await assertHeadsHealthy(CERBERUS_RECOVERY_DEADLINE_MS);
  if (!headsOk) failures.push('not all 3 heads returned 200 after the load dropped');
  else log('    all 3 heads 200 after load drop');

  return failures;
}

// ---- passive end-of-run health gate (handler-panic corroboration) ----
// Panic-envelope correctness is covered DETERMINISTICALLY by Layer 10 unit
// chaos. Here we only corroborate the process recovered cleanly from the
// cumulative fault storm: all 3 heads 200, no lingering 5xx.
async function endOfRunHealthGate() {
  const failures = [];
  log('  passive gate: asserting steady-state recovery after the fault storm...');
  const headsOk = await assertHeadsHealthy(CERBERUS_RECOVERY_DEADLINE_MS);
  if (!headsOk) {
    failures.push('end-of-run: not all 3 heads recovered to 200 after the cumulative fault load');
  } else {
    log('    all 3 heads 200 — process recovered cleanly');
  }
  const ready = await assertReadyzGreen(CH_RECOVERY_DEADLINE_MS);
  if (!ready) failures.push('end-of-run: /readyz did not settle to 200');
  else log('    /readyz 200');
  return failures;
}

// ---- scenario registry + driver --------------------------------------

// `recreatesCh: true` marks a scenario that deletes the ClickHouse pod. CH now
// backs /var/lib/clickhouse with a PersistentVolumeClaim
// (test/e2e/k3s/clickhouse.yaml), so the recreated pod comes back WITH its
// schema + data INTACT — the pod-kill is the realistic "CH briefly away, then
// back with its data" outage the breaker assertions expect, NOT an empty-table
// wipe. The heal gate after such a scenario still runs a one-shot
// `just e2e-reseed` through a FRESH port-forward, but only to RE-ANCHOR the
// rolling time-window at wall-clock now (the seeder's INSERTs re-anchor on
// now64(9)); it is no longer restoring lost tables.
//
// This one-shot is COMPLEMENTARY to the rolling seeder. As of the
// reconnecting-supervisor fix (test/e2e/seed/port_forward_supervisor.sh) the
// rolling seeder's port-forward respawns once CH is recreated, so the 30 s
// rolling feed resumes on its own and keeps the data anchored at wall-clock now
// for the time-windowed assertions of the scenarios that follow. The one-shot
// closes the freshness gap between CH coming back and the next rolling tick
// landing; the PVC guarantees the schema + historical data are already there.
//
// `destructive: true` marks a scenario that injects a POD-KILL fault (CH pod or
// a cerberus replica). The run loop sorts every selected pool so NON-destructive
// scenarios run FIRST and destructive ones LAST (see orderScenarios). This is
// load-bearing for ch-slow-query-timeout: that scenario's "deliberately slow"
// nested-subquery query is DATA-DRIVEN (its inner rate() evals scan the seeded
// counter), so it only reliably exceeds the calibrated CERBERUS_QUERY_TIMEOUT
// — and thus returns the contracted 503 — when it runs against the FULL
// rolling-seeded window. Running it BEFORE the destructive ch-pod-kill (whose recreate leaves a
// thin post-reseed window right after the one-shot re-anchor) keeps the query
// cost-dominated. As a bonus, load-admit-saturation's "breaker stays CLOSED"
// assertion then runs on a naturally-closed breaker (no prior outage), and each
// destructive scenario re-establishes its own preconditions via the heal gate.
const PHASE1 = [
  // Non-destructive, data-dependent: the slow-query timeout contract. Runs on
  // the full rolling-seeded window so the heavy nested subquery reliably blows
  // the calibrated wall-clock cap.
  { name: 'ch-slow-query-timeout', run: scenarioChSlowTimeout },
  // Destructive pod-kills, sorted to run after the non-destructive set.
  { name: 'ch-pod-kill', run: scenarioChPodKill, recreatesCh: true, destructive: true },
  { name: 'cerberus-pod-kill', run: scenarioCerberusPodKill, destructive: true },
];

const PHASE2 = [
  // load-admit-saturation is non-destructive (admission-layer shed only) and
  // wants a naturally-CLOSED breaker, so it sorts into the non-destructive head
  // of the run alongside ch-slow-query-timeout.
  { name: 'load-admit-saturation', run: scenarioLoadAdmitSaturation },
  // ch-network-partition only blackholes egress with a NetworkPolicy; it never
  // deletes the CH pod, so CH data survives — no re-seed needed after it. It is
  // non-destructive in the pod-kill sense, but it DOES trip the breaker OPEN, so
  // it sorts after the breaker-neutral non-destructive scenarios yet before the
  // pod-kills. (On the k3d image where kube-router does not enforce
  // NetworkPolicy this scenario records not-applicable; see
  // scenarioChNetworkPartition.)
  { name: 'ch-network-partition', run: scenarioChNetworkPartition },
];

// orderScenarios — stable-sort a selected pool so NON-destructive scenarios run
// before destructive (pod-kill) ones. Stable: authored order is preserved within
// each group, so the non-destructive head stays [slow-query, load-admit,
// network-partition] and the destructive tail stays [ch-pod-kill,
// cerberus-pod-kill]. The ch-pod-kill recreate (and its thin post-reseed window)
// therefore always lands AFTER the data-dependent slow-query has run against the
// full rolling-seeded window.
function orderScenarios(pool) {
  const rank = (s) => (s.destructive ? 1 : 0);
  return pool
    .map((s, i) => ({ s, i }))
    .sort((a, b) => rank(a.s) - rank(b.s) || a.i - b.i)
    .map((e) => e.s);
}

function selectedScenarios() {
  let pool = PHASE === 'all' ? [...PHASE1, ...PHASE2] : [...PHASE1];
  if (ONLY.length > 0) pool = pool.filter((s) => ONLY.includes(s.name));
  return orderScenarios(pool);
}

// PREFLIGHT_DEADLINE_MS bounds both the initial preflight gate and the
// between-scenario heal gate: how long to wait for /readyz + all heads green
// before declaring the stack (still) healthy.
const PREFLIGHT_DEADLINE_MS = 60_000;

async function assertStackGreen(deadlineMs) {
  const ready = await assertReadyzGreen(deadlineMs);
  if (!ready) return false;
  return assertHeadsHealthy(deadlineMs);
}

async function preflight() {
  // The whole lane assumes a healthy stack from `just e2e-up` + seed +
  // wait-otel. Confirm green before injecting the first fault so a
  // bring-up problem doesn't masquerade as a chaos failure.
  log('preflight: asserting the stack is green before fault injection...');
  if (!(await assertStackGreen(PREFLIGHT_DEADLINE_MS))) {
    error('preflight: /readyz + all heads never reached 200 — the stack is not healthy; aborting before any fault injection');
    return false;
  }
  log('preflight OK: /readyz 200 + all heads 200');
  return true;
}

// healBetweenScenarios re-establishes the green-stack precondition each
// scenario assumes. The lane is sequential and a CH-recreating scenario tears
// the CH pod down (Recreate, but its /var/lib/clickhouse PVC survives), so the
// NEXT scenario must not start until /readyz + all heads are green again AND
// the data plane is queryable + freshly anchored — otherwise a still-recovering
// stack from the prior fault masquerades as the next scenario's failure (e.g.
// cerberus-pod-kill's aggregate-success loop running before CH is back, or
// ch-slow-query's nested subquery hitting a still-rolling-out CH). This
// implements the heal-between-each-scenario invariant the lane's header
// documents.
//
// CH's schema + historical data PERSIST across the pod-kill (PVC-backed data
// dir — test/e2e/k3s/clickhouse.yaml), so the recreated pod is never empty.
// After a `recreatesCh` scenario the heal gate still runs a one-shot
// `just e2e-reseed` (fresh port-forward, idempotent DDL + INSERTs) — now only
// to RE-ANCHOR the rolling time-window at wall-clock now (the seeder re-anchors
// on now64(9)), then waits for the `up` series to read back through the Prom
// head before clearing the gate. The rolling seeder's own port-forward respawns
// once CH is recreated (reconnecting supervisor, see PHASE1's comment) and
// resumes its 30 s feed; the one-shot closes the freshness gap until the next
// rolling tick lands. After a non-destructive scenario, CH stays up untouched
// and only a short settle window is needed.
const HEAL_SETTLE_MS = 15_000; // OTLP flush headroom for a non-destructive scenario
const RESEED_DEADLINE_MS = 120_000; // bound the `just e2e-reseed` one-shot (DDL + INSERTs + verify)
const RESEED_VISIBLE_DEADLINE_MS = 90_000; // wait for the re-seeded `up` series to read back via the Prom head

// reseedClickHouse runs the one-shot re-seed recipe (fresh port-forward,
// idempotent DDL + fixture INSERTs + rowcount verify). Returns true on a
// clean exit; logs + returns false otherwise. Bounded so a hung port-forward
// can't stall the lane past its budget.
function reseedClickHouse() {
  log('  heal: re-anchoring the rolling window on recreated (PVC-backed) ClickHouse via `just e2e-reseed`...');
  const res = capture('just', ['e2e-reseed'], { timeout: RESEED_DEADLINE_MS });
  if (res.stdout) log(res.stdout.trimEnd());
  if (res.status !== 0) {
    if (res.stderr) log(res.stderr.trimEnd());
    return false;
  }
  return true;
}

async function healBetweenScenarios(nextName, priorScenario) {
  log(`heal gate: waiting for the stack to return green before scenario ${nextName}...`);
  const green = await assertStackGreen(CH_RECOVERY_DEADLINE_MS);
  if (!green) {
    return `heal gate before ${nextName}: stack did not return to /readyz 200 + all heads 200 within the recovery budget after the prior scenario`;
  }

  if (priorScenario && priorScenario.recreatesCh) {
    // CH came back with its PVC-backed data intact — re-anchor the rolling
    // window at now before the next time-windowed scenario asserts.
    if (!reseedClickHouse()) {
      return `heal gate before ${nextName}: re-seed of the recreated ClickHouse failed; downstream scenarios would query a stale window`;
    }
    // Confirm the data is actually queryable + freshly anchored through the
    // Prom head before clearing the gate (a bare assertHeadsHealthy is not
    // enough — assert the `up` series reads back).
    const visible = await pollUntil(
      async () => {
        const r = await httpGet('/api/v1/query?query=up');
        if (r.status !== 200) return false;
        const result = jsonField(r.body, 'data')?.result;
        return Array.isArray(result) && result.length > 0;
      },
      { deadlineMs: RESEED_VISIBLE_DEADLINE_MS, label: 're-seed-visible' },
    );
    if (!visible) {
      return `heal gate before ${nextName}: re-seeded data did not become queryable (\`up\` series absent) within budget after CH recreation`;
    }
    log('heal gate: ClickHouse re-seeded + data visible through the Prom head');
    return null;
  }

  // Non-destructive prior scenario: CH data survived. A short data-plane
  // settle absorbs OTLP flush lag before the next scenario asserts.
  await sleep(HEAL_SETTLE_MS);
  log('heal gate: stack green + settled');
  return null;
}

async function main() {
  log(`live-stack chaos lane — phase=${PHASE} url=${CERBERUS_URL} ns=${NS}`);

  if (!(await preflight())) process.exit(1);

  const scenarios = selectedScenarios();
  if (scenarios.length === 0) {
    error('no scenarios selected — check CHAOS_PHASE / CHAOS_SCENARIOS');
    process.exit(1);
  }

  const failed = [];
  for (let i = 0; i < scenarios.length; i += 1) {
    const s = scenarios[i];
    // Between scenarios (not before the first — preflight already gated it),
    // re-establish the green-stack precondition each scenario assumes. A
    // prior CH-DESTRUCTIVE scenario leaves CH freshly recreated (ephemeral)
    // and EMPTY — the heal gate re-seeds it (the rolling seeder's tunnel died
    // with the killed pod). Without the re-seed, ch-pod-kill's CH wipe turned
    // into ch-slow-query / cerberus-pod-kill failures (a 502 fast-query, a
    // sub-floor aggregate-success loop) because every later query hit empty
    // tables. Pass the PRIOR scenario so the heal gate knows whether to
    // re-seed.
    if (i > 0) {
      const healFail = await healBetweenScenarios(s.name, scenarios[i - 1]);
      if (healFail) {
        error(healFail);
        failed.push(`heal-gate-before-${s.name}`);
      }
    }
    // gh.mjs's group() wraps a SYNC fn in try/finally, so it can't bracket
    // async work (::endgroup:: would fire before the promise resolves).
    // Emit the group markers manually around the awaited scenario instead.
    log(`::group::scenario: ${s.name}`);
    let resolved;
    try {
      resolved = await s.run();
    } finally {
      log('::endgroup::');
    }
    if (resolved.length > 0) {
      for (const f of resolved) error(`[${s.name}] ${f}`);
      failed.push(s.name);
    } else {
      notice(`scenario ${s.name}: all resilience contracts held`);
    }
  }

  // Passive end-of-run health gate after the fault storm.
  const gateFails = await endOfRunHealthGate();
  if (gateFails.length > 0) {
    for (const f of gateFails) error(f);
    failed.push('end-of-run-health-gate');
  }

  if (failed.length > 0) {
    error(`chaos lane FAILED: ${failed.join(', ')}`);
    process.exit(1);
  }
  log('chaos lane: all selected scenarios passed');
  process.exit(0);
}

await main();
