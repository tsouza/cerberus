// bwc-k8s.mjs — shared k8s lookups for the bundled-ClickHouse ("bwc") e2e
// verify scripts (cerberus issues #3075, #3082). e2e-bwc-verify-placement.mjs
// and e2e-bwc-verify-mode-toggle.mjs both need a namespaced `kubectl` runner
// and the same "find the bundled ClickHouse pod" lookup; this is the one
// source of truth for both so a third bwc verify script never has to
// re-copy them (CLAUDE.md invariant on DRY — duplicated logic that should be
// one source of truth).

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
