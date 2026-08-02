// pull-images.mjs — acquire a named list of images into the local docker
// daemon under the shared registry policy.
//
// The compose lanes resolve their image list from a compose model
// (`compose-pull-images.mjs`); the k3d and integration lanes just know theirs —
// the images `k3d image import` loads into the cluster, and the ClickHouse
// servers testcontainers starts. Both kinds need the same thing: the bytes in
// the local daemon, fetched over a path that carries credentials, retried on a
// transport fault, and abandoned at once on a quota refusal.
//
// This module is that second shape. It exists so the Justfile's `_pull-retry`
// stops hand-rolling `docker pull` inside a shell `for` loop: a hand-rolled loop
// reaches Docker Hub directly, which means it never consults the GHCR mirror
// (`lib/mirror.mjs`) and spends quota the mirror exists to stop spending.
//
// Usage:
//   node .github/scripts/pull-images.mjs <ref>...
//
// Env:
//   IMAGE_PULL_BACKOFF_SECONDS  (optional; default 3) linear backoff step —
//                               attempt N sleeps N × this many seconds.
//
// Exit: 0 when every ref is in the local daemon, 1 as soon as one is not.

import process from 'node:process';

import { error } from './lib/gh.mjs';
import { pullImageWithRetry, readBackoffStepSeconds } from './lib/registry.mjs';

// Matches the compose pre-pull's step: these lanes pull the same images from the
// same registry, so they wait the same way.
const imagePullBackoffStepSeconds = 3;

const refs = process.argv.slice(2).filter((a) => a.trim() !== '');
if (refs.length === 0) {
  error('usage: node .github/scripts/pull-images.mjs <image>... — no image refs given.');
  process.exit(1);
}

const backoffStepSeconds = readBackoffStepSeconds('IMAGE_PULL_BACKOFF_SECONDS', imagePullBackoffStepSeconds);

for (const ref of refs) {
  // First failure ends the run: the lane that asked for these images cannot
  // start without them, and a second pull into a spent quota only deepens the
  // deficit for every concurrent job.
  if (!pullImageWithRetry(ref, { backoffStepSeconds, consequence: `the lane cannot start without ${ref}` })) {
    process.exit(1);
  }
}
