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

process.exit(ok ? 0 : 1)
