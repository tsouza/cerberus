// migration-cerberus-image.mjs — resolve and put the cerberus image the
// migration stacks run into the local Docker daemon: the SINGLE acquisition
// point, shared by CI and local dev. Extracted from the `migration-cerberus-image`
// Justfile recipe body (CLAUDE.md invariant 15, issue #3099, epic #3091).
//
// Two paths, decided by CERBERUS_IMAGE alone: unset (every local run, every PR /
// dispatch / push CI run) it builds MIGRATION_LOCAL_IMAGE from the repo-root
// Dockerfile.local — whose final unnamed stage IS the cerberus image, so no
// `--target` is needed — and the stack proves this tree. Set (release.yml's
// artifact lane) it pulls that released tag and the stack proves the artifact.
//
// This exists because the compose up path deliberately cannot acquire the image
// itself: `pull_policy: never` and no `--build` on the up recipes (migration-tier1-up
// / migration-tier2-up in just/migration.just) are what keep a release run from
// recompiling the source tree over the released image.
//
// The build path shells out to build-with-registry-retry.mjs (the one retry
// wrapper every image-building command in the tree goes through — see that
// module's own header); the pull path shells out to `just _pull-retry`
// (just/common.just), the SAME recipe every other pull-then-run lane already
// uses. Neither is re-implemented here: this script owns only the
// build-vs-pull DECISION the replaced bash `if` made, not the two mechanisms
// either branch already had.
//
// Env contract:
//   MIGRATION_LOCAL_IMAGE  required; the locally-built tag (just/migration.just's
//                          own MIGRATION_LOCAL_IMAGE variable, suffix included).
//   CERBERUS_IMAGE         optional; a released image ref (release.yml's
//                          artifact lane). Unset or empty selects the build path.
//
// Exit: the status of whichever command (build-with-registry-retry.mjs or
// `just _pull-retry`) it ran; 1 if MIGRATION_LOCAL_IMAGE is missing.

import { spawnSync } from 'node:child_process';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';

// resolveCerberusImage — the exact `img="${CERBERUS_IMAGE:-$local_tag}"` /
// `if [ "$img" = "$local_tag" ]` decision the replaced bash made. Pure so the
// build-vs-pull branch is unit-testable without a Docker daemon.
//
// Deliberately compares the RESOLVED image to localImage, not whether
// cerberusImageEnv was set: a caller who explicitly sets CERBERUS_IMAGE to the
// exact local tag still gets the build path, matching the bash's own
// string-equality test rather than an "is CERBERUS_IMAGE set" check.
export function resolveCerberusImage(cerberusImageEnv, localImage) {
  const image = cerberusImageEnv && cerberusImageEnv !== '' ? cerberusImageEnv : localImage;
  return { image, source: image === localImage ? 'build' : 'pull' };
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('migration-cerberus-image.mjs');
}

if (isMain()) {
  const localImage = process.env.MIGRATION_LOCAL_IMAGE;
  if (!localImage) {
    error('migration-cerberus-image: MIGRATION_LOCAL_IMAGE must be set (see just/migration.just).');
    process.exit(1);
  }

  const { image, source } = resolveCerberusImage(process.env.CERBERUS_IMAGE || '', localImage);

  let res;
  if (source === 'build') {
    log(`==> migration cerberus image: build ${image} from Dockerfile.local`);
    res = spawnSync(
      'node',
      [
        '.github/scripts/build-with-registry-retry.mjs',
        'docker',
        'build',
        '--build-arg',
        'GO_IMAGE',
        '-f',
        'Dockerfile.local',
        '-t',
        image,
        '.',
      ],
      { stdio: 'inherit' },
    );
  } else {
    log(`==> migration cerberus image: pull ${image}`);
    res = spawnSync('just', ['_pull-retry', image], { stdio: 'inherit' });
  }

  if (res.error) {
    error(`migration-cerberus-image: failed to run the ${source} command: ${res.error.message}`);
    process.exit(1);
  }
  process.exit(res.status ?? 1);
}
