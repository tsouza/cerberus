// build-with-registry-retry.mjs — run a command that fetches from a container
// registry (an image build, a `docker pull`, a compose pre-pull) and retry it
// when — and only when — it failed because the network path to the registry
// did, not because the command itself did and not because the registry refused
// on quota.
//
// The name is historical: the module was written for the build's own `FROM`
// resolution, and the retry policy it applies turned out to be the same one
// every registry fetch needs, so the host-side pulls route through it too
// rather than re-deriving it in bash.
//
// The failure this closes, on main @ d939e299 (runs 30695961421 `e2e` and
// 30695961427 `migration-e2e`, identical signature):
//
//   #5 ERROR: unexpected status from HEAD request to
//      https://registry-1.docker.io/v2/library/golang/manifests/1.26:
//      429 Too Many Requests
//
// `golang:1.26` is the `FROM` of Dockerfile.local — the base image BuildKit
// resolves while running the build. Every lane that builds a cerberus image
// (e2e / dashboard, migration-e2e, compose-smoke, the three compatibility
// harnesses) resolves it, so one Docker Hub rate-limit burst takes all of them
// down at once, including two release gates.
//
// WHY THE RETRY WRAPS THE BUILD, not a pre-pull. The obvious mirror of
// `pull-buildkit-image.mjs` — `docker pull golang:1.26` on the host first —
// only works when the builder can read the host daemon's image store, which
// depends on the driver the lane happens to use: the built-in `docker` driver
// (migration-e2e, the k3d lanes) does consult it, but the `docker-container`
// driver that `.github/actions/setup-buildx` installs (compatibility, release)
// runs BuildKit inside a container with its own content store and resolves
// `FROM` from the registry regardless. A host pre-pull would therefore protect
// some lanes and silently protect nothing in the others — the same "warms an
// image nothing uses" hazard the setup-buildx composite calls out. Wrapping the
// build works for every driver, and needs no knowledge of which base image refs
// the Dockerfile names, so the retry and the build cannot drift apart.
//
// There is deliberately NO local-image fallback here, unlike the bootstrap
// pull's "already present in the daemon is a pass". That postcondition is
// correct for a pull (a present image IS the artefact wanted) and wrong for a
// build: a tag left in the daemon by an earlier run attests nothing about this
// tree, so accepting it would be exactly the swallowed build failure this
// module exists not to be.
//
// Usage — the command to run is the trailing argv:
//   node .github/scripts/build-with-registry-retry.mjs docker build -f Dockerfile.local -t cerberus:e2e .
//   node .github/scripts/build-with-registry-retry.mjs docker compose up --wait --wait-timeout 300
//
// Env:
//   IMAGE_BUILD_RETRY_BACKOFF_SECONDS  (optional; default 10) linear backoff
//                                      step — attempt N sleeps N × this many
//                                      seconds.
//   GO_IMAGE (and any other key of `lib/mirror.mjs`'s `buildBaseImageArgs`)
//                                      (optional) the ref the build resolves
//                                      that base image from. Left unset, this
//                                      module resolves it mirror-first and
//                                      exports it to the command it runs; set,
//                                      it is passed through untouched.
//
// Exit: 0 on success; the command's own status when it failed for a reason
// another attempt cannot clear — a real build error, or a registry rate-limit
// refusal (see `rateLimitDiagnosis` in lib/registry.mjs: the quota window is
// hours, so retrying only spends more of the quota that is exhausted) — both
// unretried, on the first attempt; 1 when the retry budget is spent on
// transport faults.

import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';
import { buildBaseImageArgs } from './lib/mirror.mjs';
import {
  buildBaseImageRef,
  isRegistryRateLimit,
  isTransientRegistryFailure,
  rateLimitDiagnosis,
  readBackoffStepSeconds,
  registryAttempts,
  sleepSeconds,
} from './lib/registry.mjs';

// A Docker Hub 429 is a burst window measured in tens of seconds, not the
// couple of seconds a reset connection needs, so the build wrapper waits
// longer per attempt than the bootstrap pull does.
const defaultBackoffStepSeconds = 10;

// spawnSync reports a signalled child as status null.
const signalledChildStatus = 1;
// Conventional shell status for "command could not be executed at all".
const notExecutableStatus = 127;

function shellQuote(word) {
  return `'${String(word).replaceAll("'", "'\\''")}'`;
}

// runTeed — run argv with the parent's stdio, while also capturing everything
// it wrote. Both halves matter: a container build streams its only progress
// output for minutes, so swallowing it until the command returns would make a
// stuck build unreadable, and the retry decision needs the text. `tee` through
// bash with pipefail is what gives both plus the command's own exit status.
function runTeed(argv, logPath) {
  const script = `set -o pipefail; ${argv.map(shellQuote).join(' ')} 2>&1 | tee ${shellQuote(logPath)}`;
  const res = spawnSync('bash', ['-c', script], { stdio: 'inherit' });

  if (res.error) {
    return { status: notExecutableStatus, output: String(res.error.message) };
  }

  let output = '';
  try {
    output = readFileSync(logPath, 'utf8');
  } catch {
    // No log file means the pipeline never started; the status still stands.
  }
  return { status: res.status === null ? signalledChildStatus : res.status, output };
}

const command = process.argv.slice(2);
if (command.length === 0) {
  error(
    'build-with-registry-retry.mjs takes the image-building command as its trailing arguments, e.g. ' +
      '`node .github/scripts/build-with-registry-retry.mjs docker build -f Dockerfile.local -t cerberus:e2e .`',
  );
  process.exit(1);
}

// Point the build's base-image `FROM` refs at the mirror.
//
// This belongs here, and only here, for the same reason the retry does: this
// module is the single command every build in the tree runs through, pinned by
// `TestImageBuildingCommandsGoThroughTheRetryWrapper`. Setting the refs in the
// workflows instead would be one edit per building job — eight of them today —
// with nothing to notice when the ninth lane forgets, which is the per-leg
// shape the wrapper exists to replace.
//
// A ref the caller set explicitly always wins: an operator debugging against a
// particular toolchain image passes it, and the mirror does not overrule them.
//
// Both consumers read these from the environment rather than from an argv this
// module rewrites. Compose interpolates `${GO_IMAGE:-golang:1.26}` in the
// `build.args` of each service, and the Justfile's direct builds pass
// `--build-arg GO_IMAGE` with no value, which is docker's own "take it from the
// environment" form. Neither needs this module to know which build sites exist.
for (const [argName, upstreamRef] of Object.entries(buildBaseImageArgs)) {
  if ((process.env[argName] ?? '') !== '') continue;
  process.env[argName] = buildBaseImageRef(upstreamRef);
}

const backoffStepSeconds = readBackoffStepSeconds('IMAGE_BUILD_RETRY_BACKOFF_SECONDS', defaultBackoffStepSeconds);
const rendered = command.join(' ');

const scratchDir = mkdtempSync(join(tmpdir(), 'cerberus-build-retry-'));
const logPath = join(scratchDir, 'build.log');

function finish(code) {
  rmSync(scratchDir, { recursive: true, force: true });
  process.exit(code);
}

for (let attempt = 1; attempt <= registryAttempts; attempt++) {
  log(`==> ${rendered} (attempt ${attempt}/${registryAttempts})`);
  const { status, output } = runTeed(command, logPath);

  if (status === 0) finish(0);

  // Asked before the transient question, because a rate-limited fetch names a
  // transport fault too — and this class is the one where another attempt makes
  // the situation worse rather than merely wasting time.
  if (isRegistryRateLimit(output)) {
    error(rateLimitDiagnosis(`\`${rendered}\``));
    finish(status);
  }

  if (!isTransientRegistryFailure(output)) {
    error(
      `\`${rendered}\` failed with status ${status}, and its output names no registry or network fault. ` +
        'Failing on the first attempt: retrying a genuine failure only hides it.',
    );
    finish(status);
  }

  if (attempt < registryAttempts) {
    const waitSeconds = attempt * backoffStepSeconds;
    notice(
      `\`${rendered}\` failed on a transient registry/network fault (attempt ${attempt}/${registryAttempts}); ` +
        `retrying in ${waitSeconds}s.`,
    );
    sleepSeconds(waitSeconds);
  }
}

error(
  `\`${rendered}\` failed ${registryAttempts} times, every one of them on a transport fault between this ` +
    'runner and the registry (a reset connection, a TLS or DNS failure, a 5xx) — never a rate-limit refusal, ' +
    'which fails on the first attempt instead. The network path to the registry is unhealthy for this runner: ' +
    'failing the job rather than reporting a build that never happened.',
);
finish(1);
