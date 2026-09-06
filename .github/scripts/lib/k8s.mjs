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
// already has an equivalent inlined (as `e2e-datashard-verify.mjs`'s own
// `chQueryTSV`/`chExec` do, for a `--format TSVRaw` shape `chQuery` below
// does not cover) before reaching for a parallel implementation.

import { error } from './gh.mjs';

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
