// e2e-seed-stop.mjs — stop the rolling seeder + its port-forward supervisor
// (idempotent), extracted from `just e2e-seed-stop`'s inline bash (cerberus
// issue #3096, epic #3091 phase 5).
//
// Called from CI teardown so the dashboard job tears down cleanly even when
// the Playwright step failed before reaching `e2e-down`. SIGTERM gives the
// seeder a chance to log the exit reason; the port-forward never has any
// state to flush. The port-forward PID file holds the supervisor's setsid
// leader PID (== its PGID, since `setsid` made it a new session/group
// leader), so `kill -TERM -<pid>` (a NEGATIVE pid — the process-GROUP
// signal form) hits the whole group: the supervisor AND its current
// `kubectl port-forward` child, instead of orphaning the child while the
// supervisor itself dies.
//
// File paths — the exact ones e2e-seed-rolling.mjs writes:
//   /tmp/cerberus-e2e-seed-rolling.pid   rolling seeder (plain PID, SIGTERM)
//   /tmp/cerberus-e2e-seed-pf.pid        port-forward supervisor (PGID, group SIGTERM)
//
// A missing PID file, or a PID that no longer exists, is a silent no-op —
// this is called unconditionally from e2e-down and must never fail a
// teardown that has nothing left to stop.
//
// Exit: always 0 (idempotent teardown; matches the extracted bash's own
// `|| true` on every kill).

import { existsSync, readFileSync, unlinkSync } from 'node:fs';
import process from 'node:process';

import { log } from './lib/gh.mjs';

const ROLLING_PID = '/tmp/cerberus-e2e-seed-rolling.pid';
const PF_PID = '/tmp/cerberus-e2e-seed-pf.pid';

// safeKill — signal `pid` (or, if negative, the process GROUP `-pid`),
// swallowing ESRCH/EPERM exactly like the extracted bash's `|| true`. Pure
// enough (its only effect is the kill syscall) to keep the two teardown
// steps below symmetric and testable via the `killFn` seam.
function safeKill(pid, signal, killFn = process.kill) {
  try {
    killFn(pid, signal);
    return true;
  } catch {
    return false;
  }
}

export function stopRollingSeeder({ pidFile = ROLLING_PID, killFn = process.kill, unlinkFn = unlinkSync, existsFn = existsSync, readFn = readFileSync } = {}) {
  if (!existsFn(pidFile)) return;
  const pid = Number(readFn(pidFile, 'utf8').trim());
  if (Number.isFinite(pid)) safeKill(pid, 'SIGTERM', killFn);
  unlinkFn(pidFile);
}

export function stopPortForwardSupervisor({ pidFile = PF_PID, killFn = process.kill, unlinkFn = unlinkSync, existsFn = existsSync, readFn = readFileSync } = {}) {
  if (!existsFn(pidFile)) return;
  const pid = Number(readFn(pidFile, 'utf8').trim());
  if (Number.isFinite(pid)) {
    // Group signal first (kills the supervisor + its live kubectl child in
    // one syscall); fall back to a plain signal on the bare PID only if the
    // group form itself is rejected (e.g. the process was never its own
    // group leader for some reason) — mirrors the extracted bash's
    // `kill -TERM -"$pid" || kill -TERM "$pid" || true`.
    if (!safeKill(-pid, 'SIGTERM', killFn)) safeKill(pid, 'SIGTERM', killFn);
  }
  unlinkFn(pidFile);
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-seed-stop.mjs');
}

if (isMain()) {
  log('==> stopping rolling seeder');
  stopRollingSeeder();
  stopPortForwardSupervisor();
  process.exit(0);
}
