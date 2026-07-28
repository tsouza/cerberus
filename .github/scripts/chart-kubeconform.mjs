// chart-kubeconform.mjs — render the Helm chart for the default values and
// every ci/*-values.yaml fixture, schema-validate each rendered manifest set
// with kubeconform, and assert the rendered container image tag actually
// exists in the registry (the guard against an appVersion pointing at an
// unpublished tag).
//
// Env contract:
//   CHART_DIR   chart directory (default: deploy/helm/cerberus)
//   KUBE_VERSION  k8s API version to validate against (default: 1.28.0)
//   SKIP_IMAGE_CHECK  set to "1" to skip the registry existence probe
//                     entirely (air-gapped CI). This is the only waiver:
//                     when the probe runs, a reference it cannot positively
//                     verify fails the check.
//
// Deps: node: builtins only. Requires `helm` + `kubeconform` on PATH
// (installed by the workflow via official actions) and, for the image
// probe, `docker` (anonymous manifest inspect works for public images).
//
// Exit 1 on any kubeconform failure, any missing image, and any image whose
// existence the probe could not establish after its bounded retries.

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
// tag shape, from a second goreleaser `tags:` entry, which scanning only the
// rendered output would leave unprobed.
export function imagesIn(yamlText) {
  const out = new Set()
  for (const m of yamlText.matchAll(/^\s*image:\s*["']?([^"'\s]+)["']?\s*$/gm)) {
    out.add(m[1])
  }
  return [...out]
}

// classifyProbeFailure reads the registry's own words out of the probe's
// stderr. `docker manifest inspect` writes its verdict there ("manifest
// unknown"), so stderr has to be piped for any branch here to be reachable:
// with stderr closed, execFileSync's error carries `stderr === null` and a
// message of only "Command failed: docker manifest inspect <ref>", which
// matches no phrase below and classifies as 'unknown'. The self-test drives
// the real probe against a failing command, so the piping is asserted at the
// call site rather than as a constant sitting next to it.
export function classifyProbeFailure(err) {
  const msg = String(err.stderr || err.message || '')
  if (/manifest unknown|not found|no such manifest|MANIFEST_UNKNOWN|NAME_UNKNOWN|404/i.test(msg)) {
    return 'missing'
  }
  return 'unknown'
}

const PROBE_SPAWN_OPTS = { stdio: ['ignore', 'ignore', 'pipe'], encoding: 'utf8' }

// The probe command, taken as an argument so the self-test can exercise this
// exact execFileSync call — same options object, same error handling — against
// a command whose failure it controls.
const DOCKER_MANIFEST_INSPECT = (ref) => ['docker', ['manifest', 'inspect', ref]]

function probeOnce(ref, probeCmd) {
  const [file, args] = probeCmd(ref)
  try {
    execFileSync(file, args, PROBE_SPAWN_OPTS)
    return 'present'
  } catch (e) {
    return classifyProbeFailure(e)
  }
}

// Attempts and backoff mirror the Justfile's `_pull-retry`, which exists for
// this same registry and this same failure: the chart renders a Docker Hub
// ref (clickhouse-server), and Docker Hub answers a concurrency burst from CI
// with `toomanyrequests` even on an authenticated runner. Since 'unknown'
// FAILS the required `chart-validate` check, one refusal would otherwise fail
// every open PR at once. Retrying is not tolerance — tolerance would be
// accepting a non-verdict as a pass; this is giving the probe enough chances
// to reach a verdict at all.
const PROBE_ATTEMPTS = 5
const PROBE_BACKOFF_STEP_MS = 3_000

// Synchronous sleep: the whole script is synchronous, and the retry policy is
// injectable so the self-test drives the loop without sleeping through it.
const sleepSync = (ms) => {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}
const PROBE_RETRY = { attempts: PROBE_ATTEMPTS, sleep: sleepSync }

// imageExists returns the probe's verdict, retrying ONLY while it has none.
// 'present' and 'missing' are verdicts and return immediately — re-probing a
// definitive not-found would add PROBE_ATTEMPTS worth of backoff to every
// release PR, whose staged appVersion is legitimately absent.
export function imageExists(ref, probeCmd = DOCKER_MANIFEST_INSPECT, retry = PROBE_RETRY) {
  let state = 'unknown'
  for (let attempt = 1; attempt <= retry.attempts; attempt++) {
    state = probeOnce(ref, probeCmd)
    if (state !== 'unknown') return state
    if (attempt < retry.attempts) retry.sleep(attempt * PROBE_BACKOFF_STEP_MS)
  }
  return state
}

// probeVerdict maps a probe state onto the check's verdict. An image counts as
// verified only when the probe positively confirmed it, so 'unknown' — an
// auth/permission refusal, a rate limit, a DNS or TLS failure — is FATAL: a
// guard that reached no verdict has verified nothing, and reporting green on
// it means the `chart-validate` required check passes precisely when the guard
// is broken. `SKIP_IMAGE_CHECK=1` is the explicit, visible waiver for runs with
// no registry access. The staged-appVersion exemption covers a DEFINITIVE
// not-found only: an unverifiable staged ref is still unverified.
export function probeVerdict(state, isStaged) {
  if (state === 'present') return 'ok'
  if (state === 'missing' && isStaged) return 'staged'
  return 'fail'
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

// --- self-test: pins the probe call site and the verdict/exemption logic ---
//
// The image guard is a chain — spawn options, classifier, verdict — and only
// the middle link is pure. So the probe assertions run the REAL `imageExists`
// against a command whose failure they control, instead of asserting on the
// options object: a call site that stops piping stderr, or a verdict that
// downgrades a failed probe to a notice, has to turn one of them red.

// The phrases a controlled probe writes to stderr: a registry's not-found
// verdict, and a refusal that says nothing about whether the image exists.
const NOT_FOUND_STDERR = 'manifest unknown'
const NO_VERDICT_STDERR = 'unauthorized: authentication required'

// probeCmd factories for the self-test: a node subprocess that writes `phrase`
// to stderr and exits non-zero, and one that succeeds silently.
//
// The phrase travels through argv base64-encoded because execFileSync's error
// MESSAGE quotes the whole command line — a plaintext phrase there would reach
// the classifier through `message` even with stderr closed, and the assertion
// would then pass against a call site that pipes nothing. The fixture must
// leave stderr as the only channel it can arrive on.
const failingProbe = (phrase) => () => [
  process.execPath,
  [
    '-e',
    `process.stderr.write(Buffer.from('${Buffer.from(phrase).toString('base64')}', 'base64').toString()); process.exit(1)`,
  ],
]
const succeedingProbe = () => [process.execPath, ['-e', '']]

// One command per probe state, so a test can script what each attempt sees.
const probeFor = (state) =>
  state === 'present' ? succeedingProbe() : failingProbe(state === 'missing' ? NOT_FOUND_STDERR : NO_VERDICT_STDERR)()

// scriptedProbe hands out one command per attempt from `states` (the last
// entry repeats) and counts the attempts, so a test can assert not just the
// outcome but how many times the registry was actually asked.
const scriptedProbe = (states) => {
  const calls = { count: 0 }
  return {
    calls,
    cmd: () => probeFor(states[Math.min(calls.count++, states.length - 1)]),
  }
}

// The real attempt count with the sleeping replaced by a recorder: the retry
// loop runs exactly as it does in CI, without the self-test waiting out the
// backoff.
const recordedRetry = (slept) => ({ attempts: PROBE_ATTEMPTS, sleep: (ms) => slept.push(ms) })

function selfTest() {
  const fails = []
  let passes = 0
  const check = (cond, msg) => {
    if (cond) passes++
    else fails.push(msg)
  }

  // End-to-end through the real spawn: the registry's not-found phrase only
  // reaches the classifier if the call site pipes stderr, so a call site that
  // closes it turns this from 'missing' into 'unknown'.
  check(
    imageExists('probe-self-test', failingProbe(NOT_FOUND_STDERR), recordedRetry([])) === 'missing',
    'the probe call site pipes stderr — a registry not-found reaches the classifier',
  )
  check(
    imageExists('probe-self-test', failingProbe(NO_VERDICT_STDERR), recordedRetry([])) === 'unknown',
    'a refusal that is not a not-found reaches the classifier as unknown',
  )
  check(
    imageExists('probe-self-test', succeedingProbe, recordedRetry([])) === 'present',
    'a probe that exits 0 reports the image present',
  )

  // Retry: the point is to REACH a verdict, so a probe that has none yet gets
  // another go, and one that has a verdict is asked exactly once.
  const recovered = scriptedProbe(['unknown', 'present'])
  check(
    probeVerdict(imageExists('probe-self-test', recovered.cmd, recordedRetry([])), false) === 'ok',
    'a rate-limited probe that then answers ends verified — the retry is what reaches the verdict',
  )
  check(recovered.calls.count === 2, 'retrying stops the moment the probe reaches a verdict')

  const slept = []
  const neverAnswers = scriptedProbe(['unknown'])
  check(
    probeVerdict(imageExists('probe-self-test', neverAnswers.cmd, recordedRetry(slept)), false) === 'fail',
    'a probe that reaches no verdict in any attempt still fails the check — retry is not tolerance',
  )
  check(neverAnswers.calls.count === PROBE_ATTEMPTS, 'every attempt is spent before the check gives up')
  const expectedBackoff = Array.from({ length: PROBE_ATTEMPTS - 1 }, (_, i) => (i + 1) * PROBE_BACKOFF_STEP_MS)
  check(
    slept.join() === expectedBackoff.join(),
    'backoff grows linearly between attempts, and nothing waits after the last one',
  )

  const unpublished = scriptedProbe(['missing'])
  check(
    imageExists('probe-self-test', unpublished.cmd, recordedRetry([])) === 'missing',
    'a not-found probe reports missing',
  )
  check(
    unpublished.calls.count === 1,
    'a definitive not-found is a verdict and is never retried — a staged appVersion must not pay the backoff',
  )

  const published = scriptedProbe(['present'])
  check(imageExists('probe-self-test', published.cmd, recordedRetry([])) === 'present', 'a published image reports present')
  check(published.calls.count === 1, 'a confirmed image is asked for once')

  // The fixture itself: its command line must carry no phrase the classifier
  // recognises, or the two assertions above would pass on `message` alone.
  const [file, args] = failingProbe(NOT_FOUND_STDERR)('probe-self-test')
  check(
    classifyProbeFailure({ stderr: null, message: `Command failed: ${file} ${args.join(' ')}` }) === 'unknown',
    'the probe fixture reaches the classifier on stderr only, never through the quoted command line',
  )

  const notFound = { stderr: 'manifest unknown\n', message: 'Command failed: docker manifest inspect x' }
  check(classifyProbeFailure(notFound) === 'missing', 'piped not-found stderr classifies as missing')

  // A probe whose stderr never arrives carries only "Command failed", which is
  // no verdict at all — and no verdict fails the check.
  const swallowed = { stderr: null, message: 'Command failed: docker manifest inspect ghcr.io/tsouza/cerberus:1.13.1' }
  check(classifyProbeFailure(swallowed) === 'unknown', 'a stderr-less failure is unknown, never a silent pass')

  // None of these says the image is absent, and none of them says it is there
  // either: each classifies as unknown, and unknown fails the check.
  for (const noVerdict of [NO_VERDICT_STDERR, 'toomanyrequests: retry later', 'tls: handshake timeout']) {
    check(classifyProbeFailure({ stderr: noVerdict }) === 'unknown', `not a not-found verdict: ${noVerdict}`)
    check(probeVerdict(classifyProbeFailure({ stderr: noVerdict }), false) === 'fail', `probe failure fails the check: ${noVerdict}`)
  }

  check(probeVerdict('present', false) === 'ok', 'a confirmed image passes')
  check(probeVerdict('missing', false) === 'fail', 'an unpublished image fails')
  check(probeVerdict('missing', true) === 'staged', 'a definitive not-found on the staged appVersion is exempt')
  check(
    probeVerdict('unknown', false) === 'fail',
    'an unverifiable image fails the check — a probe that reached no verdict verified nothing',
  )
  check(
    probeVerdict('unknown', true) === 'fail',
    'the staging exemption covers a definitive not-found only, never an unverifiable ref',
  )

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
    switch (probeVerdict(state, isStagedRef(ref, staged))) {
      case 'ok':
        ghNotice(`image present: ${ref}`)
        break
      case 'staged':
        ghNotice(`image not published yet, as expected — this change stages appVersion ${staged}: ${ref}`)
        break
      default:
        ghError(
          state === 'missing'
            ? `rendered image does not exist in the registry: ${ref} — the chart's appVersion/image.tag points at an unpublished tag`
            : `could not verify image ${ref} — the registry probe was retried ${PROBE_ATTEMPTS} times with backoff and still ` +
                `reached no verdict, so the image-existence guard did not run. That is a registry that is down, refusing, or ` +
                `rate-limiting, not a single flaky call. Fix the registry access (or set SKIP_IMAGE_CHECK=1 for an air-gapped ` +
                `run); an unverified image is not a verified one.`,
        )
        ok = false
    }
  }
}

process.exit(ok ? 0 : 1)
