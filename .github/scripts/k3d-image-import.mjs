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
// A third failure mode is the Docker HOST's image store, not the cluster
// (cerberus issue #3589). `k3d image import` runs `ctr image import
// --all-platforms` inside the node over a `docker save` tarball. On the
// classic overlay2 graphdriver (every ubuntu-latest CI runner) that tarball
// holds exactly the one platform the daemon pulled, so `--all-platforms`
// is a no-op. On the containerd image store (`docker info` →
// `driver-type: io.containerd.snapshotter.v1`, the Docker Desktop and
// modern Docker Engine default) `docker save` writes the image's FULL
// multi-platform index but only the pulled platform's blobs, and
// `--all-platforms` then demands the other platforms' manifests:
// `ctr: content digest sha256:…: not found`. Every one of k3d's import
// modes (`tools-node`, `direct`) passes that flag, so on the containerd
// store this script bypasses k3d and streams `docker save` straight into
// the server node's `ctr -n k8s.io images import -` — which, without
// `--all-platforms`, imports the node's own platform only and accepts the
// containerd-store tarball. The store is detected once from `docker info`
// and the overlay2 path is the unchanged `k3d image import` CI runs today.
//
// Designed for reuse from day one (cerberus issue #3096's own scope note):
// the `e2e-bwc-up` lane (sub-issue #3097) imports an EXTENDED image list
// while EXCLUDING the standalone ClickHouse image
// (`clickhouse/clickhouse-server:*-alpine`) its kustomization never
// applies — both an arbitrary image list and an exclusion-filter were
// therefore first-class parameters here from the start, not a retrofit:
// `e2e-bwc-up` reuses this script by changing only its own image list +
// exclude pattern at the call site, never this script's interface. The
// multi-data-shard `e2e-datashard-up` reuses it the same way (cerberus
// issue #3107).
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
// containerd; 1 as soon as one exhausts its import attempts, or when the
// Docker host's image store cannot be determined (`docker info` failing).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { capture, error, log, notice } from './lib/gh.mjs';
import { filterImages, matchesAnyExcludePattern } from './lib/image-globs.mjs';

// The exclusion glob lives in lib/image-globs.mjs (pull-images.mjs applies
// the same one); re-exported so the contract stays testable from here.
export { filterImages, matchesAnyExcludePattern };

const defaultAttempts = 5;
const defaultBackoffStepSeconds = 2;

// imageStore — the two Docker host image stores this script tells apart,
// each with the import path it selects (see the module header).
export const imageStore = Object.freeze({
  graphdriver: 'graphdriver',
  containerd: 'containerd',
});

// How `docker info` names the containerd image store: its `.DriverStatus`
// carries one `["driver-type", "io.containerd.snapshotter.v1"]` row, where
// the classic graphdriver lists filesystem facts and no `driver-type` row.
const driverTypeRow = 'driver-type';
const containerdDriverTypePrefix = 'io.containerd.snapshotter.';
const dockerInfoDriverStatusArgs = ['info', '--format', '{{json .DriverStatus}}'];

// The POSIX `sh -c` pipeline the containerd path runs. `$1` is the image
// ref and `$2` the server node container, passed as positional parameters
// so neither is ever interpolated into shell text. Deliberately no
// `--all-platforms`: that flag is exactly what makes k3d's own import
// refuse the containerd-store tarball. dash has no `pipefail`, so the
// status this pipeline reports is `ctr`'s alone; a `docker save` failure
// surfaces as a truncated tarball `ctr` rejects, and the containerd
// verification after every attempt is the arbiter either way, the same
// contract the k3d path has always had.
const containerdStoreImportPipeline =
  'docker save "$1" | docker exec -i "$2" ctr -n k8s.io images import -';

// Human-readable names for the failure message: what the store is and the
// path this script took on it, so an exhausted import says which of the
// two mechanisms the operator has to look at.
const storeDescription = Object.freeze({
  [imageStore.graphdriver]: { store: 'overlay2 graphdriver', path: 'k3d image import' },
  [imageStore.containerd]: { store: 'containerd image store', path: 'docker save | docker exec ctr images import' },
});

// parseImageStore — map the JSON `docker info --format '{{json
// .DriverStatus}}'` prints to one of `imageStore`. Throws on anything that
// is not the `[[key, value], …]` array docker prints, so a daemon answering
// nonsense never silently selects a path.
export function parseImageStore(driverStatusJson) {
  let rows;
  try {
    rows = JSON.parse(driverStatusJson);
  } catch (e) {
    throw new Error(`docker info DriverStatus is not JSON: ${String(driverStatusJson).trim()} (${e.message})`);
  }
  if (!Array.isArray(rows)) {
    throw new Error(`docker info DriverStatus is not a row array: ${String(driverStatusJson).trim()}`);
  }
  const driverType = rows.find((row) => Array.isArray(row) && row[0] === driverTypeRow);
  if (driverType && String(driverType[1]).startsWith(containerdDriverTypePrefix)) return imageStore.containerd;
  return imageStore.graphdriver;
}

// detectImageStore — ask the Docker host which image store it runs. Throws
// when `docker info` fails: with no daemon to answer, neither import path
// can work, and guessing one would only move the failure somewhere quieter.
export function detectImageStore(captureImpl = capture) {
  const res = captureImpl('docker', dockerInfoDriverStatusArgs);
  if (res.status !== 0) {
    throw new Error(
      `docker info ${dockerInfoDriverStatusArgs.slice(1).join(' ')} failed (status ${res.status}): ${res.stderr.trim()}`,
    );
  }
  return parseImageStore(res.stdout);
}

// importOnce — one import attempt of `img` on the given store; returns the
// capture result of the command run. On the graphdriver store this is the
// exact `k3d image import` invocation CI has always run; on the containerd
// store it is the `docker save | ctr import` pipeline (module header).
export function importOnce(img, store, { cluster, serverContainer, captureImpl = capture }) {
  if (store === imageStore.containerd) {
    return captureImpl('sh', ['-c', containerdStoreImportPipeline, 'sh', img, serverContainer]);
  }
  return captureImpl('k3d', ['image', 'import', img, '-c', cluster]);
}

// exhaustedMessage — the ::error:: body once an image's attempt budget is
// spent: the ref, the store the host runs, the path taken on it, and the
// last import command's stderr (empty for the k3d path, whose exit status
// and output say nothing — see the module header).
export function exhaustedMessage(ref, attempts, store, lastStderr) {
  const { store: storeName, path } = storeDescription[store];
  const tail = lastStderr.trim() === '' ? '' : `; last import error: ${lastStderr.trim()}`;
  return `${ref} missing from k3d node containerd after ${attempts} import attempts (host image store: ${storeName}; import path: ${path}${tail})`;
}

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

// importAndVerifyOne — import `img` into `cluster` over the path `store`
// selects, retrying up to `attempts` times with linear backoff, verifying
// against `serverContainer` after each attempt. Returns `{ landed: true }`
// once verified, `{ landed: false, lastStderr }` once the attempt budget is
// spent. `captureImpl`/`sleepFn` default to the real implementations; tests
// inject fakes for both so the retry loop's control flow (attempt count,
// backoff schedule, early exit on landing, path selection) is verifiable
// without a real docker/k3d or real wall-clock waits.
export async function importAndVerifyOne(
  img,
  { cluster, serverContainer, store, attempts, backoffStepSeconds, sleepFn = sleep, captureImpl = capture },
) {
  const ref = normalizeRef(img);
  let lastStderr = '';
  for (let attempt = 1; attempt <= attempts; attempt++) {
    // `k3d image import` reports "Successfully imported" and exits 0 even
    // on a silent node-level failure (see the module header) — no import
    // command's exit status decides anything here; only the containerd
    // verification below decides landed/not-landed. The stderr is kept for
    // the exhausted-budget message, where the containerd path's `ctr`
    // error is the one line that says why.
    lastStderr = importOnce(img, store, { cluster, serverContainer, captureImpl }).stderr;
    if (containerdHasImage(serverContainer, ref, captureImpl)) return { landed: true, lastStderr };
    log(`    import attempt ${attempt}/${attempts}: ${ref} not in containerd yet, retrying after backoff`);
    if (attempt < attempts) await sleepFn(attempt * backoffStepSeconds * 1000);
  }
  return { landed: false, lastStderr };
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

  let store;
  try {
    store = detectImageStore();
  } catch (e) {
    error(`cannot determine the Docker host's image store, so no import path can be chosen: ${e.message}`);
    process.exit(1);
  }
  log(`==> docker host image store: ${storeDescription[store].store} — importing via ${storeDescription[store].path}`);

  for (const img of toImport) {
    const { landed, lastStderr } = await importAndVerifyOne(img, {
      cluster,
      serverContainer,
      store,
      attempts,
      backoffStepSeconds,
    });
    if (!landed) {
      error(exhaustedMessage(normalizeRef(img), attempts, store, lastStderr));
      process.exit(1);
    }
    log(`    ok ${normalizeRef(img)}`);
  }

  notice(`k3d-image-import: ${toImport.length} image(s) imported and verified into cluster ${cluster}`);
  process.exit(0);
}
