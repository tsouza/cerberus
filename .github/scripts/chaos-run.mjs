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

  // Post-recovery metric corroboration (settle poll): trips >= 1, state == 0.
  log('  corroborating breaker self-metrics via Prom head (settle poll)...');
  const tripsOk = await pollUntil(
    async () => {
      const trips = await queryBreakerMetric('cerberus_ch_breaker_trips_total');
      return trips !== null && trips >= 1;
    },
    { deadlineMs: METRIC_SETTLE_DEADLINE_MS, intervalMs: SETTLE_INTERVAL_MS, label: 'trips>=1' },
  );
  if (!tripsOk) {
    failures.push('cerberus_ch_breaker_trips_total never reached >=1 (breaker trip not recorded)');
  } else {
    log('    cerberus_ch_breaker_trips_total >= 1');
  }
  const state = await queryBreakerMetric('cerberus_ch_breaker_state');
  if (state !== null && state !== 0) {
    failures.push(`cerberus_ch_breaker_state should be 0 (CLOSED) after recovery; got ${state}`);
  } else {
    log(`    cerberus_ch_breaker_state == ${state === null ? '(pending, tolerated)' : state}`);
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

  // A heavy query_range: a wide range with a tiny step explodes into
  // millions of anchors (the O(rows x anchors) compute fan-out), reliably
  // blowing past the small CERBERUS_QUERY_TIMEOUT (3s) the chaos overlay
  // set. ?timeout= mins with the cap; we lean on the configured cap.
  const now = Math.floor(Date.now() / 1000);
  const start = now - 30 * 24 * 3600; // 30 days
  const slowPath =
    '/api/v1/query_range?query=' +
    encodeURIComponent('sum(rate(http_server_request_duration_count[5m]))') +
    `&start=${start}&end=${now}&step=1`;

  log('  fault: issuing a deliberately slow query_range (wide range, 1s step)...');
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

  // Fire N concurrent requests > the (overlay) admit cap. Use the slow
  // query so each request holds its admit slot long enough to overlap.
  const now = Math.floor(Date.now() / 1000);
  const start = now - 7 * 24 * 3600;
  const slowPath =
    '/api/v1/query_range?query=' +
    encodeURIComponent('sum(rate(http_server_request_duration_count[5m]))') +
    `&start=${start}&end=${now}&step=1`;

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
  if (stateAfter !== null && stateAfter !== 0) {
    failures.push(`cerberus_ch_breaker_state should stay 0 under admit saturation (admit/pool rejections are breaker-neutral); got ${stateAfter}`);
  } else {
    log(`    cerberus_ch_breaker_state == ${stateAfter === null ? `(pending; baseline ${baselineState})` : stateAfter} (breaker CLOSED)`);
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

const PHASE1 = [
  { name: 'ch-pod-kill', run: scenarioChPodKill },
  { name: 'ch-slow-query-timeout', run: scenarioChSlowTimeout },
  { name: 'cerberus-pod-kill', run: scenarioCerberusPodKill },
];

const PHASE2 = [
  { name: 'ch-network-partition', run: scenarioChNetworkPartition },
  { name: 'load-admit-saturation', run: scenarioLoadAdmitSaturation },
];

function selectedScenarios() {
  let pool = PHASE === 'all' ? [...PHASE1, ...PHASE2] : [...PHASE1];
  if (ONLY.length > 0) pool = pool.filter((s) => ONLY.includes(s.name));
  return pool;
}

async function preflight() {
  // The whole lane assumes a healthy stack from `just e2e-up` + seed +
  // wait-otel. Confirm green before injecting the first fault so a
  // bring-up problem doesn't masquerade as a chaos failure.
  log('preflight: asserting the stack is green before fault injection...');
  const ready = await assertReadyzGreen(60_000);
  if (!ready) {
    error('preflight: /readyz never reached 200 — the stack is not healthy; aborting before any fault injection');
    return false;
  }
  const headsOk = await assertHeadsHealthy(60_000);
  if (!headsOk) {
    error('preflight: not all 3 heads returned 200 before fault injection — aborting');
    return false;
  }
  log('preflight OK: /readyz 200 + all heads 200');
  return true;
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
  for (const s of scenarios) {
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
