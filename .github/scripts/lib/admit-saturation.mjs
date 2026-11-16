// admit-saturation.mjs — the verdict of the chaos lane's
// load-admit-saturation scenario (chaos-run.mjs), kept pure so its
// arithmetic is unit-testable without a cluster.
//
// The scenario fires a burst of requests that each carry the chaos_sleep
// build's server-side ClickHouse sleep, so every request the admit limiter
// lets through holds its slot for a fixed time (until ClickHouse aborts the
// sleep at the chaos build's max_execution_time) while the whole burst
// arrives in milliseconds. The limiter never queues
// (internal/api/admit/admit.go): with every admitted request still held, a
// replica admits at most its cap and sheds the rest. Across `replicas`
// replicas at most `replicas × cap` requests can be admitted, so at least
// `burst − replicas × cap` must be shed — a bound that holds on every run,
// fails when admission control is off (nothing is shed), and fails when a
// replica admits past its cap. The shed count exceeds the floor when the
// load balancer hands some replica fewer than `cap` requests, leaving slots
// no burst request used.
//
// The bound is only valid while the hold happened. A request that was
// admitted and came back sooner than `minHoldMs` means the sleep did not
// hold its slot (for example an image built without `-tags chaos_sleep`,
// where the header is inert), so the overlap the bound assumes is not
// guaranteed; that is reported as its own failure rather than letting the
// shed count pass or fail by chance.

// ADMIT_BURST_SIZE is how many held requests the scenario fires at once. It
// must exceed the lane's largest possible slot count — the e2e HPA ceiling
// (test/e2e/k3s/cerberus-values.yaml maxReplicas) × ADMIT_CAP_ENV — so the
// shed floor stays at least 1 however far the HPA has scaled.
export const ADMIT_BURST_SIZE = 16;

// ADMIT_CAP_ENV is the chaos overlay knob that sizes the prom head's budget.
export const ADMIT_CAP_ENV = 'CERBERUS_ADMIT_PROM';

// ADMIT_SHED_RETRY_AFTER is the Retry-After value the prom head's admit
// middleware stamps on a shed (internal/api/prom/handler.go wires
// Limiter.Middleware(1, ...)).
export const ADMIT_SHED_RETRY_AFTER = '1';

// ADMIT_SHED_BODY_MARKER is the substring of the admit middleware's 503
// body ("admission control: server saturated") that tells a shed apart from
// the handler's own 503s (timeout, breaker open).
export const ADMIT_SHED_BODY_MARKER = 'saturated';

// isAdmitShed reports whether one burst response is an admit-limiter shed.
export function isAdmitShed(r) {
  return (
    r.status === 503 &&
    r.headers?.['retry-after'] === ADMIT_SHED_RETRY_AFTER &&
    String(r.body || '').toLowerCase().includes(ADMIT_SHED_BODY_MARKER)
  );
}

// admitCapFromEnvArgs returns the positive integer value of `key` among the
// overlay's KEY=VALUE tokens (e2e-chaos-overlay.mjs's parseEnvFile output),
// or null when the key is absent or not a positive integer.
export function admitCapFromEnvArgs(envArgs, key) {
  const prefix = `${key}=`;
  const token = envArgs.find((a) => a.startsWith(prefix));
  if (token === undefined) return null;
  const raw = token.slice(prefix.length);
  if (!/^[1-9][0-9]*$/.test(raw)) return null;
  return Number(raw);
}

// outcomeLabel names one response for the status tally.
function outcomeLabel(r) {
  if (isAdmitShed(r)) return 'shed';
  if (!r.ok) return 'transport-error';
  return String(r.status);
}

// statusTally renders the burst's outcomes as "shed×12 503×4", in first-seen
// order, so a failure message shows what every request got.
export function statusTally(results) {
  const counts = new Map();
  for (const r of results) {
    const label = outcomeLabel(r);
    counts.set(label, (counts.get(label) || 0) + 1);
  }
  return [...counts].map(([label, n]) => `${label}×${n}`).join(' ');
}

// judgeAdmitSaturation returns the scenario's verdict over the burst.
//
//   results       — one httpGet result per burst request: { ok, status,
//                   headers, body, elapsedMs }
//   replicas      — Ready cerberus replicas behind the Service
//   capPerReplica — the overlay's CERBERUS_ADMIT_PROM
//   minHoldMs     — the shortest time an admitted request must have held
//                   its slot for the bound to apply
//
// A request is admitted when it got an HTTP response other than a shed; a
// request with no HTTP response at all is neither, and fails the verdict.
// Returns { shed, admitted, capacity, minShed, tally, failures }; the
// scenario passes when `failures` is empty.
export function judgeAdmitSaturation({ results, replicas, capPerReplica, minHoldMs }) {
  const burst = results.length;
  const tally = statusTally(results);
  const failures = [];

  if (!Number.isInteger(replicas) || replicas < 1) {
    failures.push(`no Ready cerberus replica to saturate (got ${replicas})`);
  }
  if (!Number.isInteger(capPerReplica) || capPerReplica < 1) {
    failures.push(`the prom admit cap must be a positive integer (got ${capPerReplica})`);
  }
  if (failures.length > 0) {
    return { shed: 0, admitted: 0, capacity: null, minShed: null, tally, failures };
  }

  const capacity = replicas * capPerReplica;
  const minShed = burst - capacity;
  const shed = results.filter(isAdmitShed).length;
  const unanswered = results.filter((r) => !r.ok);
  const admittedResults = results.filter((r) => r.ok && !isAdmitShed(r));
  const admitted = admittedResults.length;

  if (minShed < 1) {
    failures.push(
      `a burst of ${burst} cannot oversubscribe ${replicas} replica(s) × cap ${capPerReplica} = ${capacity} slots`,
    );
    return { shed, admitted, capacity, minShed, tally, failures };
  }

  if (unanswered.length > 0) {
    failures.push(
      `${unanswered.length} burst request(s) got no HTTP response (${unanswered[0].err}) [${tally}]`,
    );
  }
  const early = admittedResults.filter((r) => !(r.elapsedMs >= minHoldMs));
  if (early.length > 0) {
    const fastest = Math.min(...early.map((r) => r.elapsedMs ?? 0));
    failures.push(
      `${early.length} admitted request(s) returned before the ${minHoldMs} ms hold (fastest ${fastest} ms) — the chaos sleep did not hold their admit slots, so the burst's overlap is not guaranteed (is the image built with -tags chaos_sleep, or did the request fail before ClickHouse held it?) [${tally}]`,
    );
  }
  if (shed < minShed) {
    failures.push(
      `expected at least ${minShed} of ${burst} held requests to shed with 503 + Retry-After:${ADMIT_SHED_RETRY_AFTER} '${ADMIT_SHED_BODY_MARKER}' (${replicas} replica(s) × cap ${capPerReplica} = ${capacity} slots); ${shed} did [${tally}]`,
    );
  }
  if (admitted < 1) {
    failures.push(`no request of ${burst} was admitted — the limiter shed every request with free slots [${tally}]`);
  }

  return { shed, admitted, capacity, minShed, tally, failures };
}
