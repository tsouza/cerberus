// e2e-seed-rolling.mjs — launch the rolling seeder (seed once, then
// re-anchor the metric/log windows on now64(9) every 30s until stopped),
// extracted from `just e2e-seed-rolling`'s inline bash (cerberus issue
// #3096, epic #3091 phase 5).
//
// Replaces the static-window arms-race that widened the seed envelope to
// +-15 min in PRs #590/#615/#617/#693 just to survive the ~12 min
// Playwright suite drift — with fresh data arriving continuously the static
// window only has to cover the 30s gap between two ticks plus the 5m
// Prom/Loki staleness lookback. See just/e2e.just's own doc for the full
// rationale, including the chaos lane's port-forward-durability need
// (test/e2e/seed/port_forward_supervisor.sh, kept as-is — a reconnecting
// bash supervisor is orthogonal to this extraction).
//
// Runs three steps, each detached from THIS process (so the recipe returns
// promptly and the next CI step can run while the seeder keeps reseeding):
//   1. `setsid bash port_forward_supervisor.sh <ns> svc/clickhouse
//      19000:9000`, its own session/process-group leader (so
//      e2e-seed-stop.mjs can signal the whole group, not just the leader).
//   2. wait for the forward to accept a connection.
//   3. `go build` the seeder once (a stray `go run` keeps the toolchain
//      attached to this process, harder to detach cleanly across CI step
//      boundaries), then launch it detached under `--re-seed-interval=30s`.
// Then blocks until the seeder's own log reports the initial seed landed,
// so the CALLER (e2e-wait-otel / e2e-run) sees a populated database.
//
// Env contract:
//   NAMESPACE  k8s namespace                  (default cerberus)
//   PORT       local port-forward port        (default 19000)
//
// File paths (not parameterized — read by e2e-seed-stop.mjs too, and by
// operators tailing them during a live run):
//   /tmp/cerberus-e2e-seed-pf.log / .pid        port-forward supervisor
//   /tmp/cerberus-e2e-seeder                    built seeder binary
//   /tmp/cerberus-e2e-seed-rolling.log / .pid   rolling seeder
//
// Exit: 0 once the initial seed has landed (its log contains "seed: done");
// 1 if the port-forward never opens, or the initial seed does not land
// within its own wait budget.

import { spawn, spawnSync } from 'node:child_process';
import { existsSync, openSync, readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { error, log, notice } from './lib/gh.mjs';
import { waitForPort } from './e2e-seed.mjs';

const NAMESPACE = process.env.NAMESPACE || 'cerberus';
const PORT = Number(process.env.PORT || '19000');

const PF_LOG = '/tmp/cerberus-e2e-seed-pf.log';
const PF_PID = '/tmp/cerberus-e2e-seed-pf.pid';
const SEEDER_BIN = '/tmp/cerberus-e2e-seeder';
const ROLLING_LOG = '/tmp/cerberus-e2e-seed-rolling.log';
const ROLLING_PID = '/tmp/cerberus-e2e-seed-rolling.pid';

// Matches the extracted bash's own budgets.
const portWaitAttempts = 10;
const portWaitIntervalMs = 1000;
const initialSeedWaitAttempts = 15;
const initialSeedWaitIntervalMs = 2000;
const reseedIntervalFlag = '--re-seed-interval=30s';

function spawnDetached(cmd, args, logPath, pidPath) {
  const fd = openSync(logPath, 'w');
  const child = spawn(cmd, args, { detached: true, stdio: ['ignore', fd, fd] });
  child.unref();
  writeFileSync(pidPath, `${child.pid}\n`);
  return child.pid;
}

// initialSeedLanded — true once `logPath` contains the seeder's own
// "seed: done" marker line, exactly the extracted bash's `grep -q '^.*seed:
// done'`. Pure(ish) I/O-only read so the marker string stays in exactly one
// place.
export function initialSeedLanded(logPath) {
  if (!existsSync(logPath)) return false;
  return /seed: done/.test(readFileSync(logPath, 'utf8'));
}

async function main() {
  log('==> launching rolling seeder (30s tick) in background');

  log('    1) starting the reconnecting port-forward supervisor (setsid)');
  spawnDetached(
    'setsid',
    ['bash', 'test/e2e/seed/port_forward_supervisor.sh', NAMESPACE, 'svc/clickhouse', `${PORT}:9000`],
    PF_LOG,
    PF_PID,
  );

  log('    2) waiting for the forward to accept a connection');
  const up = await waitForPort(PORT, { attempts: portWaitAttempts, intervalMs: portWaitIntervalMs });
  if (!up) {
    error(`port-forward to svc/clickhouse never accepted a connection on 127.0.0.1:${PORT} within ${portWaitAttempts}s`);
    process.exit(1);
  }

  log('    3) building the seeder binary');
  const build = spawnSync('go', ['build', '-o', SEEDER_BIN, './test/e2e/seed/cmd/seed'], { stdio: 'inherit' });
  if (build.status !== 0) {
    error(`go build of the seeder failed with status ${build.status}`);
    process.exit(build.status ?? 1);
  }

  log('    4) launching the seeder in the background with the rolling flag');
  const fd = openSync(ROLLING_LOG, 'w');
  const seeder = spawn(SEEDER_BIN, [reseedIntervalFlag], {
    detached: true,
    stdio: ['ignore', fd, fd],
    env: {
      ...process.env,
      CH_ADDR: `127.0.0.1:${PORT}`,
      CH_DATABASE: 'otel',
      CH_USERNAME: 'cerberus',
      CH_PASSWORD: 'cerberus',
    },
  });
  seeder.unref();
  writeFileSync(ROLLING_PID, `${seeder.pid}\n`);

  log(`==> rolling seeder pid=${seeder.pid} pf-pid=${readFileSync(PF_PID, 'utf8').trim()}`);
  log("    initial seed runs synchronously inside the seeder before the loop starts —");
  log(`    tail ${ROLLING_LOG} to confirm 'seed: done' lands.`);

  log('    5) waiting for the initial seed to land before returning');
  for (let attempt = 1; attempt <= initialSeedWaitAttempts; attempt++) {
    if (initialSeedLanded(ROLLING_LOG)) {
      notice('initial seed landed');
      return true;
    }
    await sleep(initialSeedWaitIntervalMs);
  }
  return false;
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-seed-rolling.mjs');
}

if (isMain()) {
  const landed = await main();
  if (!landed) {
    const totalSeconds = (initialSeedWaitAttempts * initialSeedWaitIntervalMs) / 1000;
    error(`initial seed did not complete within ${totalSeconds}s. log:`);
    if (existsSync(ROLLING_LOG)) process.stderr.write(readFileSync(ROLLING_LOG, 'utf8'));
    process.exit(1);
  }
  process.exit(0);
}
