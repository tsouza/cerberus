// migration-artifact.mjs — resolve WHICH cerberus the Layer-14 migration lane
// tests, on every path, and refuse an incoherent input pair.
//
// migration-e2e.yml runs twice against the same commit for a release: once on
// the push (proving the SOURCE TREE) and once via `workflow_call` from
// release.yml's `release-artifact-migration` job (proving the ARTIFACT
// goreleaser just built). Both runs execute the same three tier jobs, so the
// ONE thing that differs is which cerberus binary the scenarios exec and which
// image the compose stacks run. This module is that single decision point.
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
//   image   a released image reference was supplied — the CLI is extracted OUT
//           of that image (`docker create` + `docker cp`), so the binary the
//           scenarios exec and the server the stack runs are literally the same
//           bytes, and `sharedVersion` is the release version both must report.
//
// Supplying exactly one half of the (image, expect-version) pair is a hard
// error, never a silent fall-back to `source`: that is precisely how a release
// lane would end up testing a from-source build under an artifact-shaped job
// name and reporting green.
//
// Env:
//   CERBERUS_IMAGE_INPUT           released image ref to test; empty = build
//                                  from this tree.
//   CERBERUS_EXPECT_VERSION_INPUT  version that image's binary must report;
//                                  travels with CERBERUS_IMAGE_INPUT.
//   BUILD_DIR                      where the resolved binary lands
//                                  (default `build`).
//   COMPOSE_PROJECT_SUFFIX         per-checkout suffix the local image tag
//                                  carries, so the tag named here is the one
//                                  the compose stack runs. Empty in a primary
//                                  checkout, which is every CI checkout.
//   GITHUB_ENV                     runner file the resolved plan is exported
//                                  through (CERBERUS_BIN / CERBERUS_IMAGE /
//                                  CERBERUS_EXPECT_VERSION).
//
// Exit: 0 with the plan exported; 1 on a half-supplied input pair, a failed
// build / pull / extraction, or a `--version` probe that does not report
// exactly the expected stamp.

import { appendFileSync, mkdirSync, chmodSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';

import { capture, error, notice, log } from './lib/gh.mjs';

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

// LOCAL_IMAGE_TAG is the tag the compose stacks run when no released image is
// supplied. Held equal to the `${CERBERUS_IMAGE:-…}` default in
// tiers/tier1-dual/docker-compose.dual.yml and to the Justfile's
// MIGRATION_LOCAL_IMAGE by the same regression pin.
export const LOCAL_IMAGE_TAG = expandComposeSuffix('cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}');

// IMAGE_BINARY_PATH is where the cerberus image keeps its binary — the same
// path Dockerfile.local's and Dockerfile's `COPY --from=build` write to, and
// the same one the compose healthcheck execs.
export const IMAGE_BINARY_PATH = '/usr/local/bin/cerberus';

// BINARY_NAME is the filename the resolved binary lands under inside BUILD_DIR.
const BINARY_NAME = 'cerberus';

// EXECUTABLE_MODE is what an extracted binary is chmod'ed to. `docker cp`
// preserves the image's mode, but an image built elsewhere is not this lane's
// to trust.
const EXECUTABLE_MODE = 0o755;

// PAIRED_INPUTS_MESSAGE is the refusal for a half-supplied pair. Its exact text
// is asserted by migration-artifact.test.mjs, so a loosening rewrite is caught.
export const PAIRED_INPUTS_MESSAGE =
  'cerberus_image and expect_version travel together: supply both (test a released artifact) ' +
  'or neither (build from this tree). Supplying one alone would run a source build under an ' +
  'artifact-shaped job name.';

// resolvePlan is the testable core: it decides which cerberus the lane drives
// from the two inputs alone, with no I/O.
//
// Returns { source, image, expectVersion, sharedVersion } where
//   expectVersion  the stamp the CLI this module produces must report;
//   sharedVersion  the stamp EVERY cerberus in the lane reports (CLI and
//                  server), or '' when the two come from different builds.
export function resolvePlan({ image, expectVersion } = {}) {
  const ref = String(image ?? '').trim();
  const want = String(expectVersion ?? '').trim();

  if (ref === '' && want === '') {
    return {
      source: 'source',
      image: LOCAL_IMAGE_TAG,
      expectVersion: SOURCE_BUILD_VERSION,
      sharedVersion: '',
    };
  }
  if (ref === '' || want === '') {
    throw new Error(PAIRED_INPUTS_MESSAGE);
  }
  return { source: 'image', image: ref, expectVersion: want, sharedVersion: want };
}

// buildFromSource compiles the CLI the scenarios exec out of this tree.
function buildFromSource(binary) {
  const res = capture('go', ['build', '-o', binary, './cmd/cerberus']);
  if (res.status !== 0) {
    error(`migration-artifact: go build ./cmd/cerberus failed:\n${res.stdout}${res.stderr}`);
    process.exit(1);
  }
}

// extractFromImage pulls the released image and copies its binary out, so the
// CLI the scenarios exec is the same bytes as the server the stack runs.
function extractFromImage(ref, binary) {
  const pull = capture('docker', ['pull', ref]);
  if (pull.status !== 0) {
    error(
      `migration-artifact: docker pull ${ref} failed — the released image is unpullable from this ` +
        `job (check the GHCR login step and the package's visibility):\n${pull.stdout}${pull.stderr}`,
    );
    process.exit(1);
  }

  const created = capture('docker', ['create', ref]);
  if (created.status !== 0) {
    error(`migration-artifact: docker create ${ref} failed:\n${created.stdout}${created.stderr}`);
    process.exit(1);
  }
  const container = created.stdout.trim().split('\n').pop().trim();
  if (container === '') {
    error(`migration-artifact: docker create ${ref} printed no container id`);
    process.exit(1);
  }

  try {
    const cp = capture('docker', ['cp', `${container}:${IMAGE_BINARY_PATH}`, binary]);
    if (cp.status !== 0) {
      error(
        `migration-artifact: docker cp ${container}:${IMAGE_BINARY_PATH} failed — the image does ` +
          `not carry the binary at that path:\n${cp.stdout}${cp.stderr}`,
      );
      process.exit(1);
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
    error(
      `migration-artifact: ${binary} --version exited ${res.status} (plan=${plan.source}, ` +
        `image=${plan.image}):\n${res.stdout}${res.stderr}`,
    );
    process.exit(1);
  }
  const lines = res.stdout.split('\n').map((l) => l.trim()).filter((l) => l !== '');
  if (lines.length !== 1) {
    error(
      `migration-artifact: ${binary} --version printed ${lines.length} non-empty line(s), want ` +
        `exactly one bare version: ${JSON.stringify(res.stdout)}`,
    );
    process.exit(1);
  }
  if (lines[0] !== plan.expectVersion) {
    error(
      `migration-artifact: ${binary} reports version "${lines[0]}" but the ${plan.source} plan ` +
        `expects "${plan.expectVersion}" (image=${plan.image}). The lane would have tested a ` +
        `different build than the one it claims to.`,
    );
    process.exit(1);
  }
  return lines[0];
}

function exportEnv(entries) {
  const file = process.env.GITHUB_ENV;
  const block = entries.map(([k, v]) => `${k}=${v}`).join('\n');
  if (!file) {
    log(`[no $GITHUB_ENV in env; resolved plan follows]\n${block}`);
    return;
  }
  appendFileSync(file, `${block}\n`);
}

function main() {
  let plan;
  try {
    plan = resolvePlan({
      image: process.env.CERBERUS_IMAGE_INPUT,
      expectVersion: process.env.CERBERUS_EXPECT_VERSION_INPUT,
    });
  } catch (err) {
    error(`migration-artifact: ${err.message}`);
    process.exit(1);
  }

  const buildDir = path.resolve(process.env.BUILD_DIR || 'build');
  mkdirSync(buildDir, { recursive: true });
  const binary = path.join(buildDir, BINARY_NAME);

  if (plan.source === 'source') {
    buildFromSource(binary);
  } else {
    extractFromImage(plan.image, binary);
  }

  const version = probeVersion(binary, plan);

  exportEnv([
    ['CERBERUS_BIN', binary],
    ['CERBERUS_IMAGE', plan.image],
    ['CERBERUS_EXPECT_VERSION', plan.sharedVersion],
  ]);

  notice(
    `migration-artifact: ${plan.source} plan — binary ${binary} reports ${version}, ` +
      `compose runs ${plan.image}` +
      (plan.sharedVersion === ''
        ? ' (CLI and server are separate builds; each is held to its own stamp)'
        : ` (CLI and server share the ${plan.sharedVersion} stamp)`),
  );
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
