// chart-kubeconform.mjs — render the Helm chart for the default values and
// every ci/*-values.yaml fixture, schema-validate each rendered manifest set
// with kubeconform, and assert the rendered container image tag actually
// exists in the registry (the guard that would have caught an appVersion
// pointing at an unpublished tag).
//
// Env contract:
//   CHART_DIR   chart directory (default: deploy/helm/cerberus)
//   KUBE_VERSION  k8s API version to validate against (default: 1.28.0)
//   SKIP_IMAGE_CHECK  set to "1" to skip the registry existence probe
//                     (e.g. air-gapped CI); the probe is best-effort and
//                     only fails on a DEFINITIVE not-found, never on a
//                     transient registry/network error.
//
// Deps: node: builtins only. Requires `helm` + `kubeconform` on PATH
// (installed by the workflow via official actions) and, for the image
// probe, `docker` (anonymous manifest inspect works for public images).
//
// Exit 1 on any kubeconform failure or a definitively-missing image.

import { execFileSync } from 'node:child_process'
import { readdirSync } from 'node:fs'
import { join } from 'node:path'
import { error as ghError, notice as ghNotice } from './lib/gh.mjs'

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus'
const KUBE_VERSION = process.env.KUBE_VERSION || '1.28.0'
const SKIP_IMAGE_CHECK = process.env.SKIP_IMAGE_CHECK === '1'

function helmTemplate(valuesFile) {
  const args = ['template', 'release-name', CHART_DIR]
  if (valuesFile) args.push('-f', valuesFile)
  return execFileSync('helm', args, { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 })
}

function kubeconform(manifests, label) {
  try {
    execFileSync(
      'kubeconform',
      ['-strict', '-summary', '-kubernetes-version', KUBE_VERSION],
      { input: manifests, encoding: 'utf8', stdio: ['pipe', 'inherit', 'inherit'] },
    )
    return true
  } catch {
    ghError(`kubeconform failed for ${label}`)
    return false
  }
}

// Collect distinct `image:` references from rendered manifests.
function imagesIn(manifests) {
  const out = new Set()
  for (const m of manifests.matchAll(/^\s*image:\s*["']?([^"'\s]+)["']?\s*$/gm)) {
    out.add(m[1])
  }
  return [...out]
}

// Best-effort registry existence probe. Only a DEFINITIVE not-found fails
// the build; transient/auth errors are surfaced as a notice so a flaky
// registry never blocks a chart PR.
function imageExists(ref) {
  try {
    execFileSync('docker', ['manifest', 'inspect', ref], { stdio: 'ignore' })
    return 'present'
  } catch (e) {
    const msg = String(e.stderr || e.message || '')
    if (/manifest unknown|not found|no such manifest|MANIFEST_UNKNOWN|NAME_UNKNOWN|404/i.test(msg)) {
      return 'missing'
    }
    return 'unknown'
  }
}

const fixtures = [null]
try {
  for (const f of readdirSync(join(CHART_DIR, 'ci')).sort()) {
    if (f.endsWith('.yaml') || f.endsWith('.yml')) fixtures.push(join(CHART_DIR, 'ci', f))
  }
} catch {
  // no ci/ dir — defaults only
}

let ok = true
const seenImages = new Set()
for (const fixture of fixtures) {
  const label = fixture || '<defaults>'
  let rendered
  try {
    rendered = helmTemplate(fixture)
  } catch (e) {
    ghError(`helm template failed for ${label}: ${String(e.message || e)}`)
    ok = false
    continue
  }
  if (!kubeconform(rendered, label)) ok = false
  for (const img of imagesIn(rendered)) seenImages.add(img)
}

if (!SKIP_IMAGE_CHECK) {
  for (const ref of [...seenImages].sort()) {
    const state = imageExists(ref)
    if (state === 'missing') {
      ghError(`rendered image does not exist in the registry: ${ref} — the chart's appVersion/image.tag points at an unpublished tag`)
      ok = false
    } else if (state === 'unknown') {
      ghNotice(`could not verify image (transient/registry error, not failing): ${ref}`)
    } else {
      ghNotice(`image present: ${ref}`)
    }
  }
}

process.exit(ok ? 0 : 1)
