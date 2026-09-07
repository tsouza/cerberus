// Unit + integration tests for dump-cluster-state.mjs.
//
// The unit half pins the two verdicts — the disk-full threshold and the
// pressure signals — against the exact false-positive that motivated them:
// the bare field names `kubectl describe nodes` prints on a HEALTHY node
// must never fire the infra breadcrumb. The integration half runs the real
// script with stub `kubectl` / `docker` / `df` binaries on PATH, so the
// substrate switch (NAMESPACE vs COMPOSE vs neither), the per-pod log
// enumeration, the seed-log tail and the always-exit-0 contract are
// exercised end to end without a cluster.

import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  DISK_FULL_PERCENT,
  INFRA_PRESSURE_SIGNALS,
  RESOURCE_PRESSURE_SIGNALS,
  rootUsagePercent,
  verdicts,
} from './dump-cluster-state.mjs';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'dump-cluster-state.mjs');

// What `kubectl describe nodes` prints on a healthy node — every one of these
// carries the bare words the old guards grepped for.
const HEALTHY_NODE = [
  'Conditions:',
  '  DiskPressure     False   KubeletHasNoDiskPressure   kubelet has no disk pressure',
  '  MemoryPressure   False   KubeletHasSufficientMemory kubelet has sufficient memory',
  'Capacity:',
  '  ephemeral-storage:  151764168Ki',
].join('\n');

// --- verdicts ------------------------------------------------------------------

test('a healthy node dump fires no verdict', () => {
  assert.deepEqual(verdicts({ state: HEALTHY_NODE, usagePercent: 40 }), []);
});

test('each affirmative pressure signal fires the infra verdict; the healthy forms do not', () => {
  for (const signal of [
    'write /var/lib/foo: no space left on device',
    '  DiskPressure     True    KubeletHasDiskPressure',
    'Warning  EvictionThresholdMet  KubeletHasDiskPressure',
    'NodeHasDiskPressure',
    'pod/cerberus-abc   0/1   Evicted',
  ]) {
    assert.ok(INFRA_PRESSURE_SIGNALS.test(signal), `must match: ${signal}`);
    const out = verdicts({ state: `${HEALTHY_NODE}\n${signal}`, usagePercent: 40 });
    assert.equal(out.length, 1, signal);
    assert.match(out[0], /^infra: /);
  }
  for (const healthy of ['KubeletHasNoDiskPressure', 'DiskPressure  False', 'ephemeral-storage: 1Ki']) {
    assert.ok(!INFRA_PRESSURE_SIGNALS.test(healthy), `must not match: ${healthy}`);
  }
});

test('root-fs usage at the threshold fires the infra verdict; just under it does not', () => {
  assert.equal(verdicts({ state: '', usagePercent: DISK_FULL_PERCENT - 1 }).length, 0);
  const out = verdicts({ state: '', usagePercent: DISK_FULL_PERCENT });
  assert.equal(out.length, 1);
  assert.match(out[0], new RegExp(`${DISK_FULL_PERCENT}% used`));
});

test('an unknown root-fs usage never fires the disk-full arm', () => {
  assert.deepEqual(verdicts({ state: '', usagePercent: null }), []);
});

test('resource pressure is its own verdict, independent of the infra one', () => {
  for (const signal of [
    'Warning  FailedScheduling  0/1 nodes are available: 1 Insufficient memory.',
    '0/1 nodes are available: 1 Insufficient cpu.',
    'Last State: Terminated  Reason: OOMKilled',
  ]) {
    assert.ok(RESOURCE_PRESSURE_SIGNALS.test(signal), `must match: ${signal}`);
    const out = verdicts({ state: signal, usagePercent: 40 });
    assert.equal(out.length, 1, signal);
    assert.match(out[0], /^resource: /);
  }
  const both = verdicts({ state: 'Evicted\nOOMKilled', usagePercent: 40 });
  assert.deepEqual(both.map((v) => v.split(':')[0]), ['infra', 'resource']);
});

test('rootUsagePercent parses df --output=pcent and rejects anything else', () => {
  assert.equal(rootUsagePercent('Use%\n 42%\n'), 42);
  assert.equal(rootUsagePercent('Use%\n100%'), 100);
  assert.equal(rootUsagePercent(''), null);
  assert.equal(rootUsagePercent('df: /: No such file or directory'), null);
});

// --- integration: the real script over stub binaries ----------------------------

// stubBin — a directory holding executable `kubectl` / `docker` / `df`
// shell stubs that print canned output per invocation shape, prepended to
// PATH for the run. `usage` is what df reports for /; `pods` are the pod
// names kubectl enumerates; `describe` is what `describe pods` says.
function stubBin({ usage = 40, pods = [], describe = 'no pods', composeServices = [] } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'dump-cluster-state-bin-'));
  const write = (name, body) => {
    const p = join(dir, name);
    writeFileSync(p, `#!/bin/sh\n${body}\n`);
    chmodSync(p, 0o755);
  };
  write('df', `case "$*" in *pcent*) printf 'Use%%\\n %s%%\\n' ${usage} ;; *) echo "Filesystem Size Used Avail Use% Mounted" ;; esac`);
  write('kubectl', [
    'case "$*" in',
    `  *"get pods -o jsonpath"*) printf '%s' '${pods.join(' ')}' ;;`,
    `  *"describe pods"*) echo '${describe}' ;;`,
    '  *"describe nodes"*) echo "DiskPressure False KubeletHasNoDiskPressure" ;;',
    '  *"logs"*"--previous"*) echo "previous log of $*" ;;',
    '  *"logs"*) echo "current log of $*" ;;',
    '  *) echo "kubectl $*" ;;',
    'esac',
  ].join('\n'));
  write('docker', [
    'case "$*" in',
    `  *"config --services"*) printf '%s\\n' ${composeServices.map((s) => `'${s}'`).join(' ') || "''"} ;;`,
    '  *"logs"*) echo "compose log of $*" ;;',
    '  *) echo "docker $*" ;;',
    'esac',
  ].join('\n'));
  return dir;
}

function runScript(env, bin) {
  return spawnSync(process.execPath, [SCRIPT], {
    encoding: 'utf8',
    env: { ...process.env, ...env, PATH: `${bin}:${process.env.PATH}` },
  });
}

test('a k3d lane dumps every pod (current + previous) and exits 0', () => {
  const bin = stubBin({ pods: ['cerberus-a', 'cerberus-b', 'clickhouse-0'] });
  const res = runScript({ NAMESPACE: 'cerberus', COMPOSE: '', SEED_LOG: '' }, bin);
  assert.equal(res.status, 0, res.stderr);
  for (const pod of ['cerberus-a', 'cerberus-b', 'clickhouse-0']) {
    assert.match(res.stdout, new RegExp(`current log of -n cerberus logs ${pod} --all-containers --tail \\d+`));
    assert.match(res.stdout, new RegExp(`previous log of -n cerberus logs ${pod} --all-containers --previous --tail \\d+`));
  }
  assert.match(res.stdout, /::group::describe pods in cerberus/);
  assert.match(res.stdout, /::group::pods \(all namespaces/);
  assert.doesNotMatch(res.stdout, /::notice::/, 'a healthy dump carries no breadcrumb');
});

test('an OOMKilled pod in describe output surfaces the resource breadcrumb', () => {
  const bin = stubBin({ pods: ['clickhouse-0'], describe: 'Last State: Terminated Reason: OOMKilled' });
  const res = runScript({ NAMESPACE: 'cerberus', COMPOSE: '', SEED_LOG: '' }, bin);
  assert.equal(res.status, 0);
  assert.match(res.stdout, /::notice::resource: pod\(s\) reported "OOMKilled"/);
  assert.doesNotMatch(res.stdout, /::notice::infra/);
});

test('a full root fs surfaces the infra breadcrumb even with a healthy cluster', () => {
  const bin = stubBin({ usage: DISK_FULL_PERCENT });
  const res = runScript({ NAMESPACE: 'cerberus', COMPOSE: '', SEED_LOG: '' }, bin);
  assert.equal(res.status, 0);
  assert.match(res.stdout, /::notice::infra: real disk-full/);
});

test('a compose lane dumps every defined service and never touches kubectl', () => {
  const bin = stubBin({ composeServices: ['clickhouse', 'cerberus', 'seed'] });
  const res = runScript({ NAMESPACE: '', COMPOSE: 'true', SEED_LOG: '' }, bin);
  assert.equal(res.status, 0);
  assert.match(res.stdout, /::group::compose ps/);
  for (const service of ['clickhouse', 'cerberus', 'seed']) {
    assert.match(res.stdout, new RegExp(`compose log of compose logs --tail \\d+ ${service}`));
  }
  assert.doesNotMatch(res.stdout, /kubectl/);
});

test('with neither substrate only the disk usage is dumped, and SEED_LOG files are tailed', () => {
  const seedDir = mkdtempSync(join(tmpdir(), 'dump-cluster-state-seed-'));
  const present = join(seedDir, 'rolling.log');
  writeFileSync(present, 'seed line 1\nseed line 2\n');
  const absent = join(seedDir, 'missing.log');
  const bin = stubBin();
  const res = runScript({ NAMESPACE: '', COMPOSE: '', SEED_LOG: `${present} ${absent}` }, bin);
  assert.equal(res.status, 0);
  assert.match(res.stdout, /::group::disk usage/);
  assert.doesNotMatch(res.stdout, /kubectl|compose/);
  assert.match(res.stdout, /seed line 2/);
  assert.match(res.stdout, new RegExp(`::group::${absent.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}[^\n]*\n\\(absent\\)`));
});

test('an ENOSPC line in a tailed seed log is an infra signal too', () => {
  const seedDir = mkdtempSync(join(tmpdir(), 'dump-cluster-state-seed-'));
  const path = join(seedDir, 'rolling.log');
  writeFileSync(path, 'insert failed: no space left on device\n');
  const res = runScript({ NAMESPACE: '', COMPOSE: '', SEED_LOG: path }, stubBin());
  assert.equal(res.status, 0);
  assert.match(res.stdout, /::notice::infra: .*no space left on device/);
});

test('a missing binary is reported and does not fail the dump', () => {
  // An empty PATH entry ahead of a broken one: `kubectl` resolves to nothing
  // the runner can execute, which capture() reports as exit 127.
  const bin = mkdtempSync(join(tmpdir(), 'dump-cluster-state-nobin-'));
  const res = spawnSync(process.execPath, [SCRIPT], {
    encoding: 'utf8',
    env: { ...process.env, NAMESPACE: 'cerberus', COMPOSE: '', SEED_LOG: '', PATH: bin },
  });
  assert.equal(res.status, 0, res.stderr);
  assert.match(res.stdout, /\(exit 127\)/);
});
