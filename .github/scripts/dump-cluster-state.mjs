// dump-cluster-state.mjs — the "dump state on failure" step every e2e and
// compatibility lane runs, extracted from twelve near-identical inline
// `run:` blocks (seven in e2e.yml across the compose-smoke, dashboard,
// chaos, bwc-minio, datashard and datashard-replica-affinity lanes; five in
// compatibility.yml) that had drifted apart in what they dumped and in the
// disk-full threshold they used to decide whether a red run was an infra
// flake. One module, one threshold, one set of pressure signals.
//
// Purely diagnostic: it prints, it never asserts, and it always exits 0 —
// a dump step that itself failed would hide the failure it was there to
// explain. Every subprocess it runs is best-effort (a missing binary, a
// resource that does not exist on this lane, a torn-down stack all print
// their own error and move on).
//
// What it dumps depends on which substrate the lane runs on:
//
//   NAMESPACE set   — a k3d cluster: root-fs usage, `kubectl describe
//                     nodes` (capacity, allocatable, conditions), every pod
//                     across all namespaces (scheduling contention), the
//                     namespace's events, network policies and workload
//                     objects, `describe pods` (Pending / CrashLoop reasons,
//                     resource requests), and the current AND previous-
//                     container logs of EVERY pod in the namespace — never
//                     `logs deploy/...`, which picks one pod and on a crash-
//                     looping Deployment is usually a healthy survivor (run
//                     27249217602 had two cerberus pods in CrashLoopBackOff
//                     and the old dump captured nothing about why; the
//                     --previous tail is the crash evidence). Enumerating
//                     every pod rather than a per-lane list of workloads is
//                     what let twelve blocks become one: a lane with MinIO,
//                     a Keeper ensemble or four data-shard StatefulSets
//                     needs no lane-specific dump code.
//   COMPOSE=true    — a Docker Compose stack: root-fs usage, `docker compose
//                     ps`, and the last LOG_TAIL_LINES of every service the
//                     compose project defines.
//   neither         — the compatibility lanes, whose harness tears its own
//                     compose stack down on exit: root-fs usage alone, which
//                     is all the runner still has to say.
//
// SEED_LOG names local log files to tail after the substrate dump (the
// rolling seeder + its port-forward supervisor, which run on the runner
// itself rather than in the cluster), whitespace-separated; a missing file
// is reported and skipped.
//
// Then two verdicts, both `::notice::` breadcrumbs for whoever triages the
// red run — never a pass/fail:
//
//   infra:    a REAL disk-full / ephemeral-storage eviction signal. Fires on
//             the affirmative-pressure strings only (INFRA_PRESSURE_SIGNALS)
//             — never on the bare field names `DiskPressure` /
//             `ephemeral-storage`, which `kubectl describe nodes` prints on
//             every HEALTHY node (`DiskPressure  False`,
//             `KubeletHasNoDiskPressure`, `ephemeral-storage: <cap>Ki`) and
//             which used to make the breadcrumb fire on every k3d failure
//             regardless of cause, mislabelling real regressions as "safe
//             to re-run" — or on root-fs usage at/above DISK_FULL_PERCENT.
//   resource: a pod reported insufficient cpu/memory, FailedScheduling or an
//             OOMKill (RESOURCE_PRESSURE_SIGNALS) — the shape a sized-down
//             lane (the datashard N=4 leg on one k3d node) fails in when the
//             runner is simply too small, as distinct from a code failure.
//
// Env contract:
//   NAMESPACE   k8s namespace to dump (unset = not a k3d lane)
//   COMPOSE     "true" to dump the Docker Compose stack in the cwd
//   SEED_LOG    whitespace-separated local log paths to tail (optional)
//
// Exit: always 0.

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, group, log, notice } from './lib/gh.mjs';

// DISK_FULL_PERCENT — root-fs usage at or above which the infra breadcrumb
// fires. The twelve inline blocks disagreed: the k3d lanes said "root >= 90%
// used", the compatibility lanes said "< 2 GB free". The percent form is
// the one kept because it is the one the kubelet itself evicts on: its
// default eviction signal is `nodefs.available < 10%` — a FRACTION of the
// filesystem, not a byte count — so 90% used is exactly where the k3d
// lanes' DiskPressure evictions begin. A fixed 2 GB is 1.4% of the 145 GB
// runners this repo actually gets and would fire only long after eviction
// had already happened (and on a 14 GB runner it would fire at 86%, a
// different threshold per machine). The compose and compatibility lanes
// have no kubelet, but "root is critically full" is the same question
// there, and one constant beats two.
export const DISK_FULL_PERCENT = 90;

// LOG_TAIL_LINES / PREVIOUS_LOG_TAIL_LINES — how much of a container's
// current and previous-container log to print. The previous-container tail
// is shorter because it is read for ONE reason (the crash's last words) and
// is printed for every pod, restarted or not, so it must stay cheap.
const LOG_TAIL_LINES = 200;
const PREVIOUS_LOG_TAIL_LINES = 100;

// EVENTS_TAIL_LINES — the newest namespace events to print. Events are
// sorted by lastTimestamp, so the tail is the most recent.
const EVENTS_TAIL_LINES = 100;

// INFRA_PRESSURE_SIGNALS — the affirmative disk-pressure signals, none of
// which appear on a healthy node:
//   no space left on device   - the kernel ENOSPC string (also the only one
//                               a compose container log can carry)
//   DiskPressure +True        - the node condition flipped True
//   KubeletHasDiskPressure    - the pressure event (NOT the healthy
//   NodeHasDiskPressure         'KubeletHasNoDiskPressure' / 'No' form)
//   Evicted                   - a pod actually evicted
export const INFRA_PRESSURE_SIGNALS =
  /no space left on device|DiskPressure +True|KubeletHasDiskPressure|NodeHasDiskPressure|Evicted/i;

// RESOURCE_PRESSURE_SIGNALS — a pod that could not be scheduled or was
// killed for lack of cpu/memory, as `kubectl describe pods` / `get pods`
// report it.
export const RESOURCE_PRESSURE_SIGNALS = /Insufficient (?:cpu|memory)|OOMKilled|FailedScheduling/i;

// rootUsagePercent — the integer usage percentage out of `df --output=pcent
// /` ("Use%\n 42%"), or null when the output is not that shape.
export function rootUsagePercent(dfOutput) {
  const lines = String(dfOutput ?? '').trim().split('\n');
  const m = /(\d+)%/.exec(lines[lines.length - 1] ?? '');
  return m ? Number(m[1]) : null;
}

// verdicts — the breadcrumbs to emit, as a pure function of what was
// collected: `state` is the substrate dump text the pressure signals are
// searched in, `usagePercent` the root-fs usage (null when unknown).
export function verdicts({ state, usagePercent }) {
  const out = [];
  const infra = INFRA_PRESSURE_SIGNALS.exec(String(state ?? ''));
  const full = usagePercent !== null && usagePercent >= DISK_FULL_PERCENT;
  if (infra || full) {
    const why = infra ? `signal ${JSON.stringify(infra[0])} in the dumped state` : `root fs ${usagePercent}% used (>= ${DISK_FULL_PERCENT}%)`;
    out.push(`infra: real disk-full / ephemeral-storage eviction detected (${why}) — this is an infra flake, safe to re-run on a fresh runner`);
  }
  const resource = RESOURCE_PRESSURE_SIGNALS.exec(String(state ?? ''));
  if (resource) {
    out.push(`resource: pod(s) reported ${JSON.stringify(resource[0])} — insufficient cpu/memory, FailedScheduling or an OOMKill; see the describe-pods dump above`);
  }
  return out;
}

// run — one best-effort subprocess: prints its stdout and stderr under a
// collapsible group, returns the stdout (so the caller can accumulate the
// text the verdicts search), and never throws.
function run(title, cmd, args) {
  let out = '';
  group(title, () => {
    const res = capture(cmd, args);
    out = res.stdout;
    if (res.stdout) process.stdout.write(res.stdout.endsWith('\n') ? res.stdout : `${res.stdout}\n`);
    if (res.stderr) process.stdout.write(res.stderr.endsWith('\n') ? res.stderr : `${res.stderr}\n`);
    if (res.status !== 0) log(`(exit ${res.status})`);
  });
  return out;
}

function words(value) {
  return String(value ?? '').split(/\s+/).filter(Boolean);
}

function dumpKubernetes(namespace) {
  let state = '';
  state += run('node capacity/allocatable + conditions', 'kubectl', ['describe', 'nodes']);
  state += run('pods (all namespaces, for scheduling contention)', 'kubectl', ['get', 'pods', '-A', '-o', 'wide']);
  const events = run(`events in ${namespace} (newest ${EVENTS_TAIL_LINES})`, 'kubectl', [
    '-n', namespace, 'get', 'events', '--sort-by=.lastTimestamp',
  ]);
  state += events.split('\n').slice(-EVENTS_TAIL_LINES).join('\n');
  run(`network policies in ${namespace}`, 'kubectl', ['-n', namespace, 'get', 'networkpolicies', '-o', 'wide']);
  run(`workloads in ${namespace}`, 'kubectl', ['-n', namespace, 'get', 'deployments,statefulsets,daemonsets,jobs', '-o', 'wide']);
  state += run(`describe pods in ${namespace} (Pending/CrashLoop reasons, resource requests)`, 'kubectl', [
    '-n', namespace, 'describe', 'pods',
  ]);
  const pods = words(capture('kubectl', ['-n', namespace, 'get', 'pods', '-o', 'jsonpath={.items[*].metadata.name}']).stdout);
  for (const pod of pods) {
    state += run(`${pod} logs (current, last ${LOG_TAIL_LINES})`, 'kubectl', [
      '-n', namespace, 'logs', pod, '--all-containers', '--tail', String(LOG_TAIL_LINES),
    ]);
    state += run(`${pod} logs (previous container, if restarted, last ${PREVIOUS_LOG_TAIL_LINES})`, 'kubectl', [
      '-n', namespace, 'logs', pod, '--all-containers', '--previous', '--tail', String(PREVIOUS_LOG_TAIL_LINES),
    ]);
  }
  return state;
}

function dumpCompose() {
  let state = '';
  state += run('compose ps', 'docker', ['compose', 'ps']);
  const services = words(capture('docker', ['compose', 'config', '--services']).stdout);
  for (const service of services) {
    state += run(`${service} logs (last ${LOG_TAIL_LINES})`, 'docker', [
      'compose', 'logs', '--tail', String(LOG_TAIL_LINES), service,
    ]);
  }
  return state;
}

function dumpSeedLogs(paths) {
  let state = '';
  for (const path of paths) {
    group(`${path} (last ${LOG_TAIL_LINES})`, () => {
      if (!existsSync(path)) {
        log('(absent)');
        return;
      }
      const tail = readFileSync(path, 'utf8').split('\n').slice(-LOG_TAIL_LINES).join('\n');
      state += tail;
      process.stdout.write(tail.endsWith('\n') ? tail : `${tail}\n`);
    });
  }
  return state;
}

function main() {
  const namespace = process.env.NAMESPACE || '';
  const compose = process.env.COMPOSE === 'true';
  const seedLogs = words(process.env.SEED_LOG);

  run('disk usage (DiskPressure self-diagnosis)', 'df', ['-h']);
  const usagePercent = rootUsagePercent(capture('df', ['--output=pcent', '/']).stdout);

  let state = '';
  if (namespace) state += dumpKubernetes(namespace);
  if (compose) state += dumpCompose();
  state += dumpSeedLogs(seedLogs);

  for (const verdict of verdicts({ state, usagePercent })) notice(verdict);
}

// Runs only when this file is the program; dump-cluster-state.test.mjs
// imports the pure halves above.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
