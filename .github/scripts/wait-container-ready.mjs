// wait-container-ready.mjs — block until an HTTP endpoint inside a docker
// container answers, for e2e.yml's `startup-bench` job (the inline ClickHouse
// it runs with `docker run`). The workflow used to carry the poll as an
// inline `for i in $(seq …)` loop; the loop is lib/poll.mjs's pollUntil.
//
// The probe runs INSIDE the container (`docker exec <name> wget --spider
// <url>`), so the container's own network namespace answers — the published
// host port is not part of what is being waited for.
//
// Env:
//   CONTAINER              the container name (required)
//   URL                    the URL to probe from inside it (required)
//   DEADLINE_SECONDS       total poll budget           (default 120)
//   POLL_INTERVAL_SECONDS  seconds between attempts    (default 5)
//
// Exit: 0 once the probe succeeds; 1 on the deadline, with the container's
// recent log tail printed for the diagnosis.

import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { capture, error, log } from './lib/gh.mjs';
import { pollUntil } from './lib/poll.mjs';

export const DEFAULT_DEADLINE_SECONDS = 120;
export const DEFAULT_POLL_INTERVAL_SECONDS = 5;
// LOG_TAIL_LINES is how much of the container log a timeout prints.
const LOG_TAIL_LINES = 100;

export function readSeconds(raw, fallback) {
  if (raw === undefined || raw === '') return fallback;
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) throw new Error(`expected a positive number of seconds, got ${JSON.stringify(raw)}`);
  return n;
}

async function main() {
  const container = process.env.CONTAINER;
  const url = process.env.URL;
  if (!container || !url) throw new Error('CONTAINER and URL are required');
  const deadlineSeconds = readSeconds(process.env.DEADLINE_SECONDS, DEFAULT_DEADLINE_SECONDS);
  const intervalSeconds = readSeconds(process.env.POLL_INTERVAL_SECONDS, DEFAULT_POLL_INTERVAL_SECONDS);

  const started = Date.now();
  const ready = await pollUntil(
    () => capture('docker', ['exec', container, 'wget', '-q', '--spider', url]).status === 0,
    { deadlineMs: deadlineSeconds * 1000, intervalMs: intervalSeconds * 1000, label: container },
  );
  if (!ready) {
    process.stdout.write(capture('docker', ['logs', '--tail', String(LOG_TAIL_LINES), container]).stdout);
    throw new Error(`${container} did not answer ${url} within ${deadlineSeconds}s`);
  }
  log(`${container} ready after ${Math.round((Date.now() - started) / 1000)}s`);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  main().catch((e) => {
    error(e.message);
    process.exit(1);
  });
}
