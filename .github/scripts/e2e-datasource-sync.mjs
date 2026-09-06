// e2e-datasource-sync.mjs — split-mode Grafana-datasource ConfigMap rewrite
// + sync-wait + rollout-restart, extracted from `e2e-up`'s split-mode block
// (cerberus issue #3096, epic #3091 phase 5).
//
// WHY THIS EXISTS. In split mode (`E2E_MODE=split`) each head (prometheus /
// loki / tempo) gets its own bare-named Service; the kustomize-applied
// Grafana datasource ConfigMap still points every datasource type at the
// monolith URL `http://cerberus:8080`, which in split mode resolves to the
// NodePort-alias Service selecting ONLY the prometheus head — so loki/tempo
// datasource queries would 404 (the v1.4.0 split-mode dashboard-lane
// breakage this fixes). This rewrites the ConfigMap's `url:` line for each
// datasource type to its own per-head Service host.
//
// Grafana provisions datasources into its DB ONCE, at boot, from the
// mounted ConfigMap file, and the kubelet's ConfigMap->volume sync lags a
// pod's startup by tens of seconds — updating the ConfigMap object alone
// does NOT change what Grafana already provisioned. So this: rewrites the
// ConfigMap, BLOCKS until the kubelet has synced the corrected content into
// the running Grafana pod's mounted file (polled, never a fixed sleep —
// race-free), THEN restarts Grafana so it re-provisions from the corrected
// file.
//
// Env contract:
//   NAMESPACE                  k8s namespace                (default cerberus)
//   CONFIGMAP                  the datasource ConfigMap name (default grafana-datasources)
//   GRAFANA_DEPLOYMENT         Grafana Deployment name        (default grafana)
//   ROLLOUT_TIMEOUT_SECONDS    per-rollout-status budget      (default 120)
//   SYNC_POLL_ATTEMPTS         kubelet-sync poll attempts     (default 60)
//   SYNC_POLL_INTERVAL_SECONDS seconds between poll attempts  (default 2)
//
// Exit: 0 on success; 1 if the per-head URLs never sync into the Grafana
// pod within the poll budget, or any kubectl step fails.

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { capture, error, exec, log, notice } from './lib/gh.mjs';
import { makeKubectl } from './lib/k8s.mjs';

const NAMESPACE = process.env.NAMESPACE || 'cerberus';
const CONFIGMAP = process.env.CONFIGMAP || 'grafana-datasources';
const GRAFANA_DEPLOYMENT = process.env.GRAFANA_DEPLOYMENT || 'grafana';
const ROLLOUT_TIMEOUT_SECONDS = Number(process.env.ROLLOUT_TIMEOUT_SECONDS || '120');
const SYNC_POLL_ATTEMPTS = Number(process.env.SYNC_POLL_ATTEMPTS || '60');
const SYNC_POLL_INTERVAL_SECONDS = Number(process.env.SYNC_POLL_INTERVAL_SECONDS || '2');

// The datasource types the chart provisions, and the monolith URL the
// kustomize-applied ConfigMap ships every one of them pointed at.
const DATASOURCE_TYPES = ['prometheus', 'loki', 'tempo'];
const MONOLITH_HOST = 'cerberus';
const MONOLITH_URL_LINE = `url: http://${MONOLITH_HOST}:8080`;

// rewriteDatasourceUrls — the same state machine the extracted `awk` one-
// liner ran: track the most recently seen `type: <prometheus|loki|tempo>`
// line, and on a `url: http://cerberus:8080` line, replace the `cerberus`
// host with the current type's own bare-named Service — so
// `type: loki` .. `url: http://cerberus:8080` becomes
// `url: http://loki:8080`. Pure so this is unit-testable without a cluster.
export function rewriteDatasourceUrls(yaml) {
  let currentType = null;
  const typeRe = new RegExp(`type:\\s*(${DATASOURCE_TYPES.join('|')})\\b`);
  return yaml
    .split('\n')
    .map((line) => {
      const typeMatch = typeRe.exec(line);
      if (typeMatch) currentType = typeMatch[1];
      if (currentType && line.includes(MONOLITH_URL_LINE)) {
        return line.replace(MONOLITH_HOST, currentType);
      }
      return line;
    })
    .join('\n');
}

async function main() {
  const kubectl = makeKubectl(capture, NAMESPACE);

  log('==> [split] rewriting Grafana datasource URLs to per-head Services');
  const got = kubectl(['get', 'configmap', CONFIGMAP, '-o', 'jsonpath={.data.datasources\\.yaml}']);
  if (got.status !== 0) {
    error(`could not read configmap/${CONFIGMAP}: ${got.stderr.trim()}`);
    process.exit(1);
  }
  const rewritten = rewriteDatasourceUrls(got.stdout);

  const scratchDir = mkdtempSync(join(tmpdir(), 'cerberus-e2e-ds-split-'));
  const scratchFile = join(scratchDir, 'datasources.yaml');
  writeFileSync(scratchFile, rewritten);

  const rendered = kubectl([
    'create', 'configmap', CONFIGMAP,
    `--from-file=datasources.yaml=${scratchFile}`,
    '--dry-run=client', '-o', 'yaml',
  ]);
  if (rendered.status !== 0) {
    error(`could not render the rewritten configmap/${CONFIGMAP}: ${rendered.stderr.trim()}`);
    process.exit(1);
  }
  const applied = kubectl(['apply', '-f', '-'], { input: rendered.stdout });
  if (applied.status !== 0) {
    error(`could not apply the rewritten configmap/${CONFIGMAP}: ${applied.stderr.trim()}`);
    process.exit(1);
  }

  log('==> [split] waiting for the per-head datasource URLs to sync into the Grafana pod');
  exec('kubectl', ['-n', NAMESPACE, 'rollout', 'status', `deployment/${GRAFANA_DEPLOYMENT}`, `--timeout=${ROLLOUT_TIMEOUT_SECONDS}s`]);

  let synced = false;
  for (let attempt = 1; attempt <= SYNC_POLL_ATTEMPTS; attempt++) {
    const res = kubectl([
      'exec', `deploy/${GRAFANA_DEPLOYMENT}`, '--',
      'grep', '-q', 'url: http://loki:8080',
      '/etc/grafana/provisioning/datasources/datasources.yaml',
    ]);
    if (res.status === 0) {
      synced = true;
      break;
    }
    await sleep(SYNC_POLL_INTERVAL_SECONDS * 1000);
  }
  if (!synced) {
    error(
      `per-head datasource URLs never synced into the Grafana pod after ` +
        `${SYNC_POLL_ATTEMPTS * SYNC_POLL_INTERVAL_SECONDS}s`,
    );
    process.exit(1);
  }

  log('==> [split] restarting Grafana so it re-provisions datasources from the corrected file');
  exec('kubectl', ['-n', NAMESPACE, 'rollout', 'restart', `deployment/${GRAFANA_DEPLOYMENT}`]);
  exec('kubectl', ['-n', NAMESPACE, 'rollout', 'status', `deployment/${GRAFANA_DEPLOYMENT}`, `--timeout=${ROLLOUT_TIMEOUT_SECONDS}s`]);

  notice('e2e-datasource-sync: per-head Grafana datasource URLs synced and Grafana re-provisioned');
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-datasource-sync.mjs');
}

if (isMain()) {
  await main();
  process.exit(0);
}
