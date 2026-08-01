// compose-pull-images.mjs — acquire every image a compose stack fetches,
// before `docker compose up` reaches for them, over the pull path that carries
// the runner's Docker Hub credentials.
//
// WHY NOT `docker compose pull`, which is what this replaces:
//
// The two pull paths a job has do not share a credential source. Measured on
// run 30702344415, job 91376064528 — one job, one runner, one IP, four seconds
// apart:
//
//   13:53:19  docker/login-action    Login Succeeded!
//   13:53:22  docker pull moby/buildkit:buildx-stable-1   -> all layers pulled
//   13:53:27  docker compose up ... clickhouse            -> toomanyrequests:
//             You have reached your UNAUTHENTICATED pull rate limit.
//
// The anonymous per-IP counter was demonstrably exhausted at 13:53:27, so an
// anonymous pull at 13:53:22 would have been refused too — it was not. The CLI
// pull carried the credentials `docker login` wrote; compose's did not, and
// Docker Hub's own error names the request as unauthenticated. Two further jobs
// show the same split, one of them with no buildx step at all, which rules the
// `docker-container` driver out as the variable (issue #1565).
//
// The consequence for the pre-pull is worse than "it did not help": run through
// compose, the mechanism built to absorb Docker Hub failures was itself spending
// the ANONYMOUS quota, failing, and handing `up` a still-empty daemon and a
// counter it had just pushed further into deficit. Resolving the model here and
// fetching each image with `docker pull` puts the pre-pull on the authenticated
// path, where the credentials the job established actually apply.
//
// WHAT IS PULLABLE is read off the compose model's `build:` sections — compose's
// own `--ignore-buildable` semantics — never inferred from what the daemon
// holds. Absence from the daemon means "not built yet" as often as "not fetched
// yet": Tier-2's `cerberus-migration-tier2:dead-end-receiver` is built during
// `up` and exists in no registry, and a presence check classified it as "needs
// pulling", failing the lane five attempts deep (run 30281594098).
//
// Presence in the daemon is still consulted, for one narrower purpose: to skip
// an image already held, which is what `--policy missing` did. That only ever
// removes a fetch, so it cannot resurrect the bug above.
//
// Usage:
//   node .github/scripts/compose-pull-images.mjs <compose-file>...
//
// Env:
//   COMPOSE_PULL_BACKOFF_SECONDS  (optional; default 3) linear backoff step
//                                 handed to the shared retry policy.
//
// Exit: 0 when every pullable image is in the local daemon, 1 otherwise.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, log, notice } from './lib/gh.mjs';
import { pullImageWithRetry, readBackoffStepSeconds } from './lib/registry.mjs';

// Matches the buildx bootstrap step: a compose stack's images are a manifest
// and some layers apiece, not a build's dependency closure.
const composeBackoffStepSeconds = 3;

// A service compose BUILDS rather than fetches, in every form the model states
// it. `build:` is the one `--ignore-buildable` reads; the two pull policies say
// the same thing about a service that also names an image.
function isBuildable(service) {
  if (service === null || typeof service !== 'object') return false;
  if (service.build !== undefined && service.build !== null) return true;
  return service.pull_policy === 'build' || service.pull_policy === 'never';
}

// pullableImages — the images `docker compose up` would fetch from a registry,
// as a sorted, deduplicated list. Exported for the self-test: the model→image
// decision is the part worth pinning, and it is a pure function of the parsed
// compose config.
export function pullableImages(model) {
  const services = model?.services;
  if (services === null || typeof services !== 'object') return [];

  const refs = new Set();
  for (const service of Object.values(services)) {
    if (isBuildable(service)) continue;
    const image = typeof service?.image === 'string' ? service.image.trim() : '';
    if (image !== '') refs.add(image);
  }
  return [...refs].sort();
}

function resolveModel(files) {
  const args = ['compose'];
  for (const file of files) args.push('-f', file);
  args.push('config', '--format', 'json');

  const res = capture('docker', args);
  if (res.status !== 0) {
    error(
      `docker ${args.join(' ')} failed, so the compose model could not be read and no image can be ` +
        `pre-pulled:\n${res.stdout}${res.stderr}`,
    );
    process.exit(1);
  }

  try {
    return JSON.parse(res.stdout);
  } catch (err) {
    error(`docker ${args.join(' ')} did not print parseable JSON (${err.message}):\n${res.stdout}`);
    process.exit(1);
  }
}

function presentLocally(image) {
  return capture('docker', ['image', 'inspect', image]).status === 0;
}

function main(files) {
  if (files.length === 0) {
    error('compose-pull-images: no compose file given. Pass every `-f` file the lane brings up.');
    process.exit(1);
  }

  const images = pullableImages(resolveModel(files));
  if (images.length === 0) {
    notice(`no fetchable image in ${files.join(', ')} — every service is built by compose.`);
    return 0;
  }

  const backoffStepSeconds = readBackoffStepSeconds('COMPOSE_PULL_BACKOFF_SECONDS', composeBackoffStepSeconds);
  let failed = 0;
  for (const image of images) {
    if (presentLocally(image)) {
      log(`    ${image} already in the local daemon`);
      continue;
    }
    if (!pullImageWithRetry(image, { backoffStepSeconds })) failed++;
  }

  if (failed > 0) {
    error(
      `${failed} of ${images.length} compose image(s) could not be acquired; \`docker compose up\` would pull ` +
        'them itself, single-attempt and unretried.',
    );
    return 1;
  }
  return 0;
}

// Run only when invoked as the entrypoint, so `compose-pull-images.test.mjs`
// can import `pullableImages` without the import pulling images as a side
// effect.
if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main(process.argv.slice(2)));
}
