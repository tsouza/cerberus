// pull-buildkit-image.mjs — put the BuildKit bootstrap image into the local
// docker daemon before `docker/setup-buildx-action` boots a builder from it.
//
// The `docker-container` driver boots by pulling `moby/buildkit:<tag>` and
// starting a container from it. That pull is single-attempt: one reset
// connection to Docker Hub fails `buildx inspect --bootstrap`, which fails the
// setup step, which fails the whole job before a single image has been built:
//
//   #1 [internal] booting buildkit
//   #1 pulling image moby/buildkit:buildx-stable-1
//   #1 ERROR: Error response from daemon: Head ".../moby/buildkit/manifests/
//      buildx-stable-1": read: connection reset by peer
//
// buildx does tolerate a failed pull when the image is already in the local
// image store ("pulling failed, using local image"), so acquiring it here —
// with retry — is what makes the bootstrap survive a flaky registry. The
// postcondition this module asserts is presence in the daemon, not a
// successful pull: an exhausted retry over an image the runner already holds
// is a pass, because that is precisely the state buildx falls back to.
//
// The attempt budget, the backoff and the failure vocabulary are shared with
// the image-build wrapper via ./lib/registry.mjs — the bootstrap pull and a
// build's `FROM` resolution hit the same registry and fail the same way, so
// they retry to the same policy, and stop retrying on the same class: a
// rate-limit refusal ends the loop at once rather than spending four more
// pulls out of the quota that is refusing this one.
//
// Env:
//   BUILDKIT_IMAGE                 (required) image ref to acquire, e.g.
//                                  `moby/buildkit:buildx-stable-1`. Must be
//                                  the same ref the builder is told to boot
//                                  from (`driver-opts: image=…`), or this
//                                  pre-pull warms an image nothing uses.
//   BUILDKIT_PULL_BACKOFF_SECONDS  (optional; default 3) linear backoff step —
//                                  attempt N sleeps N × this many seconds.
//
// Exit: 0 when the image is in the local daemon, 1 when it is not.

import process from 'node:process';

import { capture, error, log, notice } from './lib/gh.mjs';
import {
  isRegistryRateLimit,
  rateLimitDiagnosis,
  readBackoffStepSeconds,
  registryAttempts,
  sleepSeconds,
} from './lib/registry.mjs';

// A bootstrap pull is a single manifest + a handful of layers, so a short step
// is enough to ride out a reset connection; the image-build wrapper waits
// longer because a Hub 429 burst outlives it.
const defaultBackoffStepSeconds = 3;

function presentLocally(image) {
  return capture('docker', ['image', 'inspect', image]).status === 0;
}

const image = (process.env.BUILDKIT_IMAGE ?? '').trim();
if (image === '') {
  error('BUILDKIT_IMAGE is empty: nothing to acquire. Pass the same image ref the builder boots from.');
  process.exit(1);
}

const backoffStepSeconds = readBackoffStepSeconds('BUILDKIT_PULL_BACKOFF_SECONDS', defaultBackoffStepSeconds);

for (let attempt = 1; attempt <= registryAttempts; attempt++) {
  log(`    docker pull ${image} (attempt ${attempt}/${registryAttempts})`);
  const res = capture('docker', ['pull', image]);
  if (res.status === 0) {
    log(res.stdout.trim());
    process.exit(0);
  }
  process.stderr.write(res.stderr);

  if (presentLocally(image)) {
    notice(
      `docker pull ${image} failed, but the image is already in the local daemon — ` +
        'buildx boots the builder from the local copy.',
    );
    process.exit(0);
  }

  // A quota refusal is checked only after the local-image fallback, because a
  // copy already in the daemon satisfies the postcondition no matter why the
  // pull failed. With no local copy there is nothing to wait for: the window
  // outlasts the budget, so the remaining attempts would only deepen the
  // deficit that is failing this job and every concurrent one.
  if (isRegistryRateLimit(res.stderr + res.stdout)) {
    error(rateLimitDiagnosis(`docker pull ${image}`));
    process.exit(1);
  }

  if (attempt < registryAttempts) {
    sleepSeconds(attempt * backoffStepSeconds);
  }
}

error(
  `docker pull ${image} failed ${registryAttempts} times on transport faults and the image is absent from the ` +
    'local daemon, so `buildx inspect --bootstrap` has nothing to boot from.',
);
process.exit(1);
