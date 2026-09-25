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
//   IMAGE_PULL_EXCLUDE          (optional) whitespace-separated image-ref
//                               globs (lib/image-globs.mjs) to skip — the
//                               bwc / datashard lanes pass the standalone
//                               `clickhouse/clickhouse-server:*-alpine` image
//                               their kustomization never applies, the same
//                               pattern k3d-image-import.mjs's
//                               IMAGE_IMPORT_EXCLUDE takes.
//
// The refs are pulled through `pullImages`' bounded pool (lib/registry.mjs),
// each under the full per-image policy, with each image's log lines emitted as
// one block when it finishes.
//
// Exit: 0 when every non-excluded ref is in the local daemon, 1 when any is
// not — after naming every failed ref and every ref left unstarted.

import process from 'node:process';

import { error, log } from './lib/gh.mjs';
import { filterImages } from './lib/image-globs.mjs';
import { pullImageAsync, pullImages, readBackoffStepSeconds } from './lib/registry.mjs';

// Matches the compose pre-pull's step: these lanes pull the same images from the
// same registry, so they wait the same way.
const imagePullBackoffStepSeconds = 3;

const refs = process.argv.slice(2).filter((a) => a.trim() !== '');
if (refs.length === 0) {
  error('usage: node .github/scripts/pull-images.mjs <image>... — no image refs given.');
  process.exit(1);
}

const backoffStepSeconds = readBackoffStepSeconds('IMAGE_PULL_BACKOFF_SECONDS', imagePullBackoffStepSeconds);

const excludePatterns = (process.env.IMAGE_PULL_EXCLUDE || '').split(/\s+/).filter(Boolean);
const toPull = filterImages(refs, excludePatterns);
const excluded = refs.filter((ref) => !toPull.includes(ref));
if (excluded.length > 0) {
  log(`==> excluding ${excluded.length} image(s) matching IMAGE_PULL_EXCLUDE: ${excluded.join(', ')}`);
}

// The first failure stops the pool from starting another pull: the lane that
// asked for these images cannot start without them, and a further pull into a
// spent quota only deepens the deficit for every concurrent job. Pulls already
// in flight finish, so their outcome is reported rather than cut off.
const { failed, skipped } = await pullImages(toPull, {
  backoffStepSeconds,
  stopOnFailure: true,
  pull: (ref, options) => pullImageAsync(ref, { ...options, consequence: `the lane cannot start without ${ref}` }),
});
if (failed.length > 0) {
  const unstarted = skipped.length === 0 ? '' : `; not attempted after the first failure: ${skipped.join(', ')}`;
  error(
    `${failed.length} of ${toPull.length} image(s) could not be acquired: ${failed.join(', ')}${unstarted}. ` +
      'The lane cannot start without them; each failure is diagnosed above.',
  );
  process.exit(1);
}
