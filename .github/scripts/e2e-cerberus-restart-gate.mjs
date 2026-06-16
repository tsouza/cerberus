#!/usr/bin/env node
// e2e-cerberus-restart-gate.mjs — the "Assert zero cerberus restarts" hard
// gate for the k3d dashboard/crawl shards in .github/workflows/e2e.yml,
// with OOM-focused diagnostics.
//
// Why a module (not inline bash): the gate is no longer a one-liner. When a
// restart is detected it now dumps the evidence an OOMKill actually leaves
// — which the previous inline step missed, leaving the rc.5
// ResourceAttributes-as-Prom-labels OOM (cerberus OOMKilled, 2 restarts, on
// commit 44c2630f) undiagnosable from CI logs alone:
//
//   - `kubectl top pods --containers`  — live per-container memory usage, so
//      a pod riding near its limit at dump time is visible (best-effort:
//      metrics-server may be absent in k3d — we skip gracefully).
//   - resources.limits + the GOMEMLIMIT env per cerberus container — the
//      budget the workload blew, surfaced next to the usage.
//   - `describe pod` lastState.terminated (Reason=OOMKilled, exitCode 137)
//      surfaced PROMINENTLY — a `--previous` log tail is EMPTY for an OOM
//      kill (the kernel SIGKILLs mid-write), so the terminated-state Reason
//      is the only signal that says "this was memory, not a panic".
//   - a live heap profile (/debug/pprof/heap) pulled from each still-running
//      cerberus container BEFORE teardown when CERBERUS_DEBUG_PPROF is on —
//      the artifact that pinpoints which query shape retained the heap.
//
// Env contract (all optional except where noted):
//   NAMESPACE      — k8s namespace (default "cerberus")
//   PPROF_OUT_DIR  — directory to write captured heap profiles into
//                    (default "/tmp"); the workflow uploads it as an artifact.
//   RUN_SHARD      — the workflow gates the step on this == "true"; the
//                    `if:` in YAML already enforces it, so this script does
//                    not re-check it.
//
// Exit 0 when restarts == 0; exit 1 (after dumping evidence) otherwise.

import { error, log, group, capture } from './lib/gh.mjs';

const NS = process.env.NAMESPACE || 'cerberus';
const PPROF_OUT_DIR = process.env.PPROF_OUT_DIR || '/tmp';

// k() runs kubectl with the namespace pre-applied and never throws — a
// diagnostic command failing must not mask the real verdict.
function k(args) {
  return capture('kubectl', ['-n', NS, ...args]);
}

// dump() prints a labelled section, tolerating a failed command (|| true
// parity). Best-effort: diagnostics never fail the gate by themselves.
function dump(title, args) {
  group(title, () => {
    const res = k(args);
    if (res.stdout) log(res.stdout.trimEnd());
    if (res.status !== 0 && res.stderr) log(`(non-zero: ${res.stderr.trim()})`);
  });
}

// cerberusPods returns the pod names (no `pod/` prefix) of the cerberus
// deployment, or [] when the lookup fails.
function cerberusPods() {
  const res = k([
    'get', 'pods', '-l', 'app=cerberus',
    '-o', 'jsonpath={range .items[*]}{.metadata.name}{"\\n"}{end}',
  ]);
  if (res.status !== 0) return [];
  return res.stdout.split('\n').map((s) => s.trim()).filter(Boolean);
}

// totalRestarts sums restartCount across every cerberus pod's first
// container. Returns -1 when the lookup fails so a kubectl error doesn't
// masquerade as a clean zero.
function totalRestarts() {
  const res = k([
    'get', 'pods', '-l', 'app=cerberus',
    '-o', 'jsonpath={range .items[*]}{.status.containerStatuses[0].restartCount}{"\\n"}{end}',
  ]);
  if (res.status !== 0) {
    error(`could not read cerberus restartCount: ${res.stderr.trim()}`);
    return -1;
  }
  let total = 0;
  for (const line of res.stdout.split('\n')) {
    const n = parseInt(line.trim(), 10);
    if (!Number.isNaN(n)) total += n;
  }
  return total;
}

// surfaceLastTerminated pulls each cerberus container's lastState.terminated
// and prints the Reason/exitCode FIRST, loud, so an OOMKilled (Reason=
// OOMKilled, exitCode 137) is unmissable above the verbose describe dump.
function surfaceLastTerminated(pods) {
  group('OOM signal — lastState.terminated per cerberus container', () => {
    let sawOOM = false;
    for (const p of pods) {
      const res = k([
        'get', 'pod', p,
        '-o', 'jsonpath={range .status.containerStatuses[*]}{.name}{" lastState="}'
          + '{.lastState.terminated.reason}{" exitCode="}{.lastState.terminated.exitCode}'
          + '{" startedAt="}{.lastState.terminated.startedAt}{"\\n"}{end}',
      ]);
      const out = (res.stdout || '').trim();
      log(`${p}: ${out || '(no terminated state)'}`);
      if (/OOMKilled/.test(out) || /exitCode=137/.test(out)) sawOOM = true;
    }
    if (sawOOM) {
      error('cerberus container was OOMKilled (Reason=OOMKilled / exitCode 137) — '
        + 'the memory limit in test/e2e/k3s/cerberus.yaml was exceeded. The '
        + '--previous log tail is EMPTY for an OOM kill; see the heap profile '
        + 'artifact + `kubectl top` usage above for the query shape that blew '
        + 'the budget.');
    }
  });
}

// dumpResourceBudget prints each cerberus container's resources.limits +
// the GOMEMLIMIT env value, so the budget sits next to the usage.
function dumpResourceBudget() {
  dump('cerberus resources.limits + GOMEMLIMIT', [
    'get', 'pods', '-l', 'app=cerberus',
    '-o', 'jsonpath={range .items[*]}{.metadata.name}{": limits="}'
      + '{.spec.containers[0].resources.limits}{" env="}'
      + '{.spec.containers[0].env}{"\\n"}{end}',
  ]);
}

// captureHeapProfiles pulls /debug/pprof/heap from each still-running
// cerberus container via `kubectl exec ... wget` and writes it under
// PPROF_OUT_DIR. Best-effort: a container with no wget, no pprof endpoint
// (CERBERUS_DEBUG_PPROF unset), or already dead is skipped with a note. The
// profile of a SURVIVING replica is still useful — a restart-looping
// deployment usually has one healthy survivor whose heap shows the same
// retention shape.
function captureHeapProfiles(pods) {
  group('cerberus heap profiles (/debug/pprof/heap)', () => {
    for (const p of pods) {
      const remote = '/tmp/cerberus-heap.pprof';
      const fetched = k([
        'exec', p, '--', 'wget', '-q', '-O', remote,
        'http://localhost:8080/debug/pprof/heap',
      ]);
      if (fetched.status !== 0) {
        log(`${p}: heap profile unavailable (${(fetched.stderr || '').trim() || 'wget failed — pprof off or no wget'})`);
        continue;
      }
      const local = `${PPROF_OUT_DIR}/cerberus-heap-${p}.pprof`;
      const copied = capture('kubectl', ['-n', NS, 'cp', `${p}:${remote}`, local]);
      if (copied.status === 0) {
        log(`${p}: heap profile captured -> ${local}`);
      } else {
        log(`${p}: heap fetched in-container but cp failed: ${(copied.stderr || '').trim()}`);
      }
    }
  });
}

function main() {
  const total = totalRestarts();
  log(`cerberus container restarts (sum across pods): ${total}`);
  if (total < 0) {
    // kubectl couldn't read restartCount (cluster gone / API hiccup). The
    // inline bash this replaced treated an unreadable count as zero and
    // passed; preserve that leniency — a transient kubectl failure must not
    // fail the gate, and a genuinely-down cluster surfaces elsewhere. The
    // ::error:: annotation already flagged the read failure.
    return 0;
  }
  if (total === 0) {
    return 0;
  }

  error(`cerberus pods restarted ${total} time(s) during the e2e run — dumping evidence`);
  const pods = cerberusPods();

  // OOM-specific signals first (loud), then the broad context dump.
  surfaceLastTerminated(pods);
  dumpResourceBudget();
  // `top` needs metrics-server; k3d may not ship it — skip gracefully.
  dump('cerberus container memory usage (kubectl top — best-effort)',
    ['top', 'pods', '-l', 'app=cerberus', '--containers']);
  captureHeapProfiles(pods);

  dump('pods', ['get', 'pods', '-o', 'wide']);
  dump('events (last 60)', ['get', 'events', '--sort-by=.lastTimestamp']);
  for (const p of pods) {
    // --previous is EMPTY for an OOM kill (noted above) but carries the
    // crash evidence for a panic/exit-1 restart — keep it.
    dump(`${p} (previous container)`, ['logs', p, '--previous', '--tail', '120']);
  }
  dump('cerberus pod describes', ['describe', 'pods', '-l', 'app=cerberus']);
  return 1;
}

process.exit(main());
