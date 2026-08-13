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
// WHAT A SPENT TRANSIENT BUDGET MEANS (issue #1933). Exhausting the retries on
// a `transient` verdict is an honest report that this runner cannot reach the
// registry — and for DOCKER HUB that is precisely the condition the GHCR mirror
// was built to survive. Killing the job there fails a lane whose every image is
// in a registry nobody even contacted. So a Docker Hub login that ends this way
// downgrades the job to MIRROR-ONLY mode (lib/registry.mjs) rather than failing:
// it records the mode in $GITHUB_ENV and exits 0, and every later acquisition
// serves itself from the mirror or fails LOUDLY naming the image it wanted.
//
// Three boundaries make that a narrowing rather than a tolerance:
//
//   * only the `transient` verdict downgrades. A quota refusal and a rejected
//     credential still fail on the first attempt, as before.
//   * only DOCKER HUB downgrades. A ghcr.io login that cannot reach ghcr.io has
//     nothing to fall back TO — mirror-only mode there would mean "acquire
//     everything from the registry we just failed to reach", so it fails.
//   * the downgrade is never silence. `continue-on-error: true` on this step
//     would have been the cheap version and is the regression it looks like a
//     fix for: an unauthenticated job does not fail, it degrades to the shared
//     anonymous Docker Hub quota and reads as flake on the day that quota runs
//     out. Mirror-only mode REMOVES the Docker Hub path instead of demoting it.
//
// ENV CONTRACT
//   REGISTRY  — registry host to log in to. Blank/unset means Docker Hub, which
//               is what `docker login` itself does with no host argument.
//   USERNAME  — required.
//   PASSWORD  — required; passed on stdin, never as argv, so it cannot leak
//               into a process listing.
//   REGISTRY_LOGIN_BACKOFF_STEP_SECONDS — optional; attempt N waits N × step.
//   ON_TRANSPORT_FAULT — optional; `mirror-only` (default) or `fail`. What an
//               exhausted TRANSPORT-fault budget does on a Docker Hub login.
//               `fail` is for the two jobs Docker Hub is not a fallback for:
//               `mirror-images.yml` READS the inventory from it, and
//               `release.yml` PUSHES to it. Neither can be served by a mirror,
//               so for them an unreachable Docker Hub is a genuine dead end.
//   GITHUB_ENV — runner file the mirror-only flag is exported through.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, exportEnv, log, notice } from './lib/gh.mjs';
import {
  isRegistryRateLimit,
  isTransientRegistryFailure,
  mirrorOnlyEnvValue,
  mirrorOnlyEnvVar,
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

// The two answers to "what does an exhausted transport-fault budget do", and the
// variable that carries the choice. Spelled as named constants because the whole
// difference between a lane that survives a Docker Hub outage and one that dies
// on it is which of these two strings a step did not have to say.
export const onTransportFaultVar = 'ON_TRANSPORT_FAULT';
export const onTransportFaultMirrorOnly = 'mirror-only';
export const onTransportFaultFail = 'fail';

// Docker Hub under every name `docker login` accepts for it. The empty string is
// the ordinary one — `docker login` with no host argument targets Docker Hub —
// and the rest are here so a step that spells the host out does not silently
// lose the downgrade it never knew it had.
const dockerHubRegistries = new Set(['', 'docker.io', 'index.docker.io', 'registry-1.docker.io']);

export function isDockerHubLogin(registry) {
  return dockerHubRegistries.has(String(registry ?? '').trim().toLowerCase());
}

// downgradeToMirrorOnly — the whole downgrade decision as one pure function, for
// the same reason `classifyLoginFailure` is one: the value of the rule is in the
// conjunction, and a conjunction spread across an if-chain in `main` is testable
// only by spawning docker against a registry that is really down.
//
// All three conditions are load-bearing and none is a default:
//   verdict === 'transient'  — the registry is UNREACHABLE, as opposed to
//                              refusing us (quota) or rejecting us (credential).
//                              Only unreachability is what a mirror substitutes
//                              for.
//   Docker Hub               — the mirror stands in for Docker Hub and nothing
//                              else. A mirror-only mode entered because the
//                              MIRROR is unreachable is a contradiction.
//   not opted out            — `fail` is how the two jobs that genuinely need
//                              Docker Hub (mirroring it, publishing to it) say
//                              a mirror cannot serve them.
export function downgradeToMirrorOnly({ registry, verdict, onTransportFault }) {
  if (verdict !== 'transient') return false;
  if (!isDockerHubLogin(registry)) return false;
  return onTransportFault !== onTransportFaultFail;
}

function requiredEnv(name) {
  const value = process.env[name];
  if (value === undefined || value === '') {
    error(`${name} is required`);
    process.exit(1);
  }
  return value;
}

// readOnTransportFault — the validated policy for this step.
//
// An unrecognised value exits rather than falling back to the default. The
// default is the PERMISSIVE branch, so a typo (`ON_TRANSPORT_FAULT: fial`)
// would silently re-enable the downgrade on exactly the two jobs that spelled
// it out to switch it off — and the symptom would be a release that carried on
// after losing its push registry.
function readOnTransportFault(registry) {
  const raw = (process.env[onTransportFaultVar] ?? '').trim();
  if (raw === '') return onTransportFaultMirrorOnly;
  if (raw !== onTransportFaultMirrorOnly && raw !== onTransportFaultFail) {
    error(
      `${onTransportFaultVar} must be \`${onTransportFaultMirrorOnly}\` or \`${onTransportFaultFail}\`, ` +
        `got ${JSON.stringify(raw)}.`,
    );
    process.exit(1);
  }
  if (raw === onTransportFaultMirrorOnly && !isDockerHubLogin(registry)) {
    error(
      `${onTransportFaultVar}=${onTransportFaultMirrorOnly} is set on a login to ${registry}, which is not ` +
        'Docker Hub. Mirror-only mode means "acquire everything from the GHCR mirror", so it can only stand ' +
        'in for Docker Hub — asking for it here would mean falling back to the registry this step just ' +
        'failed to reach.',
    );
    process.exit(1);
  }
  return raw;
}

function main() {
  const registry = (process.env.REGISTRY ?? '').trim();
  const onTransportFault = readOnTransportFault(registry);
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
      if (downgradeToMirrorOnly({ registry, verdict, onTransportFault })) {
        exportEnv([[mirrorOnlyEnvVar, mirrorOnlyEnvValue]]);
        notice(
          `Logging in to ${subject} failed ${registryAttempts} times with a transport fault, so this job ` +
            'continues in MIRROR-ONLY mode: every image is acquired from the GHCR mirror, and an image the ' +
            'mirror cannot serve fails the job naming itself rather than being pulled anonymously from the ' +
            'registry that is down. This is not a quota, and it is not a green light for an unauthenticated ' +
            'pull. See the command output above.',
        );
        return;
      }
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
