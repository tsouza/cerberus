// registry.mjs — the one retry policy cerberus applies to every container
// registry fetch its CI performs.
//
// Two different fetches, one policy. The BuildKit *bootstrap* image is pulled
// by the host daemon before buildx boots a builder from it
// (`pull-buildkit-image.mjs`); the *base* images an image build names in `FROM`
// are resolved by BuildKit itself, inside the build
// (`build-with-registry-retry.mjs`). Docker Hub answers both out of the same
// rate-limit bucket and fails both the same way, so the attempt budget, the
// linear backoff, and the "is this failure worth another attempt" question
// belong in one module rather than being re-derived per call site.
//
// A registry says no in two fundamentally different ways, and the difference
// decides whether retrying is help or harm:
//
//   * a TRANSPORT fault — reset connection, TLS timeout, DNS, a 5xx — is a
//     failed attempt at something the registry is still willing to do. Another
//     attempt is free and usually works.
//   * a RATE LIMIT is the registry declining on purpose, against a counter that
//     only time decrements. Docker Hub counts pulls per account (or, for an
//     unauthenticated pull, per source IP) over a rolling window measured in
//     HOURS. A retry budget measured in seconds cannot outlast it, and each
//     attempt spends more of the very quota that is exhausted — so retrying is
//     strictly worse than failing, both for this job and for every other job
//     sharing the counter.
//
// So the rate-limit class is classified separately and is NOT retryable. That
// is not tolerance: the job still fails: it fails on the first attempt, with an
// error that names the quota instead of implying an outage.
//
// Exports:
//   registryAttempts                       attempts an acquisition gets.
//   readBackoffStepSeconds(env, fallback)  validated linear backoff step.
//   sleepSeconds(seconds)                  synchronous sleep.
//   isRegistryRateLimit(text)              true when output names a registry
//                                          rate-limit refusal. Never retry.
//   isTransientRegistryFailure(text)       true when output names a registry /
//                                          network fault that another attempt
//                                          can clear — false for a rate limit
//                                          and false for a genuine build error.
//   rateLimitDiagnosis(subject)            the one explanation every consumer
//                                          prints for the rate-limit class.
//   pullImageWithRetry(image, options)     acquire one image into the local
//                                          daemon under that policy, from the
//                                          GHCR mirror when there is one.
//   buildBaseImageRef(image)               the ref a BUILD should resolve image
//                                          from — the mirrored copy when this
//                                          runner can read it, else upstream.
//                                          A `FROM` cannot be served from the
//                                          daemon, so it needs the ref itself.
//   mirrorOnlyEnvVar / mirrorOnlyEnvValue  the job-scoped flag saying Docker Hub
//                                          is unreachable for this job.
//   mirrorOnly(env)                        is this job in that mode?
//
// The best answer to a quota is not to draw on it. `pullImageWithRetry` reaches
// for `lib/mirror.mjs`'s GHCR copy first and re-tags it to the upstream name,
// so the retry policy below is what protects the FALLBACK rather than the
// common case. See that module for why a mirror beats authenticating each pull
// path in turn.
//
// MIRROR-ONLY MODE. There is a third thing a registry can do, distinct from
// both refusing and answering: it can be UNREACHABLE. `registry-login.mjs`
// exhausts its retries on a transport fault, and until #1933 that killed the
// job — a Docker Hub outage taking down a lane whose every image sits in a GHCR
// mirror that was never contacted. So a login that ends that way records
// mirror-only mode for the rest of the job instead, and every acquisition below
// reads it.
//
// What the mode changes is one thing only: the Docker Hub fallback is REMOVED,
// not deprioritised. A mirror miss becomes a hard error naming the image. That
// is the half worth stating twice, because the obvious cheap version of this
// feature — letting the login be advisory — degrades a job to the shared
// ANONYMOUS Docker Hub quota, which is the silent failure the login gate
// (`assert-image-jobs-authenticate.mjs`, R2/R3) exists to make impossible. A
// job that cannot be served by the mirror must fail loudly and say which image
// it was.
//
// The mode is entered ONLY on a transport fault. A quota refusal
// (`rate-limit`) or a rejected credential still fails on the first attempt: the
// first is a counter that mirror-only mode cannot decrement and the second is a
// secret that is simply wrong, and neither is a reason to run a job in a
// degraded mode.

import process from 'node:process';

import { capture, error, log, notice } from './gh.mjs';
import { mirroredRef } from './mirror.mjs';

// Five attempts with a linear backoff: long enough to ride out a registry blip
// or a transport fault, short enough that a genuinely unreachable registry
// fails the job rather than parking it. It is deliberately NOT the budget a
// rate limit gets — that class gets one attempt. Mirrors the Justfile's
// `_pull-retry`.
export const registryAttempts = 5;

const msPerSecond = 1_000;

// Synchronous sleep: the callers are linear sequences of spawnSync calls, so
// there is no event loop to yield to.
export function sleepSeconds(seconds) {
  if (seconds <= 0) return;
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, seconds * msPerSecond);
}

// readBackoffStepSeconds — read a linear backoff step out of the environment,
// falling back when the variable is unset or blank. Attempt N sleeps N × the
// step. Exits non-zero on a value that isn't a non-negative number, because a
// silently-coerced backoff is a retry loop that no longer waits.
export function readBackoffStepSeconds(envName, fallbackSeconds) {
  const raw = process.env[envName];
  if (raw === undefined || raw.trim() === '') return fallbackSeconds;

  const seconds = Number(raw);
  if (!Number.isFinite(seconds) || seconds < 0) {
    error(`${envName} must be a non-negative number, got ${raw}`);
    process.exit(1);
  }
  return seconds;
}

// The signatures of a registry refusing on quota, in every form the toolchain
// surfaces one: the HTTP status, the registry error code, and the prose the
// daemon relays from Docker Hub's own body.
//
//   #5 ERROR: unexpected status from HEAD request to
//      https://registry-1.docker.io/v2/library/golang/manifests/1.26:
//      429 Too Many Requests
//
//   toomanyrequests: You have reached your unauthenticated pull rate limit.
const registryRateLimitPatterns = [
  /429 Too Many Requests/i,
  /\btoomanyrequests\b/i,
  /\bpull rate limit\b/i,
  /\brate limit exceeded\b/i,
];

export function isRegistryRateLimit(text) {
  const haystack = String(text ?? '');
  return registryRateLimitPatterns.some((re) => re.test(haystack));
}

// rateLimitDiagnosis — the single explanation for the class, so every consumer
// says the same true thing. The wording it replaced ("the registry is not
// answering for this runner") described an outage, and a whole afternoon of
// red lanes across every open PR was read as a Docker Hub incident before
// anyone questioned it. Naming the quota, its window, and the fact that the
// retry already happened and could not have helped is what makes the next
// reader look at pull volume instead of at a vendor status page.
export function rateLimitDiagnosis(subject) {
  return (
    `${subject} was refused by the registry's pull rate limit. This is a QUOTA, not an outage: the counter is ` +
    'per account — or per runner IP when the pull is unauthenticated — over a rolling window measured in hours, ' +
    'so retrying cannot clear it, and every extra attempt spends more of the quota that is already exhausted. ' +
    'Failing on the first attempt by design. The fixes, in order: pull the image from a registry this project ' +
    'controls, authenticate the pull, or cut the number of concurrent jobs pulling it and wait out the window.'
  );
}

// The environment variable carrying mirror-only mode from the login step that
// decided it to every acquisition later in the same job, and the one value that
// turns it on.
//
// $GITHUB_ENV is the channel because the decision has JOB scope: it is made
// once, by one step, and every subsequent step has to honour it — including the
// ones that reach an acquisition three hops down a Justfile recipe, which no
// step-output plumbing would reach without an edit per lane. The name is
// prefixed so it cannot collide with a variable a tool being tested reads.
export const mirrorOnlyEnvVar = 'CERBERUS_REGISTRY_MIRROR_ONLY';
export const mirrorOnlyEnvValue = '1';

// mirrorOnly — is this process running under a job that lost Docker Hub?
//
// Read at call time rather than at module load, and against an environment the
// caller may pass, so the mode is a property of the run rather than of the
// import order. Any value other than the one above is off: a half-set variable
// must not put a job into a degraded mode by accident.
export function mirrorOnly(env = process.env) {
  return String(env[mirrorOnlyEnvVar] ?? '').trim() === mirrorOnlyEnvValue;
}

// mirrorOnlyDiagnosis — the one explanation for "this job is mirror-only and
// the mirror could not serve `subject`".
//
// It names the image because that is the only actionable fact: the fix is to
// add that ref to `mirroredImages` in lib/mirror.mjs and let `mirror-images.yml`
// copy it. It says what did NOT happen because the alternative — reaching for
// Docker Hub anonymously — is the failure that would have been silent, and a
// reader who does not know it was deliberately declined will go looking for the
// pull that never happened.
export function mirrorOnlyDiagnosis(image, consequence = '') {
  const because = consequence === '' ? '' : ` — ${consequence}`;
  return (
    `${image} is not available from the GHCR mirror, and this job is running mirror-only because Docker Hub ` +
    'was unreachable when it logged in. The upstream pull was NOT attempted: an anonymous Docker Hub pull is ' +
    'the silent degradation the mirror and the login gate exist to remove, so this fails instead' +
    `${because}. The fix is to add the image to \`mirroredImages\` in .github/scripts/lib/mirror.mjs so ` +
    '`mirror-images.yml` copies it, and to check that this job logs in to ghcr.io before it acquires anything.'
  );
}

// The failure signatures that mean "the registry / network said no", as
// distinct from "the build said no" and from "the registry said not-so-fast".
// Every entry is a fault in fetching bytes over the wire, never a compiler,
// linker, test or Dockerfile error — that distinction is what lets a caller
// retry on this list and fail immediately off it, instead of retrying (and so
// hiding) a real build failure.
const transientRegistryFailurePatterns = [
  // BuildKit / containerd manifest + blob resolution against a registry that
  // answered with something other than the content.
  /unexpected status from (?:HEAD|GET|POST) request/i,
  /error pulling image configuration/i,
  /failed to copy: httpReadSeeker/i,
  /failed to do request/i,
  // Registry-side 5xx: the fetch is retryable by definition.
  /\b5\d\d (?:Bad Gateway|Service Unavailable|Internal Server Error|Gateway Time-?out)\b/i,
  // Transport faults on the way to a registry or a module proxy. `go mod
  // download` inside a build stage fails the same way against proxy.golang.org.
  /TLS handshake timeout/i,
  /connection reset by peer/i,
  /\bi\/o timeout\b/i,
  /context deadline exceeded/i,
  /net\/http: request canceled/i,
  /http2: (?:server sent GOAWAY|stream error)/i,
  /INTERNAL_ERROR; received from peer/i,
  // DNS.
  /temporary failure in name resolution/i,
  /no such host/i,
  /server misbehaving/i,
];

// A rate-limited fetch is reported by BuildKit as `unexpected status from HEAD
// request … 429`, which matches the transient list too — so the rate-limit
// question is asked FIRST and wins outright. Any output naming a quota refusal
// is non-retryable even when it also names a transport fault: the one thing a
// caller must not do while a quota is exhausted is spend more of it.
export function isTransientRegistryFailure(text) {
  const haystack = String(text ?? '');
  if (isRegistryRateLimit(haystack)) return false;
  return transientRegistryFailurePatterns.some((re) => re.test(haystack));
}

// An acquisition is a single manifest plus a handful of layers, so a short step
// is enough to ride out a reset connection; the image-build wrapper waits longer
// because a build's `FROM` resolution sits behind a much longer tail.
const defaultPullBackoffStepSeconds = 3;

function presentLocally(image) {
  return capture('docker', ['image', 'inspect', image]).status === 0;
}

// acquireFromMirror — try the GHCR copy first and re-tag it to the ref the
// caller asked for.
//
// The re-tag is the whole point. What the caller wants is the image PRESENT in
// the local daemon under its upstream name: compose files, Kubernetes
// manifests, and `k3d image import` all name Docker Hub, and compose only
// fetches an image it cannot find locally. Tagging the mirrored copy to the
// upstream name satisfies every one of them without a single ref rewritten in
// a file an operator reads.
//
// A miss returns false and says so once, at notice level. It is not a failure:
// the caller falls back to the upstream ref under the ordinary policy, which is
// exactly the behaviour that existed before the mirror. Completeness is gated
// where it can be fixed — in `mirror-images.mjs` and the inventory test — not
// here, where the only available response would be to fail a lane over an
// image that is perfectly reachable upstream.
function acquireFromMirror(image) {
  const mirror = mirroredRef(image);
  if (mirror === null) return false;

  // What a miss means depends on the mode, so the sentence is built from it
  // rather than asserting a fallback that mirror-only mode has removed. A
  // notice promising a Docker Hub pull that never happens is worse than no
  // notice: it sends the next reader looking for a pull in the log.
  const next = mirrorOnly() ? 'and this job is mirror-only, so the acquisition fails' : 'falling back to Docker Hub';

  log(`    docker pull ${mirror} (mirror of ${image})`);
  const pull = capture('docker', ['pull', mirror]);
  if (pull.status !== 0) {
    notice(
      `${mirror} could not be pulled, ${next}. The mirror is stale or the ` +
        `package is not public; \`mirror-images.mjs\` is what fixes that.\n${pull.stderr.trim()}`,
    );
    return false;
  }

  const tag = capture('docker', ['tag', mirror, image]);
  if (tag.status !== 0) {
    notice(`pulled ${mirror} but could not tag it as ${image}, ${next}.\n${tag.stderr.trim()}`);
    return false;
  }

  log(`    ${image} acquired from the mirror`);
  return true;
}

// buildBaseImageRef — the ref a BUILD should resolve `image` from: the mirrored
// copy when this runner can actually read it, the upstream ref when it cannot.
//
// This is the same mirror-first-with-fallback decision `acquireFromMirror`
// makes, moved one layer out. It has to be a separate decision because the two
// acquisition shapes land in different places: `acquireFromMirror` pulls into
// the HOST daemon and re-tags, which a `FROM` under the `docker-container`
// driver never consults. Only the ref BuildKit is given can steer that, so the
// question here is not "can I pull this" but "will BuildKit resolve this", and
// `imagetools inspect` asks exactly that — it reads the manifest through the
// same registry path a `FROM` takes, without pulling any layers.
//
// A miss is a notice, not a failure, for the reason the mirror pull gives: the
// upstream ref is perfectly reachable, so refusing to build over a stale or
// unreadable mirror would fail a lane for no gain. It is also what keeps forks
// working — a fork's runner authenticates to ghcr.io as itself and cannot read
// this repo's private packages, so it probes, misses, and builds from Docker
// Hub exactly as it did before the mirror existed.
//
// Under mirror-only mode that reasoning inverts, because its premise is gone:
// the upstream ref is NOT reachable. Returning it would hand BuildKit a `FROM`
// against a registry this job has already established it cannot talk to, and
// the failure would arrive minutes later as a build timeout naming neither the
// mirror nor the outage. So the miss returns null — "there is no ref that can
// serve this build" — and the caller reports it before spending the time.
export function buildBaseImageRef(image) {
  const mirror = mirroredRef(image);
  if (mirror === null) {
    if (mirrorOnly()) {
      error(mirrorOnlyDiagnosis(image, 'the build has no base image to resolve its `FROM` against'));
      return null;
    }
    return image;
  }

  const probe = capture('docker', ['buildx', 'imagetools', 'inspect', mirror]);
  if (probe.status !== 0) {
    if (mirrorOnly()) {
      error(
        `${mirror} is not resolvable from this runner and this job is mirror-only because Docker Hub was ` +
          `unreachable when it logged in, so there is no ref a build can resolve ${image} from. Check that ` +
          `this job logs in to ghcr.io before it builds.\n${probe.stderr.trim()}`,
      );
      return null;
    }
    notice(
      `${mirror} is not resolvable from this runner, so the build resolves ${image} from Docker Hub. ` +
        'That spends the anonymous quota this mirror exists to stop spending; `mirror-images.mjs` is ' +
        `what fixes it.\n${probe.stderr.trim()}`,
    );
    return image;
  }

  log(`    builds will resolve ${image} as ${mirror}`);
  return mirror;
}

// pullImageWithRetry — acquire one image into the local daemon under the policy
// above, and report every outcome in the shared vocabulary. This is the one
// implementation: a caller that hand-rolls the loop is a call site that will
// eventually diverge on the question the module exists to answer.
//
// Options:
//   backoffStepSeconds  linear step; attempt N sleeps N × this.
//   acceptLocalCopy     a failed pull still succeeds when the image is already
//                       in the daemon. For callers whose postcondition is
//                       PRESENCE rather than freshness (the buildx bootstrap:
//                       buildx itself falls back to the local copy). Off by
//                       default, because a caller that needs the released bytes
//                       must not silently accept whatever the runner cached.
//   consequence         clause naming what breaks when the budget is spent,
//                       appended to the exhaustion error.
//
// Under mirror-only mode the upstream loop below is not entered at all: the
// mirror either serves the image or the acquisition fails naming it. See the
// module header for why a fallback to an ANONYMOUS Docker Hub pull is the one
// outcome that must not be available here.
//
// Returns true when the image is in the local daemon, false otherwise.
export function pullImageWithRetry(image, options = {}) {
  const {
    backoffStepSeconds = defaultPullBackoffStepSeconds,
    acceptLocalCopy = false,
    consequence = '',
  } = options;

  if (acquireFromMirror(image)) return true;

  if (mirrorOnly()) {
    // Asked before the mode, exactly as it is inside the loop: a copy already
    // in the daemon satisfies this caller's postcondition no matter which
    // registry is reachable, and no pull is needed to honour it.
    if (acceptLocalCopy && presentLocally(image)) {
      notice(
        `${image} could not be acquired from the mirror, but it is already in the local daemon — ` +
          'the caller accepts the local copy.',
      );
      return true;
    }
    error(mirrorOnlyDiagnosis(image, consequence));
    return false;
  }

  for (let attempt = 1; attempt <= registryAttempts; attempt++) {
    log(`    docker pull ${image} (attempt ${attempt}/${registryAttempts})`);
    const res = capture('docker', ['pull', image]);
    if (res.status === 0) {
      log(res.stdout.trim());
      return true;
    }
    process.stderr.write(res.stderr);

    // A quota refusal is checked only after the local-copy fallback, because a
    // copy already in the daemon satisfies that caller's postcondition no
    // matter why the pull failed.
    if (acceptLocalCopy && presentLocally(image)) {
      notice(
        `docker pull ${image} failed, but the image is already in the local daemon — ` +
          'the caller accepts the local copy.',
      );
      return true;
    }

    // With nothing local to fall back on there is nothing to wait for either:
    // the window outlasts the budget, so the remaining attempts would only
    // deepen the deficit failing this job and every concurrent one.
    if (isRegistryRateLimit(res.stderr + res.stdout)) {
      error(rateLimitDiagnosis(`docker pull ${image}`));
      return false;
    }

    if (attempt < registryAttempts) {
      sleepSeconds(attempt * backoffStepSeconds);
    }
  }

  const absent = acceptLocalCopy ? ' and the image is absent from the local daemon' : '';
  const because = consequence === '' ? '.' : `, so ${consequence}`;
  error(`docker pull ${image} failed ${registryAttempts} times on transport faults${absent}${because}`);
  return false;
}
