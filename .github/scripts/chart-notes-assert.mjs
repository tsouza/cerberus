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
// Asserts the two independent trigger conditions for the capacity-warning
// block:
//   1. hotVolume.enabled && !hotVolume.persistence.enabled -> warning present,
//      naming `persistence.size`.
//   2. ...additionally naming the replica-count multiplier when
//      `bundled.replicas > 1`.
//   3. hotVolume.persistence.enabled -> no warning (dedicated PVC, no shared
//      capacity risk).
//   4. hotVolume disabled entirely -> no warning.
//
// Env contract:
//   CHART_DIR   chart directory (default: deploy/helm/cerberus)
//   NAMESPACE   namespace to dry-run install into (default: default)
//
// Deps: node: builtins only. Requires `helm` on PATH and a reachable
// Kubernetes API (KUBECONFIG pointing at one — any throwaway cluster works,
// nothing is actually created).
// Exit 1 on any failed assertion, 0 when all pass.

import { execFileSync } from 'node:child_process'
import { error as ghError, notice as ghNotice } from './lib/gh.mjs'

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus'
const NAMESPACE = process.env.NAMESPACE || 'default'

function notesFor(setArgs) {
  const out = execFileSync(
    'helm',
    ['install', 'notes-check', CHART_DIR, '--dry-run', '--namespace', NAMESPACE, ...setArgs],
    { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  const idx = out.indexOf('NOTES:')
  if (idx === -1) throw new Error(`helm install --dry-run produced no NOTES: section (args: ${setArgs.join(' ')})`)
  return out.slice(idx)
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

// --- 4. hotVolume disabled entirely: no capacity warning. ---
{
  const off = notesFor(['--set', 'clickhouse.bundled.enabled=true'])
  check(!off.includes(WARNING_NEEDLE), 'hotVolume disabled: no capacity warning')
}

if (!ok) {
  ghError('chart-notes-assert FAILED')
  process.exit(1)
}
ghNotice('chart-notes-assert: all assertions passed')
process.exit(0)
