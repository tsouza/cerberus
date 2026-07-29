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

// Five attempts with a linear backoff, matching the Justfile's `_pull-retry`:
// long enough to ride out a registry blip, short enough that a genuinely
// unreachable registry fails the job in under a minute.
const pullAttempts = 5;
const defaultBackoffStepSeconds = 3;
const msPerSecond = 1_000;

// Synchronous sleep: this module is a linear sequence of spawnSync calls, so
// there is no event loop to yield to.
function sleepSeconds(seconds) {
  if (seconds <= 0) return;
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, seconds * msPerSecond);
}

function presentLocally(image) {
  return capture('docker', ['image', 'inspect', image]).status === 0;
}

const image = (process.env.BUILDKIT_IMAGE ?? '').trim();
if (image === '') {
  error('BUILDKIT_IMAGE is empty: nothing to acquire. Pass the same image ref the builder boots from.');
  process.exit(1);
}

const backoffStepSeconds = Number(process.env.BUILDKIT_PULL_BACKOFF_SECONDS ?? defaultBackoffStepSeconds);
if (!Number.isFinite(backoffStepSeconds) || backoffStepSeconds < 0) {
  error(`BUILDKIT_PULL_BACKOFF_SECONDS must be a non-negative number, got ${process.env.BUILDKIT_PULL_BACKOFF_SECONDS}`);
  process.exit(1);
}

for (let attempt = 1; attempt <= pullAttempts; attempt++) {
  log(`    docker pull ${image} (attempt ${attempt}/${pullAttempts})`);
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

  if (attempt < pullAttempts) {
    sleepSeconds(attempt * backoffStepSeconds);
  }
}

error(
  `docker pull ${image} failed ${pullAttempts} times and the image is absent from the local daemon, ` +
    'so `buildx inspect --bootstrap` has nothing to boot from.',
);
process.exit(1);
