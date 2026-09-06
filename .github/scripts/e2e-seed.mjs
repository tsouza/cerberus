// e2e-seed.mjs — seed (or re-seed) deterministic OTel fixture rows into the
// e2e cluster's ClickHouse through a throwaway port-forward, parameterized
// by port and log path so ONE script replaces both `just e2e-seed` (the
// initial seed, port 19000) and `just e2e-reseed` (the chaos lane's
// one-shot re-anchor after a CH-recreating scenario, port 19001 — a
// DISTINCT port so it never races `e2e-seed-rolling`'s own long-lived
// forward on 19000) (cerberus issue #3096, epic #3091 phase 5).
//
// Runs test/e2e/seed/cmd/seed (a) applying the upstream OTel-CH DDL via
// internal/schema/ddl.Apply and (b) inserting the deterministic fixture
// rows. Idempotent — the seeder's DDL is CREATE ... IF NOT EXISTS and its
// INSERTs re-anchor on now64(9), so running this against either an empty or
// an already-populated ClickHouse is safe (see just/e2e.just's own doc for
// the dual-data-source model and the chaos-lane re-anchoring rationale this
// replaces).
//
// Opens `kubectl port-forward svc/clickhouse <PORT>:9000`, waits for it to
// accept a connection, runs the Go seeder against it with
// CH_ADDR=127.0.0.1:<PORT>, and tears the forward down on every exit path —
// success, seeder failure, or this script being killed.
//
// Env contract:
//   NAMESPACE  k8s namespace                (default cerberus)
//   PORT       local port-forward port      (default 19000)
//   LOG_PATH   port-forward stdout/stderr    (default /tmp/cerberus-e2e-seed-pf.log)
//
// Exit: the Go seeder's own exit status; 1 if the port-forward never
// accepts a connection within the wait budget.

import { spawn, spawnSync } from 'node:child_process';
import { connect } from 'node:net';
import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { createFreshFileFd, error, log } from './lib/gh.mjs';

const NAMESPACE = process.env.NAMESPACE || 'cerberus';
const PORT = Number(process.env.PORT || '19000');
const LOG_PATH = process.env.LOG_PATH || '/tmp/cerberus-e2e-seed-pf.log';

// Matches the extracted bash's own wait budget: 10 attempts, 1s apart.
const portWaitAttempts = 10;
const portWaitIntervalMs = 1000;

// portOpen — true once a raw TCP connect to 127.0.0.1:port succeeds,
// exported so the wait loop's contract is testable with a real ephemeral
// listener instead of a mocked network layer.
export function portOpen(port) {
  return new Promise((resolve) => {
    const sock = connect({ host: '127.0.0.1', port }, () => {
      sock.end();
      resolve(true);
    });
    sock.on('error', () => resolve(false));
  });
}

export async function waitForPort(port, { attempts = portWaitAttempts, intervalMs = portWaitIntervalMs, portOpenFn = portOpen } = {}) {
  for (let attempt = 1; attempt <= attempts; attempt++) {
    if (await portOpenFn(port)) return true;
    if (attempt < attempts) await sleep(intervalMs);
  }
  return false;
}

async function main() {
  const logFd = createFreshFileFd(LOG_PATH);
  const pf = spawn('kubectl', ['-n', NAMESPACE, 'port-forward', 'svc/clickhouse', `${PORT}:9000`], {
    stdio: ['ignore', logFd, logFd],
  });

  let cleanedUp = false;
  const cleanup = () => {
    if (cleanedUp) return;
    cleanedUp = true;
    if (pf.exitCode === null && pf.signalCode === null) pf.kill('SIGTERM');
  };
  process.on('exit', cleanup);

  const up = await waitForPort(PORT);
  if (!up) {
    error(`port-forward to svc/clickhouse never accepted a connection on 127.0.0.1:${PORT} within ${portWaitAttempts}s`);
    cleanup();
    process.exit(1);
  }

  log(`==> seeding OTel data via Go seeder (port-forward 127.0.0.1:${PORT} -> svc/clickhouse:9000)`);
  const seed = spawnSync('go', ['run', './test/e2e/seed/cmd/seed'], {
    stdio: 'inherit',
    env: {
      ...process.env,
      CH_ADDR: `127.0.0.1:${PORT}`,
      CH_DATABASE: 'otel',
      CH_USERNAME: 'cerberus',
      CH_PASSWORD: 'cerberus',
    },
  });
  cleanup();
  if (seed.error) {
    error(`could not start the Go seeder: ${seed.error.message}`);
    process.exit(1);
  }
  const status = seed.status === null ? 1 : seed.status;
  if (status === 0) log('==> seed done');
  process.exit(status);
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-seed.mjs');
}

if (isMain()) {
  await main();
}
