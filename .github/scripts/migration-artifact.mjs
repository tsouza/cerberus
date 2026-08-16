// migration-artifact.mjs — resolve WHICH cerberus the Layer-14 migration lane
// tests, on every path, and refuse an incoherent input pair.
//
// migration-e2e.yml runs twice against the same commit for a release: once
// against the source tree and once via `workflow_call` against the sealed,
// unpublished candidate. Both runs execute the same three tier jobs, so the
// one thing that differs is which binary the scenarios exec and which image
// the compose stacks run. This module is that single decision point.
//
// Two plans, and nothing in between:
//
//   source  no image was supplied — `go build ./cmd/cerberus` produces the CLI
//           (stamped `dev`, cmd/cerberus's `var Version`), and the compose
//           stacks build their server from Dockerfile.local (stamped `e2e`).
//           The CLI and the server are DIFFERENT builds, so there is no single
//           stamp both report and `sharedVersion` is empty: the Go harness
//           then holds each side to its own known-correct default rather than
//           to one wrong shared value. Both sides still assert.
//   candidate  a sealed candidate was supplied — its complete byte roster,
//              source identity and candidate digest are verified, then its OCI
//              layout is copied into an ephemeral loopback registry. The CLI
//              is extracted from that exact digest, so the binary the scenarios
//              exec and the server the stack runs are literally the same bytes.
//
// Supplying only part of the candidate identity is a hard error, never a silent
// fall-back to `source`: that is precisely how a release lane would end up
// testing a source build under an artifact-shaped job name and reporting green.
//
// Env:
//   CERBERUS_CANDIDATE_DIR_INPUT   sealed candidate root; empty = source mode.
//   CERBERUS_CANDIDATE_DIGEST_INPUT exact candidate manifest digest.
//   CERBERUS_EXPECT_VERSION_INPUT  version that image's binary must report;
//                                  travels with the candidate identity.
//   CERBERUS_SOURCE_SHA_INPUT      exact candidate source commit.
//   CERBERUS_SOURCE_TREE_INPUT     exact candidate source tree.
//   COMPOSE_PROJECT_SUFFIX         per-checkout suffix the local image tag
//                                  carries, so the tag named here is the one
//                                  the compose stack runs. Empty in a primary
//                                  checkout, which is every CI checkout.
//   GITHUB_ENV                     runner file the resolved plan is exported
//                                  through (CERBERUS_BIN / CERBERUS_IMAGE /
//                                  CERBERUS_EXPECT_VERSION and, in candidate
//                                  mode, CERBERUS_CANDIDATE_REGISTRY_CONTAINER).
//
// Exit: 0 with the plan exported; 1 on a half-supplied input pair, a failed
// build / pull / extraction, or a `--version` probe that does not report
// exactly the expected stamp.

import { mkdirSync, chmodSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';

import { capture, error, exportEnv, notice } from './lib/gh.mjs';
import { pullImageWithRetry } from './lib/registry.mjs';
import { verifyCandidate, verifyCheckout } from './release-candidate.mjs';

// SOURCE_BUILD_VERSION is what `go build ./cmd/cerberus` with no ldflags
// reports — cmd/cerberus/main.go's `var Version = "dev"`. Held equal to that
// declaration by test/regression/migration_tier1_test.go, which reads both.
export const SOURCE_BUILD_VERSION = 'dev';

// COMPOSE_SUFFIX_REF is the per-checkout suffix interpolation every compose
// project name and every locally built image tag in this repo ends with. Each
// consumer expands it in its own runtime: compose interpolates it, a Justfile
// recipe lets its shell do it, and expandComposeSuffix does it here, so all
// three name one image. Empty in a primary checkout, which is what every CI
// checkout is. See scripts/compose-project-suffix.sh.
const COMPOSE_SUFFIX_REF = '${COMPOSE_PROJECT_SUFFIX:-}';

function expandComposeSuffix(ref) {
  return ref.replace(COMPOSE_SUFFIX_REF, process.env.COMPOSE_PROJECT_SUFFIX ?? '');
}

// LOCAL_IMAGE_TAG is the tag the compose stacks run when no candidate is
// supplied. Held equal to the `${CERBERUS_IMAGE:-…}` default in
// tiers/tier1-dual/docker-compose.dual.yml and to the Justfile's
// MIGRATION_LOCAL_IMAGE by the same regression pin.
export const LOCAL_IMAGE_TAG = expandComposeSuffix('cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}');

// IMAGE_BINARY_PATH is where the cerberus image keeps its binary — the same
// path Dockerfile.local's and Dockerfile's `COPY --from=build` write to, and
// the same one the compose healthcheck execs.
export const IMAGE_BINARY_PATH = '/usr/local/bin/cerberus';

// BUILD_DIR is the repo-relative directory the resolved binary lands in, and
// BINARY_NAME the filename it lands under. Both are constants rather than env
// knobs on purpose: the joined path is handed to capture() as the command to
// execute, so an env-supplied component would let whoever sets the environment
// choose which program this module runs and then stamps as "the cerberus under
// test". No workflow ever set the knob this replaced.
const BUILD_DIR = 'build';
const BINARY_NAME = 'cerberus';

// EXECUTABLE_MODE is what an extracted binary is chmod'ed to. `docker cp`
// preserves the image's mode, but an image built elsewhere is not this lane's
// to trust.
const EXECUTABLE_MODE = 0o755;

const LOOPBACK_REGISTRY_HOST = '127.0.0.1';
const REGISTRY_CONTAINER_PREFIX = 'cerberus-migration-candidate-registry';
const REGISTRY_IMAGE = 'registry:2';
const REGISTRY_START_ATTEMPTS = 10;
const REGISTRY_RETRY_DELAY_SECONDS = 1;
const REGISTRY_CONTAINER_PORT = '5000/tcp';
const LOOPBACK_ENDPOINT_RE = /^127\.0\.0\.1:[1-9][0-9]{0,4}$/;
const REGISTRY_CONTAINER_RE = /^cerberus-migration-candidate-registry-[1-9][0-9]*$/;
export const CANDIDATE_REGISTRY_ENV = 'CERBERUS_CANDIDATE_REGISTRY_CONTAINER';

// PAIRED_INPUTS_MESSAGE is the refusal for a half-supplied pair. Its exact text
// is asserted by migration-artifact.test.mjs, so a loosening rewrite is caught.
export const PAIRED_INPUTS_MESSAGE =
  'candidate_dir, candidate_digest, expect_version, source_sha, and source_tree travel together: ' +
  'supply all five (test the unpublished candidate) or none (build from this tree). Supplying a ' +
  'partial identity would run unbound bytes under an artifact-shaped job name.';

// resolvePlan is the testable core: it decides which cerberus the lane drives
// from the two inputs alone, with no I/O.
//
// Returns { source, image, expectVersion, sharedVersion } where
//   expectVersion  the stamp the CLI this module produces must report;
//   sharedVersion  the stamp EVERY cerberus in the lane reports (CLI and
//                  server), or '' when the two come from different builds.
export function resolvePlan({ candidateDir, candidateDigest, expectVersion, sourceSHA, sourceTree } = {}) {
  const candidate = String(candidateDir ?? '').trim();
  const digest = String(candidateDigest ?? '').trim();
  const want = String(expectVersion ?? '').trim();
  const sha = String(sourceSHA ?? '').trim();
  const tree = String(sourceTree ?? '').trim();
  const supplied = [candidate, digest, want, sha, tree].filter((value) => value !== '').length;

  if (supplied === 0) {
    return {
      source: 'source',
      image: LOCAL_IMAGE_TAG,
      expectVersion: SOURCE_BUILD_VERSION,
      sharedVersion: '',
    };
  }
  if (supplied !== 5) {
    throw new Error(PAIRED_INPUTS_MESSAGE);
  }
  return {
    source: 'candidate',
    candidateDir: candidate,
    candidateDigest: digest,
    sourceSHA: sha,
    sourceTree: tree,
    expectVersion: want,
    sharedVersion: want,
  };
}

export function loopbackRegistryEndpoint(output) {
  const endpoints = String(output ?? '')
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean);
  if (endpoints.length !== 1 || !LOOPBACK_ENDPOINT_RE.test(endpoints[0])) {
    throw new Error(
      `loopback registry published endpoint is ${JSON.stringify(endpoints)}, want one 127.0.0.1:<port>`,
    );
  }
  const port = Number(endpoints[0].slice(endpoints[0].lastIndexOf(':') + 1));
  if (!Number.isSafeInteger(port) || port > 65535) {
    throw new Error(`loopback registry published an invalid TCP port ${JSON.stringify(port)}`);
  }
  return endpoints[0];
}

export function stopLoopbackRegistry(container, runner = capture) {
  const name = String(container ?? '').trim();
  if (name === '') return false;
  if (!REGISTRY_CONTAINER_RE.test(name)) {
    throw new Error(`refusing to remove invalid candidate registry container ${JSON.stringify(name)}`);
  }
  const stopped = runner('docker', ['rm', '-f', name]);
  if (stopped.status !== 0) {
    if (/no such container/i.test(`${stopped.stdout}\n${stopped.stderr}`)) return false;
    throw new Error(
      `loopback registry ${name} could not be removed:\n${stopped.stdout}${stopped.stderr}`,
    );
  }
  return true;
}

function startLoopbackRegistry() {
  if (!pullImageWithRetry(REGISTRY_IMAGE, { consequence: 'the candidate OCI layout cannot be loaded' })) {
    throw new Error(`cannot acquire the loopback registry substrate ${REGISTRY_IMAGE}`);
  }
  const container = `${REGISTRY_CONTAINER_PREFIX}-${process.pid}`;
  capture('docker', ['rm', '-f', container]);
  const started = capture('docker', [
    'run',
    '--detach',
    '--rm',
    '--name',
    container,
    '--publish',
    `${LOOPBACK_REGISTRY_HOST}::5000`,
    REGISTRY_IMAGE,
  ]);
  if (started.status !== 0) {
    throw new Error(`loopback registry failed to start:\n${started.stdout}${started.stderr}`);
  }
  const published = capture('docker', ['port', container, REGISTRY_CONTAINER_PORT]);
  try {
    if (published.status !== 0) {
      throw new Error(
        `cannot resolve the loopback registry port:\n${published.stdout}${published.stderr}`,
      );
    }
    const endpoint = loopbackRegistryEndpoint(published.stdout);
    return { container, repository: `${endpoint}/cerberus` };
  } catch (cause) {
    capture('docker', ['rm', '-f', container]);
    throw cause;
  }
}

function loadCandidate(plan) {
  let candidate;
  try {
    verifyCheckout(plan.sourceSHA, plan.sourceTree);
    candidate = verifyCandidate(path.resolve(plan.candidateDir), {
      sha: plan.sourceSHA,
      tree: plan.sourceTree,
      app: plan.expectVersion,
      appTag: `v${plan.expectVersion}`,
      digest: plan.candidateDigest,
    });
  } catch (cause) {
    throw new Error(`candidate verification failed:\n${cause.message}`);
  }

  const registry = startLoopbackRegistry();
  const source = `${path.resolve(plan.candidateDir, candidate.manifest.image.layout)}:${plan.expectVersion}`;
  const taggedRef = `${registry.repository}:${plan.expectVersion}`;
  let copied = null;
  for (let attempt = 1; attempt <= REGISTRY_START_ATTEMPTS; attempt += 1) {
    copied = capture('oras', ['cp', '--from-oci-layout', '--to-plain-http', source, taggedRef]);
    if (copied.status === 0) break;
    if (attempt < REGISTRY_START_ATTEMPTS) {
      capture('sleep', [String(REGISTRY_RETRY_DELAY_SECONDS)]);
    }
  }
  if (copied?.status !== 0) {
    capture('docker', ['rm', '-f', registry.container]);
    throw new Error(
      `failed to load candidate OCI layout:\n${copied?.stdout ?? ''}${copied?.stderr ?? ''}`,
    );
  }
  const resolved = capture('oras', ['resolve', '--plain-http', taggedRef]);
  const actualDigest = resolved.stdout.trim();
  if (resolved.status !== 0 || actualDigest !== candidate.manifest.image.digest) {
    capture('docker', ['rm', '-f', registry.container]);
    throw new Error(
      `loopback image resolved to ${JSON.stringify(actualDigest)}, want ` +
        `${candidate.manifest.image.digest}:\n${resolved.stdout}${resolved.stderr}`,
    );
  }
  return {
    container: registry.container,
    image: `${registry.repository}@${candidate.manifest.image.digest}`,
  };
}

// buildFromSource compiles the CLI the scenarios exec out of this tree.
function buildFromSource(binary) {
  const res = capture('go', ['build', '-o', binary, './cmd/cerberus']);
  if (res.status !== 0) {
    throw new Error(`go build ./cmd/cerberus failed:\n${res.stdout}${res.stderr}`);
  }
}

// extractFromImage pulls the candidate image and copies its binary out, so the
// CLI the scenarios exec is the same bytes as the server the stack runs.
function extractFromImage(ref, binary) {
  // Through the shared policy rather than a bare `docker pull`: a transport
  // fault gets the same retry budget every other registry fetch gets, and a
  // quota refusal fails on the first attempt instead of spending four more of
  // an exhausted counter. `acceptLocalCopy` is deliberately off — the whole
  // point is to run the candidate bytes, so a stale copy the runner happens to
  // hold must not stand in for them.
  if (!pullImageWithRetry(ref, { consequence: 'the candidate binary cannot be extracted' })) {
    throw new Error(
      `${ref} is unpullable from this job — if the failure above is not a registry ` +
        'fault, check that the loopback candidate registry is reachable.',
    );
  }

  const created = capture('docker', ['create', ref]);
  if (created.status !== 0) {
    throw new Error(`docker create ${ref} failed:\n${created.stdout}${created.stderr}`);
  }
  const container = created.stdout.trim().split('\n').pop().trim();
  if (container === '') {
    throw new Error(`docker create ${ref} printed no container id`);
  }

  try {
    const cp = capture('docker', ['cp', `${container}:${IMAGE_BINARY_PATH}`, binary]);
    if (cp.status !== 0) {
      throw new Error(
        `docker cp ${container}:${IMAGE_BINARY_PATH} failed — the image does ` +
          `not carry the binary at that path:\n${cp.stdout}${cp.stderr}`,
      );
    }
  } finally {
    // Best-effort: the container is a throwaway created only to be copied out
    // of, and a leaked one on an ephemeral runner cannot affect the verdict.
    capture('docker', ['rm', '-f', container]);
  }
  chmodSync(binary, EXECUTABLE_MODE);
}

// probeVersion runs the resolved binary's `--version` and holds it to the
// expected stamp. This is what turns a stale, wrong-arch or wrong-tree image
// into a named failure before a single scenario runs.
function probeVersion(binary, plan) {
  const res = capture(binary, ['--version']);
  if (res.status !== 0) {
    throw new Error(
      `${binary} --version exited ${res.status} (plan=${plan.source}, ` +
        `image=${plan.image}):\n${res.stdout}${res.stderr}`,
    );
  }
  const lines = res.stdout.split('\n').map((l) => l.trim()).filter((l) => l !== '');
  if (lines.length !== 1) {
    throw new Error(
      `${binary} --version printed ${lines.length} non-empty line(s), want ` +
        `exactly one bare version: ${JSON.stringify(res.stdout)}`,
    );
  }
  if (lines[0] !== plan.expectVersion) {
    throw new Error(
      `${binary} reports version "${lines[0]}" but the ${plan.source} plan ` +
        `expects "${plan.expectVersion}" (image=${plan.image}). The lane would have tested a ` +
        `different build than the one it claims to.`,
    );
  }
  return lines[0];
}

function resolveArtifact() {
  const plan = resolvePlan({
    candidateDir: process.env.CERBERUS_CANDIDATE_DIR_INPUT,
    candidateDigest: process.env.CERBERUS_CANDIDATE_DIGEST_INPUT,
    expectVersion: process.env.CERBERUS_EXPECT_VERSION_INPUT,
    sourceSHA: process.env.CERBERUS_SOURCE_SHA_INPUT,
    sourceTree: process.env.CERBERUS_SOURCE_TREE_INPUT,
  });

  const buildDir = path.resolve(BUILD_DIR);
  mkdirSync(buildDir, { recursive: true });
  const binary = path.join(buildDir, BINARY_NAME);

  let loaded = null;
  try {
    if (plan.source === 'source') {
      buildFromSource(binary);
    } else {
      loaded = loadCandidate(plan);
      plan.image = loaded.image;
      extractFromImage(plan.image, binary);
    }

    const version = probeVersion(binary, plan);

    exportEnv([
      ['CERBERUS_BIN', binary],
      ['CERBERUS_IMAGE', plan.image],
      ['CERBERUS_EXPECT_VERSION', plan.sharedVersion],
      [CANDIDATE_REGISTRY_ENV, loaded?.container ?? ''],
    ]);

    notice(
      `migration-artifact: ${plan.source} plan — binary ${binary} reports ${version}, ` +
        `compose runs ${plan.image}` +
        (plan.sharedVersion === ''
          ? ' (CLI and server are separate builds; each is held to its own stamp)'
          : ` (CLI and server share the ${plan.sharedVersion} stamp)`),
    );
  } catch (cause) {
    if (loaded) stopLoopbackRegistry(loaded.container);
    throw cause;
  }
}

function cleanupArtifact() {
  const container = process.env[CANDIDATE_REGISTRY_ENV];
  if (stopLoopbackRegistry(container)) {
    notice(`migration-artifact: removed candidate registry ${container}`);
  } else {
    notice('migration-artifact: no live candidate registry to remove');
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    const mode = process.argv[2] || 'resolve';
    if (mode === 'resolve') resolveArtifact();
    else if (mode === 'cleanup') cleanupArtifact();
    else throw new Error(`unknown mode ${JSON.stringify(mode)}; expected resolve or cleanup`);
  } catch (cause) {
    error(`migration-artifact: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exitCode = 1;
  }
}
