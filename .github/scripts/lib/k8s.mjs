// k8s.mjs — shared k8s + in-cluster-ClickHouse lookups for the e2e Node
// scripts (cerberus issues #3075, #3082, #3096).
//
// Started as `lib/bwc-k8s.mjs`, scoped to the bundled-ClickHouse ("bwc") e2e
// verify scripts alone (`e2e-bwc-verify-placement.mjs` and
// `e2e-bwc-verify-mode-toggle.mjs`), which both needed a namespaced
// `kubectl` runner and a "find the bundled ClickHouse pod" lookup.
// `e2e-datashard-verify.mjs` (issue #3079) became a third consumer of
// `makeKubectl` — nothing here is actually bwc-specific — and issue #3096's
// `e2e-wait-otel.mjs` needs the same `kubectl`-exec ClickHouse query pattern
// those bwc scripts each re-implemented locally as their own `chQuery`.
// Renamed and promoted rather than left as a second parallel k8s-helper
// module: one source of truth for every e2e script that needs to shell a
// `kubectl`/ClickHouse command into the cluster (CLAUDE.md's DRY rule).
//
// Before adding a NEW helper here, check whether an existing e2e script
// already has an equivalent inlined before reaching for a parallel
// implementation. `chQuery` takes an optional `format` (the
// `--format TSVRaw` shape `e2e-datashard-verify.mjs` and
// `e2e-datashard-replica-affinity-verify.mjs` both read multi-column rows
// through, each behind a two-line row-splitting wrapper of its own), so a
// new script needing a different output format passes it here rather than
// re-implementing the `kubectl exec ... clickhouse-client` invocation.
// `waitForClusterHealth` (cerberus issue #3172) is the same story one level
// up: it started as `e2e-datashard-replica-affinity-verify.mjs`'s own
// private poll loop and was promoted here so every datashard-lane script
// that fires load right after `just e2e-datashard-up` can wait out the
// same startup race, not just the one script that first hit it.

import { error } from './gh.mjs';
import { pollUntil } from './poll.mjs';

// Returns a `kubectl(args, opts) => capture() result` closure pinned to the
// given namespace.
export function makeKubectl(capture, ns) {
  return (args, opts = {}) => capture('kubectl', ['-n', ns, ...args], opts);
}

// Resolve the bundled ClickHouse pod by the chart's immutable selector
// label. Exits 1 (with an ::error:: annotation) if no such pod exists yet —
// callers never need to handle a missing pod themselves.
export function clickhousePodName(kubectl, ns) {
  const res = kubectl([
    'get', 'pod',
    '-l', 'app.kubernetes.io/component=clickhouse',
    '-o', 'jsonpath={.items[0].metadata.name}',
  ]);
  const name = res.stdout.trim();
  if (res.status !== 0 || !name) {
    error(`could not resolve bundled ClickHouse pod in namespace ${ns}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return name;
}

// Run a ClickHouse query inside a pod/Deployment via `kubectl exec ... --
// clickhouse-client`, WITHOUT exiting on failure — the caller decides what a
// non-zero status means (e2e-wait-otel.mjs's poll loop treats a not-yet-
// query-able ClickHouse as "count is 0 so far", never a hard failure).
// `target` is anything `kubectl exec` accepts (a pod name, or `deploy/foo`).
export function chQueryRaw(kubectl, target, { database, user = 'cerberus', password = 'cerberus', format } = {}, sql) {
  const args = ['exec', target, '--', 'clickhouse-client', '--user', user, '--password', password];
  if (database) args.push('--database', database);
  if (format) args.push('--format', format);
  args.push('--query', sql);
  return kubectl(args);
}

// chQueryRaw, but exits 1 (with an ::error:: annotation) on a non-zero
// status and returns the trimmed stdout directly — the shape every
// assertion-style verify script (never a best-effort poll) wants.
export function chQuery(kubectl, target, opts, sql) {
  const res = chQueryRaw(kubectl, target, opts, sql);
  if (res.status !== 0) {
    error(`clickhouse query failed: ${sql}\n${res.stderr.trim()}`);
    process.exit(1);
  }
  return res.stdout.trim();
}

// clusterHealthPollIntervalMs is deliberately wider than pollUntil's own
// DEFAULT_POLL_INTERVAL_MS: each iteration here spawns a fresh `kubectl
// exec` + `clickhouse-client` subprocess (chQuery has no persistent-session
// mode), and the phenomenon this poll waits out — ClickHouse's own
// distributed_replica_error_half_life — decays over 60s by default, so
// sub-second responsiveness buys nothing a caller could observe. 5s cuts a
// worst-case 60s wait from ~30 subprocess spawns to ~12 while staying far
// finer than the 60s time constant it is tracking.
const clusterHealthPollIntervalMs = 5_000;

// waitForClusterHealth polls `system.clusters` until every (shard, replica)
// row's errors_count is 0, or deadlineMs elapses (cerberus issue #3148,
// promoted to this shared module by issue #3172 — same class as #3109/PR
// #3125's mode-toggle readiness race).
//
// THE RACE THIS EXISTS TO CLOSE: `just e2e-datashard-up`'s readiness gate
// is `kubectl rollout status`, which only proves each ClickHouse pod's own
// container passed its liveness/readiness probe — NOT that its inter-node
// connections to every OTHER data shard (and, on a multi-replica-per-shard
// topology, every peer replica) are already dialable. The first cross-node
// dial issued before that settles gets refused, and ClickHouse increments
// the target's `system.clusters.errors_count` server-side
// (`PoolWithFailoverBase::Pool::error_count`) — during that decay window a
// verify script observing replica/shard selection, or firing load that
// assumes a settled cluster, would see behavior the startup race caused
// rather than the steady-state behavior it means to check. This is why the
// wait belongs here rather than in any ONE verify script: every datashard-
// lane script that fires load right after cluster-up is exposed to it.
//
// `target` and `chOpts` are chQuery's own (a pod name or `deploy/foo`, and
// `{ database, user, password }`). Returns the elapsed seconds on success.
// Throws with the last-seen dirty rows on timeout — firing load into a
// cluster that never settled would only reproduce the exact race this poll
// exists to wait out.
export async function waitForClusterHealth(kubectl, target, chOpts, { cluster, deadlineMs, intervalMs = clusterHealthPollIntervalMs } = {}) {
  const start = Date.now();
  let lastDirty = [];
  const ok = await pollUntil(
    async () => {
      const out = chQuery(
        kubectl,
        target,
        { ...chOpts, format: 'TSVRaw' },
        `SELECT shard_num, replica_num, errors_count
         FROM system.clusters WHERE cluster = '${cluster}'
         ORDER BY shard_num, replica_num`,
      );
      const rows = out.split('\n').map((l) => l.trimEnd()).filter((l) => l.length > 0);
      lastDirty = rows.map((r) => r.split('\t')).filter(([, , errorsCount]) => Number(errorsCount) !== 0);
      return lastDirty.length === 0;
    },
    { deadlineMs, intervalMs, label: 'cluster-health' },
  );
  if (!ok) {
    const detail = lastDirty.map(([shard, replica, n]) => `shard=${shard} replica=${replica} errors_count=${n}`).join('; ');
    throw new Error(
      `system.clusters never reached a clean state (errors_count=0 for every replica) within ` +
        `${(deadlineMs / 1000).toFixed(0)}s: ${detail} — firing load now would risk observing a ` +
        `decision still influenced by the connection errors this poll exists to wait out`,
    );
  }
  return (Date.now() - start) / 1000;
}
