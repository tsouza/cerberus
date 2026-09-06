// k3d-image-import.mjs — import a list of images into a k3d cluster and
// VERIFY each one actually landed in the node's containerd, with retry.
// Extracted from `e2e-up`'s image-import-and-verify loop (cerberus issue
// #3096, epic #3091 phase 5).
//
// Two failure modes this loop guards against (both observed live, see
// just/e2e.just's own history for the run ids):
//   1. k3d bundles EVERY image into one tarball, mounts it into a
//      transient tools node, and runs `ctr image import`; the bundled
//      tarball intermittently vanishes mid-import, a transient
//      image-volume race whose window grows with the bundle size — so
//      importing ONE image at a time shrinks both the tarball and the
//      race.
//   2. `k3d image import` prints "Successfully imported" and exits 0 even
//      when a node-level import silently failed (leaving the pod
//      `ImagePullBackOff` with `pullPolicy: Never`), so success has to be
//      VERIFIED against the node's own containerd, never trusted from
//      k3d's own exit code.
//
// Designed for reuse from day one (cerberus issue #3096's own scope note):
// the `e2e-bwc-up` lane (sub-issue #3097) imports an EXTENDED image list
// while EXCLUDING the standalone ClickHouse image
// (`clickhouse/clickhouse-server:*-alpine`) its kustomization never
// applies — both an arbitrary image list and an exclusion-filter were
// therefore first-class parameters here from the start, not a retrofit:
// `e2e-bwc-up` reuses this script by changing only its own image list +
// exclude pattern at the call site, never this script's interface. The
// multi-data-shard `e2e-datashard-up` still duplicates this exact loop
// today — tracked separately, see cerberus issue #3107.
//
// Usage:
//   node .github/scripts/k3d-image-import.mjs <image>...
//
// Env:
//   K3D_CLUSTER            required; the k3d cluster name (`-c` to
//                          `k3d image import`, and names the server-0
//                          container `k3d-<cluster>-server-0` containerd
//                          is queried against).
//   IMAGE_IMPORT_EXCLUDE   optional; whitespace-separated glob patterns
//                          (`*` matches within one `/`-or-`:`-delimited
//                          segment, e.g. `clickhouse/clickhouse-server:*-alpine`).
//                          Any argv image matching ANY pattern is skipped
//                          entirely — never imported, never verified.
//   IMAGE_IMPORT_ATTEMPTS         optional; default 5.
//   IMAGE_IMPORT_BACKOFF_SECONDS  optional; default 2 — attempt N sleeps
//                                 N × this many seconds, matching the
//                                 extracted bash's `sleep $((attempt * 2))`.
//
// Exit: 0 when every non-excluded image is verified present in the node's
// containerd; 1 as soon as one exhausts its import attempts.

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { capture, error, log, notice } from './lib/gh.mjs';

const defaultAttempts = 5;
const defaultBackoffStepSeconds = 2;

// normalizeRef — the exact `case` translation the extracted bash used to
// turn a short image name into the fully-qualified ref containerd stores it
// under: a ref with a registry host (a dot or a port before the first `/`)
// or an explicit `host:port/...` form is left alone; a `namespace/name`
// short ref gets a `docker.io/` prefix; a bare `name` gets
// `docker.io/library/`.
export function normalizeRef(img) {
  const slash = img.indexOf('/');
  const firstSegment = slash === -1 ? img : img.slice(0, slash);
  const looksLikeRegistryHost = firstSegment.includes('.') || firstSegment.includes(':');
  if (slash !== -1 && looksLikeRegistryHost) return img;
  if (slash !== -1) return `docker.io/${img}`;
  return `docker.io/library/${img}`;
}

// globToRegExp — a minimal single-`*` image-ref glob: `*` matches any run
// of characters EXCLUDING `/` (so a pattern never accidentally spans a
// repository-path boundary it didn't ask to). Deliberately separate from
// ci-lane-contract.mjs's own `matchesGlob` (file-path globs, `**` included):
// an image ref's `/`-and-`:`-delimited shape is a different domain, and the
// one pattern this needs (`clickhouse/clickhouse-server:*-alpine`) is
// simple enough that borrowing a bigger, unrelated gate script's glob
// engine would be a needless coupling for one function.
function globToRegExp(glob) {
  const escaped = glob.replace(/[|\\{}()[\]^$+?.]/g, '\\$&').replace(/\*/g, '[^/]*');
  return new RegExp(`^${escaped}$`);
}

// matchesAnyExcludePattern — true when `img` (the argv literal, e.g.
// `clickhouse/clickhouse-server:26.3-alpine`) matches at least one of
// `patterns`.
export function matchesAnyExcludePattern(img, patterns) {
  return patterns.some((p) => globToRegExp(p).test(img));
}

// filterImages — argv images minus every one matching an exclude pattern,
// preserving order. Pure so the exclusion-filter contract is unit-testable
// without a k3d cluster.
export function filterImages(images, excludePatterns) {
  if (excludePatterns.length === 0) return [...images];
  return images.filter((img) => !matchesAnyExcludePattern(img, excludePatterns));
}

// containerdHasImage — true when `ref` is present in the k3d server node's
// containerd image store. A non-zero status (the node not up yet, or not
// found at all) is treated as "not landed", never thrown — a transient
// error answering this query is exactly the retry loop's own precondition,
// not a reason to abort. `captureImpl` defaults to the real `capture()` so
// callers never have to pass it; tests inject a fake to avoid shelling out
// to a real docker/k3d.
export function containerdHasImage(serverContainer, ref, captureImpl = capture) {
  const res = captureImpl('docker', ['exec', serverContainer, 'ctr', '-n', 'k8s.io', 'images', 'ls', '-q']);
  if (res.status !== 0) return false;
  return res.stdout.split('\n').some((line) => line.trim() === ref);
}

// importAndVerifyOne — import `img` into `cluster`, retrying up to
// `attempts` times with linear backoff, verifying against `serverContainer`
// after each attempt. Returns true once verified, false once the attempt
// budget is spent. `captureImpl`/`sleepFn` default to the real
// implementations; tests inject fakes for both so the retry loop's control
// flow (attempt count, backoff schedule, early exit on landing) is
// verifiable without a real docker/k3d or real wall-clock waits.
export async function importAndVerifyOne(
  img,
  { cluster, serverContainer, attempts, backoffStepSeconds, sleepFn = sleep, captureImpl = capture },
) {
  const ref = normalizeRef(img);
  for (let attempt = 1; attempt <= attempts; attempt++) {
    // `k3d image import` reports "Successfully imported" and exits 0 even
    // on a silent node-level failure (see the module header) — its exit
    // status is deliberately never inspected here; only the containerd
    // verification below decides landed/not-landed.
    captureImpl('k3d', ['image', 'import', img, '-c', cluster]);
    if (containerdHasImage(serverContainer, ref, captureImpl)) return true;
    log(`    import attempt ${attempt}/${attempts}: ${ref} not in containerd yet, retrying after backoff`);
    if (attempt < attempts) await sleepFn(attempt * backoffStepSeconds * 1000);
  }
  return false;
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('k3d-image-import.mjs');
}

if (isMain()) {
  const images = process.argv.slice(2).filter((a) => a.trim() !== '');
  if (images.length === 0) {
    error('usage: node .github/scripts/k3d-image-import.mjs <image>... — no image refs given.');
    process.exit(1);
  }

  const cluster = process.env.K3D_CLUSTER;
  if (!cluster) {
    error('K3D_CLUSTER is required (the k3d cluster name to import into and verify against).');
    process.exit(1);
  }
  const serverContainer = `k3d-${cluster}-server-0`;

  const excludePatterns = (process.env.IMAGE_IMPORT_EXCLUDE || '').split(/\s+/).filter(Boolean);
  const attempts = Number(process.env.IMAGE_IMPORT_ATTEMPTS || String(defaultAttempts));
  const backoffStepSeconds = Number(process.env.IMAGE_IMPORT_BACKOFF_SECONDS || String(defaultBackoffStepSeconds));

  const toImport = filterImages(images, excludePatterns);
  const skipped = images.filter((img) => !toImport.includes(img));
  if (skipped.length > 0) {
    log(`==> excluding ${skipped.length} image(s) matching IMAGE_IMPORT_EXCLUDE: ${skipped.join(', ')}`);
  }

  for (const img of toImport) {
    const landed = await importAndVerifyOne(img, { cluster, serverContainer, attempts, backoffStepSeconds });
    if (!landed) {
      error(`${normalizeRef(img)} missing from k3d node containerd after ${attempts} import attempts`);
      process.exit(1);
    }
    log(`    ok ${normalizeRef(img)}`);
  }

  notice(`k3d-image-import: ${toImport.length} image(s) imported and verified into cluster ${cluster}`);
  process.exit(0);
}
