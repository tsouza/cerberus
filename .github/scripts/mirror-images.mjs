// mirror-images.mjs — copy every upstream image cerberus's CI consumes into
// the project's own GHCR namespace.
//
// The inventory and the upstream→mirror mapping live in `lib/mirror.mjs`, which
// also carries the argument for why a mirror is the durable fix and a per-path
// `docker login` is not. This module is the other half: the thing that puts
// bytes behind that mapping.
//
// COPIED, NOT REBUILT. `docker buildx imagetools create --tag <mirror>
// <upstream>` copies the manifest LIST — every architecture, the original
// digests, the original config — without materialising layers in the local
// daemon. `docker pull && docker tag && docker push` would flatten a multi-arch
// image to the runner's own architecture, which is how a mirror silently starts
// serving amd64 to an arm64 consumer.
//
// FAILURE IS FATAL HERE, ON PURPOSE. A consumer treats a mirror miss as a
// fallback and pulls upstream, because failing a lane over an image that is
// perfectly reachable upstream would be strictly worse than the problem the
// mirror solves. That tolerance is only honest if the completeness question is
// asked somewhere it can be answered — which is here. A ref this module cannot
// copy fails the job, in the one place where the fix is to run the job again or
// to correct the inventory.
//
// Usage:
//   node .github/scripts/mirror-images.mjs           copy every inventoried ref
//   node .github/scripts/mirror-images.mjs --list    print the plan, copy nothing
//
// Env:
//   IMAGE_MIRROR_REGISTRY  (optional) GHCR namespace to copy INTO; see
//                          `lib/mirror.mjs`. Unset in CI.
//   GITHUB_STEP_SUMMARY    (optional) appended with the per-image outcome table.
//
// Exit: 0 when every inventoried image is in the mirror, 1 otherwise.

import { appendFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, log, notice } from './lib/gh.mjs';
import { mirrorRegistry, mirroredImages, mirroredRef } from './lib/mirror.mjs';

// plan — the (upstream, mirror) pairs this run would copy. Exported so the test
// can assert the inventory maps cleanly without spawning docker.
export function plan() {
  return mirroredImages.map((upstream) => ({ upstream, mirror: mirroredRef(upstream) }));
}

function copy(upstream, mirror) {
  const res = capture('docker', ['buildx', 'imagetools', 'create', '--tag', mirror, upstream]);
  if (res.status === 0) return { ok: true, detail: 'copied' };
  return { ok: false, detail: (res.stderr + res.stdout).trim().split('\n').slice(-1)[0] ?? 'failed' };
}

function writeSummary(rows) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path === undefined || path.trim() === '') return;

  const lines = [
    `### Image mirror → \`${mirrorRegistry}\``,
    '',
    '| upstream | outcome |',
    '| --- | --- |',
    ...rows.map((r) => `| \`${r.upstream}\` | ${r.ok ? '✅ ' : '❌ '}${r.detail} |`),
    '',
  ];
  appendFileSync(path, `${lines.join('\n')}\n`);
}

function main(argv) {
  const pairs = plan();

  // A null mirror here is an inventory bug, not a miss: everything in the
  // inventory is by definition something the inventory claims to mirror.
  const unmappable = pairs.filter((p) => p.mirror === null).map((p) => p.upstream);
  if (unmappable.length > 0) {
    error(
      `mirroredRef() returned no mirror for inventoried image(s): ${unmappable.join(', ')}. ` +
        'An entry that names a registry other than Docker Hub does not belong in the inventory.',
    );
    return 1;
  }

  if (argv.includes('--list')) {
    for (const { upstream, mirror } of pairs) log(`${upstream} -> ${mirror}`);
    return 0;
  }

  const rows = [];
  for (const { upstream, mirror } of pairs) {
    log(`==> ${upstream} -> ${mirror}`);
    const outcome = copy(upstream, mirror);
    rows.push({ upstream, ...outcome });
    if (!outcome.ok) error(`could not mirror ${upstream}: ${outcome.detail}`);
  }
  writeSummary(rows);

  const failed = rows.filter((r) => !r.ok);
  if (failed.length > 0) {
    error(
      `${failed.length} of ${rows.length} image(s) were not mirrored. Consumers fall back to Docker Hub for ` +
        'those, which is the exact quota exposure the mirror exists to remove.',
    );
    return 1;
  }

  notice(`mirrored ${rows.length} image(s) into ${mirrorRegistry}`);
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main(process.argv.slice(2)));
}
