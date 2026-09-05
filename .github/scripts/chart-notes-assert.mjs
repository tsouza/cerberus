// chart-notes-assert.mjs — behavioural render assertions for
// `templates/NOTES.txt`'s hot-volume capacity warning (cerberus issue #3075).
//
// `helm template` NEVER renders NOTES.txt (a long-standing Helm behavior —
// notes are only evaluated for `helm install`/`helm upgrade`, real or
// `--dry-run`, against a REACHABLE Kubernetes API), so this cannot run
// alongside chart-render-assert.mjs in the cluster-less `chart-validate` job.
// It runs instead in the `bwc-minio` e2e job, which already brings up a real
// k3d cluster — `helm install --dry-run` against it renders and returns NOTES
// without creating any resources.
//
// Asserts the independent trigger conditions for two NOTES.txt blocks:
//
// The capacity-warning block:
//   1. hotVolume.enabled && !hotVolume.persistence.enabled -> warning present,
//      naming `persistence.size`.
//   2. ...additionally naming the replica-count multiplier when
//      `bundled.replicas > 1`.
//   3. hotVolume.persistence.enabled -> no warning (dedicated PVC, no shared
//      capacity risk).
//   4. hotVolume disabled entirely (EXPLICIT — it defaults to true, hot-cold,
//      since #3075) -> no warning.
//
// The hot-cold-default upgrade-hazard warning (fires ONLY on `helm upgrade`,
// never a fresh `helm install`, of a bundled release resolving to hot-cold —
// #3075 flipped hotVolume.enabled's default to true, so an EXISTING
// deployment that never pinned it silently moves to hot-cold on upgrade):
//   5. Fresh install, hot-cold (bare default) -> no upgrade warning.
//   6. Upgrade, hot-cold (bare default) -> warning renders, naming the fix
//      (`hotVolume.enabled=false`).
//   7. Upgrade, EXPLICIT object-store mode (hotVolume.enabled=false) -> no
//      warning (already pinned, nothing to warn about).
//   8. Upgrade, hot-only mode -> no warning (not the hot-cold default).
//
// Env contract:
//   CHART_DIR   chart directory (default: deploy/helm/cerberus)
//   NAMESPACE   namespace to dry-run install into (default: default)
//
// Deps: node: builtins only. Requires `helm` on PATH and a reachable
// Kubernetes API (KUBECONFIG pointing at one — any throwaway cluster works).
// `notesFor` creates nothing (--dry-run install). `notesForUpgrade` DOES
// create (and then removes) one real, minimal-footprint throwaway release —
// bundled DISABLED — purely so a subsequent `helm upgrade --dry-run` against
// it genuinely sets .Release.IsUpgrade=true; the scenario under test rides
// the upgrade step's --set args, which is dry-run only (nothing real
// changes). This is unavoidable: `.Release.IsUpgrade` reflects Helm's action
// (install vs. upgrade) applied to a release Helm's storage backend already
// knows about, and no flag fakes that.
// Exit 1 on any failed assertion, 0 when all pass.

import { execFileSync } from 'node:child_process'
import { error as ghError, notice as ghNotice } from './lib/gh.mjs'

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus'
const NAMESPACE = process.env.NAMESPACE || 'default'

function extractNotes(out, how) {
  const idx = out.indexOf('NOTES:')
  if (idx === -1) throw new Error(`${how} produced no NOTES: section`)
  return out.slice(idx)
}

function notesFor(setArgs) {
  const out = execFileSync(
    'helm',
    ['install', 'notes-check', CHART_DIR, '--dry-run', '--namespace', NAMESPACE, ...setArgs],
    { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  return extractNotes(out, `helm install --dry-run (args: ${setArgs.join(' ')})`)
}

function notesForUpgrade(setArgs) {
  const relName = `notes-upg-${Date.now()}-${Math.floor(Math.random() * 1e6)}`
  execFileSync('helm', ['install', relName, CHART_DIR, '--namespace', NAMESPACE, '--wait=false'], {
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  })
  try {
    const out = execFileSync(
      'helm',
      ['upgrade', relName, CHART_DIR, '--dry-run', '--namespace', NAMESPACE, ...setArgs],
      { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
    )
    return extractNotes(out, `helm upgrade --dry-run (args: ${setArgs.join(' ')})`)
  } finally {
    try {
      execFileSync('helm', ['uninstall', relName, '--namespace', NAMESPACE, '--wait=false'], { encoding: 'utf8' })
    } catch {
      // best-effort cleanup; a leaked throwaway release in a throwaway/e2e
      // cluster is harmless and the cluster is torn down with the job anyway.
    }
  }
}

let ok = true
function check(cond, msg) {
  if (cond) {
    ghNotice(`PASS: ${msg}`)
  } else {
    ghError(`FAIL: ${msg}`)
    ok = false
  }
}

const WARNING_NEEDLE = 'WARNING: clickhouse.bundled.hotVolume.enabled is true'
const MULTIPLIER_NEEDLE = 'MULTIPLIES by clickhouse.bundled.replicas'

// --- 1 + 2. hotVolume on, no dedicated persistence: base warning, and the
// replica-count multiplier ONLY once replicas > 1. ---
{
  const single = notesFor(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=true'])
  check(single.includes(WARNING_NEEDLE), 'hotVolume enabled + no dedicated persistence: capacity warning renders')
  check(single.includes('persistence.size'), 'capacity warning names persistence.size')
  check(!single.includes(MULTIPLIER_NEEDLE), 'replicas=1 (default): no replica-count multiplier line')

  const multi = notesFor([
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.enabled=true',
    '--set', 'clickhouse.bundled.replicas=3',
  ])
  check(multi.includes(WARNING_NEEDLE), 'replicas=3: base capacity warning still renders')
  check(multi.includes(MULTIPLIER_NEEDLE), 'replicas=3: replica-count multiplier line renders, naming the count')
  check(multi.includes('(3)'), 'replica-count multiplier line names the actual replica count')
}

// --- 3. Dedicated hot-volume persistence: no capacity warning. ---
{
  const dedicated = notesFor([
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.persistence.enabled=true',
  ])
  check(!dedicated.includes(WARNING_NEEDLE), 'hotVolume.persistence.enabled: no capacity warning (dedicated PVC)')
}

// --- 4. hotVolume disabled entirely: no capacity warning. hotVolume.enabled
// defaults to true since #3075 (hot-cold is the chart's default storage
// mode), so this scenario needs an EXPLICIT false, not the bare default. ---
{
  const off = notesFor(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=false'])
  check(!off.includes(WARNING_NEEDLE), 'hotVolume disabled: no capacity warning')
}

// --- 5. Upgrade hazard: the hot-cold-default warning fires ONLY on an
// upgrade (never a fresh install) of a bundled release resolving to
// hot-cold, and never fires for object-store/hot-only modes. ---
{
  const UPGRADE_WARNING_NEEDLE = 'WARNING — UPGRADE:';

  const freshHotCold = notesFor(['--set', 'clickhouse.bundled.enabled=true']);
  check(!freshHotCold.includes(UPGRADE_WARNING_NEEDLE), 'fresh install (hot-cold, bare default): no upgrade-hazard warning');

  const upgradeHotCold = notesForUpgrade(['--set', 'clickhouse.bundled.enabled=true']);
  check(upgradeHotCold.includes(UPGRADE_WARNING_NEEDLE), 'upgrade + hot-cold (bare default): upgrade-hazard warning renders');
  check(upgradeHotCold.includes('hotVolume.enabled=false'), 'upgrade-hazard warning names the fix (hotVolume.enabled=false)');

  const upgradeObjectStore = notesForUpgrade(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=false']);
  check(!upgradeObjectStore.includes(UPGRADE_WARNING_NEEDLE), 'upgrade + explicit object-store mode: no upgrade-hazard warning (already pinned)');

  const upgradeHotOnly = notesForUpgrade([
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.objectStorage.enabled=false',
    '--set', 'schema.ttl=30d',
  ]);
  check(!upgradeHotOnly.includes(UPGRADE_WARNING_NEEDLE), 'upgrade + hot-only mode: no upgrade-hazard warning (not the hot-cold default)');
}

if (!ok) {
  ghError('chart-notes-assert FAILED')
  process.exit(1)
}
ghNotice('chart-notes-assert: all assertions passed')
process.exit(0)
