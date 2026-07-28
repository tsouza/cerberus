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
import { readdirSync, readFileSync } from 'node:fs'
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

// Collect distinct `image:` references from YAML text. Applied to the rendered
// manifests AND to Chart.yaml itself, whose `artifacthub.io/images` annotation
// advertises a `v`-prefixed ref that no rendered manifest carries — a second
// tag shape, from a second goreleaser `tags:` entry, that nothing probed while
// only rendered output was scanned.
export function imagesIn(yamlText) {
  const out = new Set()
  for (const m of yamlText.matchAll(/^\s*image:\s*["']?([^"'\s]+)["']?\s*$/gm)) {
    out.add(m[1])
  }
  return [...out]
}

// Best-effort registry existence probe. Only a DEFINITIVE not-found fails
// the build; transient/auth errors are surfaced as a notice so a flaky
// registry never blocks a chart PR.
//
// stderr MUST be piped. `docker manifest inspect` writes its verdict there
// ("manifest unknown"), and with `stdio: 'ignore'` the thrown error carries
// `stderr === null` and a `message` of only "Command failed: docker manifest
// inspect <ref>" — no phrase the classifier below can match. Every failure,
// including a definitive not-found, therefore fell through to 'unknown' and
// was downgraded to a notice: the `chart-validate` required check could not
// fail on the one condition it exists to catch.
export function classifyProbeFailure(err) {
  const msg = String(err.stderr || err.message || '')
  if (/manifest unknown|not found|no such manifest|MANIFEST_UNKNOWN|NAME_UNKNOWN|404/i.test(msg)) {
    return 'missing'
  }
  return 'unknown'
}

// Named so the self-test can pin it. classifyProbeFailure is pure and would go
// on passing its own assertions if this reverted to `stdio: 'ignore'` — the
// defect lived here, not in the classifier, so this is what has to be gated.
export const PROBE_SPAWN_OPTS = { stdio: ['ignore', 'ignore', 'pipe'], encoding: 'utf8' }

function imageExists(ref) {
  try {
    execFileSync('docker', ['manifest', 'inspect', ref], PROBE_SPAWN_OPTS)
    return 'present'
  } catch (e) {
    return classifyProbeFailure(e)
  }
}

// The appVersion this very change stages, or null if it is unchanged.
//
// A release PR bumps appVersion to a tag that does not exist yet — publishing
// it is what MERGING the PR does — and chart-ci runs again on the merge commit
// concurrently with release.yml's publish. So the newly-staged tag is expected
// to be absent on both events, and only that tag: every other missing image is
// still fatal. Returns null when the base cannot be resolved, which withholds
// the exemption rather than widening it.
export function stagedAppVersion(headYaml, baseYaml) {
  const head = appVersionOf(headYaml)
  const base = appVersionOf(baseYaml)
  if (!head || !base || head === base) return null
  return head
}

export function appVersionOf(chartYaml) {
  const m = /^appVersion:\s*["']?([^"'\s]+)["']?\s*$/m.exec(chartYaml || '')
  return m ? m[1] : null
}

// A ref is exempt only if its tag is exactly the staged appVersion or its
// `v`-prefixed twin — the two shapes .goreleaser.yml publishes.
export function isStagedRef(ref, staged) {
  if (!staged) return false
  const tag = ref.slice(ref.lastIndexOf(':') + 1)
  return tag === staged || tag === `v${staged}`
}

function gitShow(ref, path) {
  try {
    return execFileSync('git', ['show', `${ref}:${path}`], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] })
  } catch {
    return null
  }
}

// --- self-test: pins the pure classification/exemption logic ---------------
//
// Each case that guards a regression also proves the OLD behaviour is gone,
// rather than only that the new behaviour is present: `stdio: 'ignore'`
// produced an error object with `stderr === null` and a bare "Command failed"
// message, and that exact shape must classify as 'missing' now that stderr is
// piped — asserting only on a piped-stderr fixture would pass either way.
function selfTest() {
  const fails = []
  let passes = 0
  const check = (cond, msg) => {
    if (cond) passes++
    else fails.push(msg)
  }

  // The defect was the spawn options, not the classifier. Pin them directly:
  // without a piped stderr every branch below is unreachable in production.
  check(PROBE_SPAWN_OPTS.stdio[2] === 'pipe', 'the probe pipes stderr — the classifier sees nothing otherwise')
  check(PROBE_SPAWN_OPTS.encoding === 'utf8', 'probe stderr is decoded to a string, not a Buffer')

  const notFound = { stderr: 'manifest unknown\n', message: 'Command failed: docker manifest inspect x' }
  check(classifyProbeFailure(notFound) === 'missing', 'piped not-found stderr classifies as missing')

  // The regression itself: with stdio 'ignore' this is all the probe ever saw.
  const swallowed = { stderr: null, message: 'Command failed: docker manifest inspect ghcr.io/tsouza/cerberus:1.13.1' }
  check(
    classifyProbeFailure(swallowed) === 'unknown',
    'stderr-less failure is unknown — proves the guard was inert until stderr was piped',
  )

  for (const transient of ['unauthorized: authentication required', 'toomanyrequests: retry later', 'tls: handshake timeout']) {
    check(classifyProbeFailure({ stderr: transient }) === 'unknown', `transient stays unknown: ${transient}`)
  }

  check(appVersionOf('appVersion: "1.13.0"\n') === '1.13.0', 'appVersion parsed through quotes')
  check(appVersionOf('appVersion: 1.13.0\n') === '1.13.0', 'appVersion parsed unquoted')
  check(appVersionOf('version: 0.13.0\n') === null, 'chart version is not mistaken for appVersion')

  const base = 'appVersion: "1.13.0"\n'
  const bumped = 'appVersion: "1.13.1"\n'
  check(stagedAppVersion(bumped, base) === '1.13.1', 'a bump stages the new appVersion')
  check(stagedAppVersion(base, base) === null, 'an unchanged appVersion stages nothing')
  check(stagedAppVersion(bumped, null) === null, 'an unresolvable base withholds the exemption')

  check(isStagedRef('ghcr.io/tsouza/cerberus:1.13.1', '1.13.1'), 'bare staged tag is exempt')
  check(isStagedRef('ghcr.io/tsouza/cerberus:v1.13.1', '1.13.1'), 'v-prefixed staged tag is exempt')
  check(!isStagedRef('ghcr.io/tsouza/cerberus:1.13.0', '1.13.1'), 'the PREVIOUS release is not exempt')
  check(!isStagedRef('grafana/grafana:12.2.9', '1.13.1'), 'a third-party image is never exempt')
  check(!isStagedRef('ghcr.io/tsouza/cerberus:1.13.1', null), 'no staged version means no exemption at all')

  // The annotation ref is a second tag shape that only Chart.yaml carries.
  const chartYaml = 'annotations:\n  artifacthub.io/images: |\n    - name: cerberus\n      image: ghcr.io/tsouza/cerberus:v1.13.0\n'
  check(imagesIn(chartYaml).includes('ghcr.io/tsouza/cerberus:v1.13.0'), 'artifacthub.io/images annotation is probed')

  for (const bad of ['1.13', '1.13.O', 'v1.13.1', '']) {
    check(!APP_VERSION_RE.test(bad), `malformed appVersion rejected offline: ${JSON.stringify(bad)}`)
  }
  check(APP_VERSION_RE.test('1.13.1') && APP_VERSION_RE.test('1.14.0-rc.1'), 'release and pre-release appVersions accepted')

  if (fails.length) {
    for (const f of fails) ghError(`chart-kubeconform --self-test FAIL: ${f}`)
    process.exit(1)
  }
  ghNotice(`chart-kubeconform --self-test: all ${passes} assertions passed`)
  process.exit(0)
}

const APP_VERSION_RE = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/

if (process.argv.includes('--self-test')) selfTest()

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

const headChartYaml = readFileSync(join(CHART_DIR, 'Chart.yaml'), 'utf8')
for (const img of imagesIn(headChartYaml)) seenImages.add(img)

// A malformed appVersion cannot be caught by the registry probe once the
// staging exemption below is in play — `1.13.O` would be "the tag this change
// stages" and get waved through. Assert the shape offline instead, where no
// exemption applies.
const headAppVersion = appVersionOf(headChartYaml)
if (!headAppVersion || !APP_VERSION_RE.test(headAppVersion)) {
  ghError(`chart appVersion is not a semver release: ${JSON.stringify(headAppVersion)}`)
  ok = false
}

// pull_request runs compare against the PR base; push runs against the parent
// of the pushed commit. Either way the question is the same: does THIS change
// stage a new appVersion?
const baseRef = process.env.GITHUB_BASE_REF ? `origin/${process.env.GITHUB_BASE_REF}` : 'HEAD^'
const staged = stagedAppVersion(headChartYaml, gitShow(baseRef, join(CHART_DIR, 'Chart.yaml')))

if (!SKIP_IMAGE_CHECK) {
  for (const ref of [...seenImages].sort()) {
    const state = imageExists(ref)
    if (state === 'missing' && isStagedRef(ref, staged)) {
      ghNotice(`image not published yet, as expected — this change stages appVersion ${staged}: ${ref}`)
    } else if (state === 'missing') {
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
