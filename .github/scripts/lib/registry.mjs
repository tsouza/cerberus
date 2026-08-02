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
//
// The best answer to a quota is not to draw on it. `pullImageWithRetry` reaches
// for `lib/mirror.mjs`'s GHCR copy first and re-tags it to the upstream name,
// so the retry policy below is what protects the FALLBACK rather than the
// common case. See that module for why a mirror beats authenticating each pull
// path in turn.

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

  log(`    docker pull ${mirror} (mirror of ${image})`);
  const pull = capture('docker', ['pull', mirror]);
  if (pull.status !== 0) {
    notice(
      `${mirror} could not be pulled, falling back to ${image} on Docker Hub. The mirror is stale or the ` +
        `package is not public; \`mirror-images.mjs\` is what fixes that.\n${pull.stderr.trim()}`,
    );
    return false;
  }

  const tag = capture('docker', ['tag', mirror, image]);
  if (tag.status !== 0) {
    notice(
      `pulled ${mirror} but could not tag it as ${image}, falling back to Docker Hub.\n${tag.stderr.trim()}`,
    );
    return false;
  }

  log(`    ${image} acquired from the mirror`);
  return true;
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
// Returns true when the image is in the local daemon, false otherwise.
export function pullImageWithRetry(image, options = {}) {
  const {
    backoffStepSeconds = defaultPullBackoffStepSeconds,
    acceptLocalCopy = false,
    consequence = '',
  } = options;

  if (acquireFromMirror(image)) return true;

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
