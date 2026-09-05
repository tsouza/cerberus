// chart-render-assert.mjs — behavioural render assertions for the cerberus Helm
// chart's HA-hardening paths that kubeconform (schema-only) cannot check:
//
//   1. Split-mode PodDisruptionBudget: `mode: split` + podDisruptionBudget
//      enabled renders ONE PDB per enabled head, each selecting only that head's
//      pods via app.kubernetes.io/component=<svc>. Disabling a head drops its
//      PDB. The monolith PDB render is unchanged (single aggregate PDB).
//   2. Derived GOMEMLIMIT: each container gets a GOMEMLIMIT env sized to ~80% of
//      THAT container's resources.limits.memory (per-head in split, per-pod in
//      monolith); an explicit extraEnv GOMEMLIMIT always wins; an unset limit
//      emits nothing.
//   9+ Bundled-ClickHouse hot/cold storage tiering (cerberus issue #3075): the
//      four-cell hotVolume x objectStorage matrix, the two `fail` guards, the
//      tierVolume/tierAfter auto-defaulting rule (and its fixed suppression
//      bug), the dedicated hot-volume PVC path, the NOTES.txt capacity
//      warning, and the ClickHouse Service `sessionAffinity` default/opt-out.
//
// Env contract:
//   CHART_DIR   chart directory (default: deploy/helm/cerberus)
//
// Deps: node: builtins only. Requires `helm` on PATH.
// Exit 1 on any failed assertion, 0 when all pass.

import { execFileSync } from 'node:child_process'
import { error as ghError, notice as ghNotice } from './lib/gh.mjs'

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus'

// ~80% headroom factor mirrored from cerberus.gomemlimitEnv (_helpers.tpl); the
// derived byte budget below the cgroup limit leaves room for off-heap memory.
const GOMEMLIMIT_HEADROOM = 0.8
const MiB = 1048576
const GiB = 1073741824

function tpl(args) {
  return execFileSync('helm', ['template', 'rn', CHART_DIR, ...args], {
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
}

// tplFail — render and expect `helm template` to FAIL. Returns the combined
// stderr+message text on failure (for asserting the error names the right
// keys), or null if the render unexpectedly succeeded.
function tplFail(args) {
  try {
    tpl(args)
    return null
  } catch (e) {
    return String(e.stderr || e.message || '')
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

// Count occurrences of a literal substring.
function count(haystack, needle) {
  return haystack.split(needle).length - 1
}

// --- 1. Split-mode per-head PDBs ----------------------------------------------
{
  const out = tpl(['-f', `${CHART_DIR}/ci/split-pdb-values.yaml`, '-s', 'templates/poddisruptionbudget.yaml'])
  check(count(out, 'kind: PodDisruptionBudget') === 3, 'split mode renders 3 PodDisruptionBudgets (one per enabled head)')
  for (const svc of ['prometheus', 'loki', 'tempo']) {
    check(out.includes(`name: rn-cerberus-${svc}`), `split PDB exists for head ${svc}`)
    check(
      count(out, `app.kubernetes.io/component: ${svc}`) === 2,
      `split PDB ${svc} carries its component selector (metadata label + matchLabels)`,
    )
  }

  // Disabling a head drops its PDB.
  const out2 = tpl([
    '-f', `${CHART_DIR}/ci/split-pdb-values.yaml`,
    '--set', 'split.loki.enabled=false',
    '-s', 'templates/poddisruptionbudget.yaml',
  ])
  check(count(out2, 'kind: PodDisruptionBudget') === 2, 'disabling a head drops its PDB (2 remain)')
  check(!out2.includes('name: rn-cerberus-loki'), 'disabled head loki has no PDB')
}

// --- 2. Monolith PDB unchanged (single aggregate PDB, no component selector) ---
{
  const out = tpl(['--set', 'podDisruptionBudget.enabled=true', '-s', 'templates/poddisruptionbudget.yaml'])
  check(count(out, 'kind: PodDisruptionBudget') === 1, 'monolith renders exactly 1 PDB')
  check(out.includes('name: rn-cerberus\n'), 'monolith PDB keeps the aggregate name')
  check(!/component: (prometheus|loki|tempo)/.test(out), 'monolith PDB has no per-head component selector')
}

// --- 3. Derived GOMEMLIMIT, monolith -----------------------------------------
{
  const out = tpl(['-s', 'templates/deployment.yaml'])
  const want = Math.floor(1536 * MiB * GOMEMLIMIT_HEADROOM)
  check(out.includes(`value: "${want}B"`), `monolith GOMEMLIMIT is ~80% of default 1536Mi limit (${want}B)`)
  check(count(out, 'name: GOMEMLIMIT') === 1, 'monolith emits exactly one GOMEMLIMIT')
}

// --- 4. Derived GOMEMLIMIT, per-head in split --------------------------------
{
  const out = tpl(['-f', `${CHART_DIR}/ci/split-pdb-values.yaml`, '-s', 'templates/split.yaml'])
  const lean = Math.floor(1 * GiB * GOMEMLIMIT_HEADROOM)
  const fat = Math.floor(4 * GiB * GOMEMLIMIT_HEADROOM)
  check(count(out, `value: "${lean}B"`) === 2, `prom + loki heads get 80%-of-1Gi GOMEMLIMIT (${lean}B x2)`)
  check(out.includes(`value: "${fat}B"`), `tempo head gets 80%-of-4Gi GOMEMLIMIT (${fat}B)`)
  check(count(out, 'name: GOMEMLIMIT') === 3, 'split emits one GOMEMLIMIT per head')
}

// --- 5. Explicit extraEnv GOMEMLIMIT wins (derived suppressed) ----------------
{
  const overrideArgs = ['--set-string', 'extraEnv[0].name=GOMEMLIMIT', '--set-string', 'extraEnv[0].value=2GiB']

  const mono = tpl([...overrideArgs, '-s', 'templates/deployment.yaml'])
  check(count(mono, 'name: GOMEMLIMIT') === 1, 'monolith: explicit GOMEMLIMIT is the only one (derived suppressed)')
  check(mono.includes('value: 2GiB'), 'monolith: explicit GOMEMLIMIT value wins')

  const split = tpl(['-f', `${CHART_DIR}/ci/split-pdb-values.yaml`, ...overrideArgs, '-s', 'templates/split.yaml'])
  check(count(split, 'name: GOMEMLIMIT') === 3, 'split: one explicit GOMEMLIMIT per head, no derived duplicate')
  check(count(split, 'value: 2GiB') === 3, 'split: explicit GOMEMLIMIT value wins on every head')
}

// --- 6. Unset memory limit emits no GOMEMLIMIT --------------------------------
{
  const out = tpl(['--set', 'resources=null', '-s', 'templates/deployment.yaml'])
  check(!out.includes('name: GOMEMLIMIT'), 'no memory limit set -> GOMEMLIMIT skipped silently')
}

// --- 7. admit.{prom,loki,tempo} accept an integer concurrency cap -------------
// Schema was boolean-only, which rejected an integer cap client-side even though
// the binary + template both honor it. Guard against a revert to boolean-only.
{
  const out = tpl(['--set', 'admit.prom=128', '-s', 'templates/configmap-env.yaml'])
  check(out.includes('CERBERUS_ADMIT_PROM: "128"'), 'admit.prom integer cap renders as CERBERUS_ADMIT_PROM="128"')

  let boolOk = true
  try {
    tpl(['--set', 'admit.loki=false', '-s', 'templates/configmap-env.yaml'])
  } catch {
    boolOk = false
  }
  check(boolOk, 'admit.loki boolean still accepted (toggle preserved)')

  let negRejected = false
  try {
    tpl(['--set', 'admit.tempo=-1', '-s', 'templates/configmap-env.yaml'])
  } catch {
    negRejected = true
  }
  check(negRejected, 'admit.tempo negative rejected (minimum:0 enforced)')
}

// --- 8. admit.tail is its own env knob, independent of admit.loki ------------
// The Loki /tail WebSocket holds its admission slot until the client
// disconnects, so it draws on CERBERUS_ADMIT_TAIL rather than the shared
// CERBERUS_ADMIT_LOKI request budget. A chart that dropped the key, or aliased
// it back onto admit.loki, would silently restore the starvation the split
// exists to prevent.
{
  const out = tpl(['--set', 'admit.tail=4', '--set', 'admit.loki=128', '-s', 'templates/configmap-env.yaml'])
  check(out.includes('CERBERUS_ADMIT_TAIL: "4"'), 'admit.tail integer cap renders as CERBERUS_ADMIT_TAIL="4"')
  check(out.includes('CERBERUS_ADMIT_LOKI: "128"'), 'admit.tail does not disturb CERBERUS_ADMIT_LOKI')

  let negRejected = false
  try {
    tpl(['--set', 'admit.tail=-1', '-s', 'templates/configmap-env.yaml'])
  } catch {
    negRejected = true
  }
  check(negRejected, 'admit.tail negative rejected (minimum:0 enforced)')
}

// --- 9. Object-store mode (default) unchanged: legacy disk/policy/volume
// shape, and the bundled ClickHouse Service gains ONLY sessionAffinity beyond
// its pre-existing spec fields. A repo-checked comparison of this branch's
// full render against origin/main (every ci/*-values.yaml fixture, plus
// defaults) additionally confirmed the ONLY diff anywhere in the chart is
// this sessionAffinity addition — see the PR description's test plan. This
// section pins the same invariant structurally so it keeps failing a future
// regression even after that one-time base comparison is no longer meaningful
// (main will eventually BE this code). ---
{
  const out = tpl(['--set', 'clickhouse.bundled.enabled=true', '-s', 'templates/clickhouse/configmap-config.yaml'])
  check(out.includes('<bwc_object_disk>'), 'object-store mode: legacy bwc_object_disk name unchanged')
  check(out.includes('<bwc_object_cache>'), 'object-store mode: legacy bwc_object_cache name unchanged')
  check(/<main>\s*<disk>bwc_object_cache<\/disk>\s*<\/main>/.test(out), 'object-store mode: legacy single "main" volume unchanged')
  check(out.includes('<bwc_object_store>'), 'object-store mode: default policy name bwc_object_store unchanged')
  check(!out.includes('bwc_hot_disk'), 'object-store mode: no hot disk rendered')

  const svc = tpl(['--set', 'clickhouse.bundled.enabled=true', '-s', 'templates/clickhouse/service.yaml'])
  const clusterIPSvc = svc.split('---')[1]
  const stripped = clusterIPSvc.replace(/\n\s*sessionAffinity:.*\n(\s*sessionAffinityConfig:\n(?:\s{4,}.*\n)*)?/, '\n')
  check(
    stripped.includes('type: ClusterIP') && stripped.includes('targetPort: native') && stripped.includes('targetPort: http')
      && !stripped.includes('sessionAffinity'),
    'ClickHouse ClusterIP Service: stripping sessionAffinity(+Config) leaves exactly the pre-existing spec (type/ports/selector) — sessionAffinity is the ONLY new field',
  )
}

// --- 10. hotVolume x objectStorage four-cell matrix + the two `fail` guards.
{
  const bothOff = tplFail(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.objectStorage.enabled=false'])
  check(bothOff !== null, 'hotVolume=false + objectStorage=false: render FAILS')
  check(
    bothOff && /hotVolume\.enabled/.test(bothOff) && /objectStorage\.enabled/.test(bothOff),
    'the both-disabled failure names BOTH hotVolume.enabled and objectStorage.enabled',
  )

  const hotOnlyNoTTL = tplFail([
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.enabled=true',
    '--set', 'clickhouse.bundled.objectStorage.enabled=false',
  ])
  check(hotOnlyNoTTL !== null, 'hot-only mode with schema.ttl unset: render FAILS')
  check(hotOnlyNoTTL && /schema\.ttl/.test(hotOnlyNoTTL), 'the hot-only-no-ttl failure names schema.ttl')

  const hotOnlyArgs = [
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.enabled=true',
    '--set', 'clickhouse.bundled.objectStorage.enabled=false',
    '--set', 'schema.ttl=30d',
  ]
  const hotOnly = tpl([...hotOnlyArgs, '-s', 'templates/clickhouse/configmap-config.yaml'])
  check(hotOnly.includes('<bwc_hot_only>'), 'hot-only mode: policy name bwc_hot_only')
  check(hotOnly.includes('<bwc_hot_disk>'), 'hot-only mode: local hot disk rendered')
  check(!hotOnly.includes('bwc_object_disk') && !hotOnly.includes('bwc_object_cache'), 'hot-only mode: NO object-store disk/cache rendered')
  check(!hotOnly.includes('<cold>'), 'hot-only mode: single volume only, no cold volume')

  const hotOnlyFull = tpl(hotOnlyArgs)
  check(!hotOnlyFull.includes('kind: Secret'), 'hot-only mode: no object-store Secret rendered')
  check(!/S3_ACCESS_KEY_ID|GCS_ACCESS_KEY_ID|AZURE_ACCOUNT_NAME/.test(hotOnlyFull), 'hot-only mode: no object-store credential env vars rendered')

  let hotColdShellS3 = null
  for (const [label, fixture] of [
    ['s3', 'ci/bwc-values.yaml'],
    ['gcs', 'ci/bwc-gcs-values.yaml'],
    ['azure', 'ci/bwc-azure-values.yaml'],
  ]) {
    const out = tpl(['-f', `${CHART_DIR}/${fixture}`, '--set', 'clickhouse.bundled.hotVolume.enabled=true', '-s', 'templates/clickhouse/configmap-config.yaml'])
    check(out.includes('<bwc_hot_cold>'), `hot-cold mode (${label}): policy name bwc_hot_cold`)
    check(out.includes('<bwc_hot_disk>'), `hot-cold mode (${label}): local hot disk rendered`)
    check(/<hot>\s*<disk>bwc_hot_disk<\/disk>\s*<\/hot>\s*<cold>\s*<disk>bwc_object_cache<\/disk>\s*<\/cold>/.test(out), `hot-cold mode (${label}): hot volume listed BEFORE cold`)
    check(/<move_factor>0\.2<\/move_factor>/.test(out), `hot-cold mode (${label}): default move_factor 0.2 rendered`)
    // Shape identical across backends: strip the backend-specific bwc_object_disk
    // inner block and the disks/policies shell must match byte-for-byte.
    const shell = out.replace(/<bwc_object_disk>[\s\S]*?<\/bwc_object_disk>/, '<bwc_object_disk/>')
    if (label === 's3') hotColdShellS3 = shell
    else check(shell === hotColdShellS3, `hot-cold mode (${label}): identical disk/volume shape to s3, only the cold disk's backend-specific block differs`)
  }
}

// --- 11. cerberus.bundled.apply tierVolume/tierAfter defaulting (+ the fixed
// suppression bug: a per-signal override must not suppress the base default).
{
  const hotColdArgs = ['-f', `${CHART_DIR}/ci/bwc-values.yaml`, '--set', 'clickhouse.bundled.hotVolume.enabled=true']
  const defaults = tpl([...hotColdArgs, '-s', 'templates/configmap-env.yaml'])
  check(defaults.includes('CERBERUS_SCHEMA_TIER_VOLUME: "cold"'), 'hot-cold mode: schema.tierVolume auto-defaults to "cold"')
  check(defaults.includes('CERBERUS_SCHEMA_TIER_AFTER: "7d"'), 'hot-cold mode: schema.tierAfter auto-defaults to "7d"')

  const operatorVolume = tpl([...hotColdArgs, '--set', 'schema.tierVolume=warm', '-s', 'templates/configmap-env.yaml'])
  check(operatorVolume.includes('CERBERUS_SCHEMA_TIER_VOLUME: "warm"'), 'an operator-set schema.tierVolume is never overridden')

  const operatorAfter = tpl([...hotColdArgs, '--set-string', 'schema.tierAfter=3d', '-s', 'templates/configmap-env.yaml'])
  check(operatorAfter.includes('CERBERUS_SCHEMA_TIER_AFTER: "3d"'), 'an operator-set schema.tierAfter is never overridden')

  // THE FIXED BUG: setting only the per-signal TIER_AFTER_METRICS override
  // (which rides the schema.<KEY> long-tail passthrough, NOT the typed base
  // tierAfter key) must NOT suppress the base tierAfter default that still
  // covers Logs/Traces.
  const perSignalOnly = tpl([...hotColdArgs, '--set-string', 'schema.TIER_AFTER_METRICS=3d', '-s', 'templates/configmap-env.yaml'])
  check(
    perSignalOnly.includes('CERBERUS_SCHEMA_TIER_AFTER: "7d"'),
    'setting ONLY schema.TIER_AFTER_METRICS still leaves the auto-defaulted schema.tierAfter=7d in place for Logs/Traces',
  )
  check(perSignalOnly.includes('CERBERUS_SCHEMA_TIER_AFTER_METRICS: "3d"'), 'the per-signal override itself still renders')
}

// --- 12. hotVolume.persistence.enabled: dedicated PVC + mount, hot disk XML
// path differs from the zero-new-PVC default subpath.
{
  const defaultPath = tpl(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=true', '--set', 'schema.ttl=30d', '-s', 'templates/clickhouse/configmap-config.yaml'])
  check(defaultPath.includes('<path>/var/lib/clickhouse/hot/</path>'), 'default hot volume: XML path is a subpath of the metadata mount')

  const dedicatedArgs = [
    '--set', 'clickhouse.bundled.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.enabled=true',
    '--set', 'schema.ttl=30d',
    '--set', 'clickhouse.bundled.hotVolume.persistence.enabled=true',
    '--set', 'clickhouse.bundled.hotVolume.persistence.size=50Gi',
  ]
  const dedicatedXML = tpl([...dedicatedArgs, '-s', 'templates/clickhouse/configmap-config.yaml'])
  check(dedicatedXML.includes('<path>/var/lib/clickhouse-hot/</path>'), 'dedicated hot volume: XML path points at the DISTINCT dedicated mount, not the metadata subpath')
  check(!dedicatedXML.includes('/var/lib/clickhouse/hot/'), 'dedicated hot volume: the default subpath is NOT also rendered')

  const dedicatedSts = tpl([...dedicatedArgs, '-s', 'templates/clickhouse/statefulset.yaml'])
  check(/name: hot\s*\n\s*mountPath: \/var\/lib\/clickhouse-hot/.test(dedicatedSts), 'dedicated hot volume: StatefulSet mounts a dedicated "hot" volume')
  check(count(dedicatedSts, 'name: hot\n') > 0, 'dedicated hot volume: volumeClaimTemplate "hot" section present')
  check(/name: hot[\s\S]*?storage: "50Gi"/.test(dedicatedSts), 'dedicated hot volume: volumeClaimTemplate sized from hotVolume.persistence.size')

  const nonDedicatedSts = tpl(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=true', '--set', 'schema.ttl=30d', '-s', 'templates/clickhouse/statefulset.yaml'])
  check(!nonDedicatedSts.includes('mountPath: /var/lib/clickhouse-hot'), 'default (non-dedicated) hot volume: no separate StatefulSet mount/PVC')
}

// --- 13. storagePolicyName operator override wins in every mode. ---
{
  for (const args of [
    ['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.storagePolicyName=custom_policy'],
    ['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=true', '--set', 'clickhouse.bundled.objectStorage.enabled=false', '--set', 'schema.ttl=30d', '--set', 'clickhouse.bundled.storagePolicyName=custom_policy'],
    ['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.hotVolume.enabled=true', '--set', 'clickhouse.bundled.storagePolicyName=custom_policy'],
  ]) {
    const out = tpl([...args, '-s', 'templates/clickhouse/configmap-config.yaml'])
    check(out.includes('<custom_policy>'), `operator-set storagePolicyName wins over the mode-derived default (args: ${args.join(' ')})`)
  }
}

// --- 14. ClickHouse Service sessionAffinity default-on / opt-out. ---
{
  const def = tpl(['--set', 'clickhouse.bundled.enabled=true', '-s', 'templates/clickhouse/service.yaml'])
  check(def.includes('sessionAffinity: ClientIP'), 'sessionAffinity defaults to ClientIP')
  check(def.includes('timeoutSeconds: 10800'), 'sessionAffinityTimeoutSeconds defaults to 10800')

  const customTimeout = tpl(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.service.sessionAffinityTimeoutSeconds=60', '-s', 'templates/clickhouse/service.yaml'])
  check(customTimeout.includes('timeoutSeconds: 60'), 'sessionAffinityTimeoutSeconds is overridable')

  const optOut = tpl(['--set', 'clickhouse.bundled.enabled=true', '--set', 'clickhouse.bundled.service.sessionAffinity=None', '-s', 'templates/clickhouse/service.yaml'])
  check(optOut.includes('sessionAffinity: None'), 'sessionAffinity: "None" disables affinity')
  check(!optOut.includes('sessionAffinityConfig'), 'sessionAffinity: "None" omits sessionAffinityConfig entirely')
}

process.exit(ok ? 0 : 1)
