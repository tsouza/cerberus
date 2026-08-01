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
// Exports:
//   registryAttempts                       attempts an acquisition gets.
//   readBackoffStepSeconds(env, fallback)  validated linear backoff step.
//   sleepSeconds(seconds)                  synchronous sleep.
//   isTransientRegistryFailure(text)       true when command output names a
//                                          registry / network fault rather
//                                          than a genuine build error.

import process from 'node:process';

import { error } from './gh.mjs';

// Five attempts with a linear backoff: long enough to ride out a registry blip
// or a Docker Hub 429 burst, short enough that a genuinely unreachable registry
// fails the job rather than parking it. Mirrors the Justfile's `_pull-retry`.
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

// The failure signatures that mean "the registry / network said no", as
// distinct from "the build said no". Every entry is a fault in fetching bytes
// over the wire, never a compiler, linker, test or Dockerfile error — that
// distinction is what lets a caller retry on this list and fail immediately off
// it, instead of retrying (and so hiding) a real build failure.
//
// The 429 that took `e2e` + `migration-e2e` down on main lands in the first two
// entries:
//
//   #5 ERROR: unexpected status from HEAD request to
//      https://registry-1.docker.io/v2/library/golang/manifests/1.26:
//      429 Too Many Requests
const transientRegistryFailurePatterns = [
  // Docker Hub rate limiting, in both the HTTP-status and the error-code form.
  /429 Too Many Requests/i,
  /\btoomanyrequests\b/i,
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

export function isTransientRegistryFailure(text) {
  const haystack = String(text ?? '');
  return transientRegistryFailurePatterns.some((re) => re.test(haystack));
}
