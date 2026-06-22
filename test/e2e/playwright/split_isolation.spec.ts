import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import { execFileSync } from 'node:child_process';

/**
 * Split-mode blast-radius isolation.
 *
 * This is the test the `mode: split` topology EXISTS for. In monolith mode all
 * three heads share one process/cgroup, so killing the gateway kills all three
 * datasources at once — monolith CANNOT pass this test, which is exactly what
 * makes it a meaningful differentiator. In split mode each head is its own
 * Deployment + Service, so taking one head down must leave the other two
 * serving.
 *
 * The proof: scale the Tempo head Deployment to zero, wait for its Service
 * endpoints to drain, then assert that
 *   - the Tempo datasource can no longer serve a query (its backend is gone),
 *   - the Prometheus AND Loki datasources STILL serve queries.
 * Then restore the Tempo head so the cluster is left as we found it.
 *
 * This spec is assigned ONLY to the split matrix leg (see
 * .github/scripts/dashboard-matrix.mjs). It additionally fails loudly if it is
 * ever run outside split mode, so it can never silently become a no-op:
 * CERBERUS_MODE must be "split".
 */

const MODE = process.env.CERBERUS_MODE ?? '';
const NAMESPACE = process.env.CERBERUS_NAMESPACE ?? 'cerberus';
// The Tempo head Deployment name in split mode: <release>-cerberus-tempo. The
// e2e Helm release is named `cerberus` (see `just e2e-up`), so the fullname is
// `cerberus` (release name contains the chart name) and the head suffix is
// `-tempo`.
const TEMPO_DEPLOYMENT = process.env.CERBERUS_TEMPO_DEPLOYMENT ?? 'cerberus-tempo';

function kubectl(args: string[]): string {
  return execFileSync('kubectl', ['-n', NAMESPACE, ...args], { encoding: 'utf8' });
}

function scaleTempo(replicas: number): void {
  kubectl(['scale', `deployment/${TEMPO_DEPLOYMENT}`, `--replicas=${replicas}`]);
}

// Poll the Deployment's ready-replica count until it reaches `want`. Returns
// when satisfied; throws (failing the test) if the deadline passes — never a
// silent give-up.
async function waitReadyReplicas(want: number, deadlineMs: number): Promise<void> {
  const start = Date.now();
  for (;;) {
    const out = kubectl([
      'get',
      `deployment/${TEMPO_DEPLOYMENT}`,
      '-o',
      'jsonpath={.status.readyReplicas}',
    ]).trim();
    const ready = out === '' ? 0 : Number(out);
    if (ready === want) return;
    if (Date.now() - start > deadlineMs) {
      throw new Error(
        `deployment/${TEMPO_DEPLOYMENT} did not reach readyReplicas=${want} within ${deadlineMs}ms (last=${ready})`,
      );
    }
    await new Promise((r) => setTimeout(r, READY_POLL_INTERVAL_MS));
  }
}

// How long to wait for the Tempo head to finish scaling up or down.
const SCALE_DEADLINE_MS = 120_000;
// Gap between readyReplicas polls while waiting on a scale.
const READY_POLL_INTERVAL_MS = 2_000;
// Window for Grafana/Kubernetes to propagate endpoint removal after scale-to-0.
const ENDPOINT_DRAIN_TIMEOUT_MS = 30_000;

// A Prometheus-head query through the Grafana datasource proxy. `up` is the
// smallest instant vector that actually hits ClickHouse.
async function promServes(request: APIRequestContext): Promise<boolean> {
  const resp = await request.get(
    '/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up',
  );
  if (resp.status() !== 200) return false;
  const body = await resp.json();
  return body.status === 'success';
}

// A Loki-head label query through the proxy.
async function lokiServes(request: APIRequestContext): Promise<boolean> {
  const resp = await request.get('/api/datasources/proxy/uid/cerberus-loki/loki/api/v1/labels');
  if (resp.status() !== 200) return false;
  const body = await resp.json();
  return body.status === 'success';
}

// A Tempo-head query through the proxy. With the head scaled to zero its
// Service has no endpoints, so Grafana's proxy returns a 5xx (no backend).
async function tempoServes(request: APIRequestContext): Promise<boolean> {
  const resp = await request.get('/api/datasources/proxy/uid/cerberus-tempo/api/echo');
  return resp.status() === 200;
}

test.describe('split-mode head isolation', () => {
  test.beforeAll(() => {
    if (MODE !== 'split') {
      throw new Error(
        `split_isolation.spec.ts must run only in split mode (CERBERUS_MODE=split); got "${MODE}". ` +
          'This spec is gated to the split matrix leg; running it elsewhere is a configuration bug, not a skip.',
      );
    }
  });

  // Always restore the Tempo head, even if an assertion failed mid-test, so the
  // cluster is left as we found it for any later spec on the same shard.
  test.afterAll(async () => {
    if (MODE !== 'split') return;
    scaleTempo(1);
    await waitReadyReplicas(1, SCALE_DEADLINE_MS);
  });

  test('killing the Tempo head leaves Prometheus and Loki serving', async ({ request }) => {
    // Pre-condition: all three heads serve. (If this fails the cluster was
    // already unhealthy — surface it here, not as a misleading isolation
    // failure later.)
    expect(await promServes(request), 'prometheus serves before kill').toBe(true);
    expect(await lokiServes(request), 'loki serves before kill').toBe(true);
    expect(await tempoServes(request), 'tempo serves before kill').toBe(true);

    // Kill the Tempo head and wait for its endpoints to drain.
    scaleTempo(0);
    await waitReadyReplicas(0, SCALE_DEADLINE_MS);

    // The Tempo datasource must now fail — its backend is gone. expect.poll
    // tolerates the brief endpoint-removal propagation window without ever
    // passing on a still-serving Tempo.
    await expect
      .poll(async () => tempoServes(request), {
        message: 'tempo must stop serving once its head is scaled to zero',
        timeout: ENDPOINT_DRAIN_TIMEOUT_MS,
      })
      .toBe(false);

    // The whole point: the OTHER two heads keep serving through the Tempo
    // outage. A monolith (one process) could not satisfy this.
    expect(await promServes(request), 'prometheus still serves during tempo outage').toBe(true);
    expect(await lokiServes(request), 'loki still serves during tempo outage').toBe(true);
  });
});
