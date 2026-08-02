// registry-login — `docker login` with the same retry discipline every other
// registry interaction in this repo already has.
//
// WHY THIS EXISTS. `mirror-images.yml` failed on its first real run (the
// push-to-main for #1579, run 30724446834) at the *Log in to Docker Hub* step:
//
//   Error response from daemon: Get "https://registry-1.docker.io/v2/":
//   context deadline exceeded
//
// The job died before pulling anything, so #1579 merged green while GHCR was
// never populated and the mirror shipped INERT — every lane silently fell back
// to Docker Hub, the exact condition the mirror exists to remove. That is worth
// a script rather than a re-run for two reasons: this workflow is the fallback
// path for everything else, so the most load-bearing registry interaction in
// the repo was the least protected one; and its failure is silent at the point
// of use, because a consumer reads a mirror miss as a fallback and pulls
// upstream, making "the mirror is empty" and "the mirror is working"
// indistinguishable from any consuming job.
//
// `docker/login-action` has no retry input, which is why the step is a script
// instead. The classification is NOT re-implemented here: it imports
// isTransientRegistryFailure / isRegistryRateLimit from lib/registry.mjs, so a
// login inherits the same verdicts a pull gets and there is one place to fix
// the class. That import is what keeps #1563 honest — a handshake that timed
// out is genuinely transient and retries, while a quota refusal is a spent
// rolling window that one more attempt can only spend further, so it fails on
// the first attempt by design.
//
// A credential rejection is the third class and also fails immediately: a
// password that is wrong on attempt 1 is wrong on attempt 5, and retrying it
// only turns a clear error into a slow one.
//
// ENV CONTRACT
//   REGISTRY  — registry host to log in to. Blank/unset means Docker Hub, which
//               is what `docker login` itself does with no host argument.
//   USERNAME  — required.
//   PASSWORD  — required; passed on stdin, never as argv, so it cannot leak
//               into a process listing.
//   REGISTRY_LOGIN_BACKOFF_STEP_SECONDS — optional; attempt N waits N × step.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, log, notice } from './lib/gh.mjs';
import {
  isRegistryRateLimit,
  isTransientRegistryFailure,
  rateLimitDiagnosis,
  readBackoffStepSeconds,
  registryAttempts,
  sleepSeconds,
} from './lib/registry.mjs';

// A login is one round trip to /v2/, so it needs less patience than an image
// acquisition (whose tail includes layer transfer). Kept short because the
// failure this rides out is a handshake blip, not a slow download.
const defaultLoginBackoffStepSeconds = 2;

// The signatures of a registry rejecting the credential itself, as distinct
// from failing to reach the registry at all. Retrying these is pointless.
const credentialRejectionPatterns = [
  /\b401 Unauthorized\b/i,
  /\bunauthorized\b/i,
  /incorrect username or password/i,
  /authentication required/i,
  // GHCR answers a bad token with `denied: denied`; Docker Hub with `denied:
  // requested access to the resource is denied`. Matching the bare word covers
  // both, and nothing on the transport-fault list uses it.
  /\bdenied\b/i,
];

function isCredentialRejection(text) {
  const haystack = String(text ?? '');
  return credentialRejectionPatterns.some((re) => re.test(haystack));
}

// classifyLoginFailure — the whole verdict in one pure function, so the
// precedence between the classes is testable without spawning docker.
//
// The ORDER is the substance. A quota refusal is asked first and wins outright
// because Docker Hub's 429 body can also name a transport fault, and the one
// thing a caller must not do while a quota is exhausted is spend more of it —
// the same reason lib/registry.mjs asks it first. Credentials come next: a
// registry that answered "401" did reach us, so the transport is fine and the
// secret is not. Only what is left is retried.
export function classifyLoginFailure(text) {
  if (isRegistryRateLimit(text)) return 'rate-limit';
  if (isCredentialRejection(text)) return 'credential';
  if (isTransientRegistryFailure(text)) return 'transient';
  return 'fatal';
}

function requiredEnv(name) {
  const value = process.env[name];
  if (value === undefined || value === '') {
    error(`${name} is required`);
    process.exit(1);
  }
  return value;
}

function main() {
  const registry = (process.env.REGISTRY ?? '').trim();
  const username = requiredEnv('USERNAME');
  const password = requiredEnv('PASSWORD');
  // Docker Hub is the no-host form of the command, matching `docker login`'s
  // own default rather than inventing a hostname for it.
  const subject = registry === '' ? 'Docker Hub' : registry;
  const args = ['login', '--username', username, '--password-stdin'];
  if (registry !== '') args.push(registry);

  const backoffStep = readBackoffStepSeconds(
    'REGISTRY_LOGIN_BACKOFF_STEP_SECONDS',
    defaultLoginBackoffStepSeconds,
  );

  for (let attempt = 1; attempt <= registryAttempts; attempt += 1) {
    const res = capture('docker', args, { input: password });
    if (res.status === 0) {
      if (attempt > 1) notice(`Logged in to ${subject} on attempt ${attempt} of ${registryAttempts}.`);
      return;
    }

    const output = `${res.stdout}\n${res.stderr}`;
    log(output.trim());

    const verdict = classifyLoginFailure(output);
    if (verdict === 'rate-limit') {
      error(rateLimitDiagnosis(`Logging in to ${subject}`));
      process.exit(1);
    }
    if (verdict === 'credential') {
      error(
        `${subject} rejected the credential. This is not retried: a password that is wrong on the first ` +
          'attempt is wrong on the last. Check that the secret is set on this repository and has not expired.',
      );
      process.exit(1);
    }
    if (verdict === 'fatal') {
      error(
        `Logging in to ${subject} failed with an error that is not a transport fault, a quota refusal or a ` +
          'credential rejection, so it is not retried. See the command output above.',
      );
      process.exit(1);
    }
    if (attempt === registryAttempts) {
      error(
        `Logging in to ${subject} failed ${registryAttempts} times with a transport fault. The registry is ` +
          'not reachable from this runner; this is not a quota. See the command output above.',
      );
      process.exit(1);
    }
    const waitSeconds = attempt * backoffStep;
    log(
      `Login to ${subject} hit a transport fault (attempt ${attempt} of ${registryAttempts}); ` +
        `retrying in ${waitSeconds}s.`,
    );
    sleepSeconds(waitSeconds);
  }
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
