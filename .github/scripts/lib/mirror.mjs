// mirror.mjs — the GHCR mirror of every upstream image cerberus's CI consumes,
// and the mapping from an upstream ref to its mirrored copy.
//
// WHY A MIRROR AND NOT MORE AUTHENTICATION.
//
// Docker Hub meters pulls against a counter that is per ACCOUNT when the pull
// is authenticated and per SOURCE IP when it is not. A GitHub-hosted runner
// shares its IP with every other job on that host, so the anonymous bucket is
// spent by strangers as readily as by us. The obvious remedy is to move every
// pull onto the account bucket by finding the paths that do not read the
// credentials `docker login` wrote — and that remedy is falsified, not merely
// incomplete. An AUTHENTICATED `docker pull` on the CLI path was refused as
// *unauthenticated* for `clickhouse/clickhouse-server` while `grafana/loki`
// succeeded on that same path 0.8s later (run 30713014897). Holding the path
// fixed and varying the image shows the refusal following the IMAGE. Whatever
// budget is exhausted, authenticating harder does not restore it.
//
// A mirror is immune to that question entirely: a ref that names GHCR reaches
// GHCR no matter which path resolves it — including paths nobody has audited —
// and draws on a budget no other tenant of this runner's IP is sharing. That
// property, not the rate limit itself, is why the mirror is the fix rather than
// one of two options.
//
// THE PACKAGES ARE PRIVATE. Publishing them would need `admin:packages` to flip
// visibility, and would put a copy of other projects' images under this org's
// name for anyone to pull — an outward-facing act this repository has no reason
// to take for a CI convenience. The cost is that every consuming job needs a
// `registry-login.mjs` step against `ghcr.io` with the job's own `GITHUB_TOKEN`
// (`packages: read`); that is mechanical, reviewable, and local to each job.
// Without it `acquireFromMirror` gets a denial, says so once at notice level,
// and the caller falls back to Docker Hub — so a job that forgets the login is
// slower and quota-exposed, never broken.
//
// HOW THE SWITCH IS MADE. Not by rewriting refs across the tree. `pull-images`
// callers ask for the UPSTREAM ref; `pullImageWithRetry` (lib/registry.mjs)
// fetches the mirrored copy and `docker tag`s it to the upstream name, so what
// lands in the local daemon is indistinguishable from an upstream pull. Compose
// files, Kubernetes manifests, the quickstart, and the Helm chart keep naming
// Docker Hub — which matters, because those files are read by operators who
// should not be pointed at a mirror this project maintains for its own CI.
//
// A mirror MISS is not an error. The caller falls back to the upstream ref
// under the same retry policy, so an image that has not been mirrored yet
// behaves exactly as it did before the mirror existed. The completeness gate is
// on the other side: `mirror-images.mjs` fails when it cannot mirror something
// in this inventory, and `TestMirrorInventoryCoversEveryUpstreamImage` fails
// when the tree names an upstream image this inventory omits.
//
// Exports:
//   mirrorRegistry         the GHCR namespace mirrored copies live under.
//   mirroredImages         every upstream ref cerberus's CI pulls.
//   mirroredRef(ref)       the mirrored ref for an inventoried image, else null.

import process from 'node:process';

// The namespace mirrored copies live under. Overridable so the mirroring job
// can be pointed at a scratch namespace when someone is changing the job
// itself; unset everywhere else, which is every CI run.
export const mirrorRegistry = (process.env.IMAGE_MIRROR_REGISTRY ?? '').trim() || 'ghcr.io/tsouza/cerberus-mirror';

// Every upstream image cerberus's CI pulls from Docker Hub, as the ref the tree
// names. Grouped by what consumes them so a reader can tell what a bump breaks.
//
// Docker Hub is the only registry represented: the other upstreams cerberus
// consumes are already on registries that do not meter this way
// (`gcr.io/distroless/*` in the Dockerfiles,
// `ghcr.io/open-telemetry/...telemetrygen` in the k3d sample app), so mirroring
// them would add a hop and a staleness window for no gain.
export const mirroredImages = [
  // The SQL substrate: the compatibility harnesses, the migration tiers, the
  // k3d stack, the startup bench, and the `-tags=integration` testcontainers
  // lanes (24.8 is the older server the replicated-DDL pins are proved on).
  'clickhouse/clickhouse-server:24.8-alpine',
  'clickhouse/clickhouse-server:25.8-alpine',
  'clickhouse/clickhouse-server:26.3',
  'clickhouse/clickhouse-server:26.5',
  'clickhouse/clickhouse-server:26.5-alpine',
  'clickhouse/clickhouse-server:26.6',
  'clickhouse/clickhouse-server:26.6-alpine',

  // Reference backends the three heads are diffed against, plus the reference
  // Prometheus the PromQL surface gate probes and the reference Mimir the
  // histogram bench measures against.
  'prom/prometheus:v3.11.3',
  'grafana/loki:3.7.0',
  'grafana/tempo:main-2f74ea8',
  'grafana/mimir:2.14.0',

  // The observability surface the e2e + migration lanes stand up.
  'grafana/grafana:12.2.9',
  'otel/opentelemetry-collector-contrib:0.152.1',

  // The k3s node image every k3d cluster boots from. The Justfile pre-pulls it
  // on the host and hands the cached copy to `k3d cluster create --image`
  // precisely because a mid-flight cluster-creation pull from Docker Hub timed
  // out and failed whole e2e runs; the mirror is what removes the remaining
  // dependence on Docker Hub answering at all (issue #1514).
  'rancher/k3s:v1.31.5-k3s1',

  // The backwards-compatibility lane's object store.
  'minio/minio:RELEASE.2025-09-07T16-13-09Z',
  'minio/mc:RELEASE.2025-08-13T08-35-41Z',

  // Init containers in the k3d sample app.
  'busybox:1.37',

  // The builder buildx boots from. Pulled by the host daemon before any build
  // runs, so a refusal here fails the job before it has done anything.
  'moby/buildkit:buildx-stable-1',

  // The Go toolchain image every cerberus image builds FROM, and the ONLY
  // Docker Hub ref this tree reaches through a `FROM` rather than a compose
  // `image:` key or a `docker pull` — every other base image is
  // `gcr.io/distroless/*`, which is not metered this way. That distinction
  // matters because the two acquisition shapes need different treatment: the
  // re-tag in `pullImageWithRetry` populates the HOST daemon, and the
  // `docker-container` BuildKit driver has its own image store that never
  // consults it, so a `FROM` resolves against the registry regardless. The
  // build CONSUMES the mirrored copy by naming it in a build arg — see
  // `buildBaseImageArgs` below. It is the ref that opened the incident:
  // `unexpected status from HEAD request …/library/golang/manifests/1.26: 429`
  // is what reddened `e2e` and `migration-e2e` on main.
  'golang:1.26',
];

// buildBaseImageArgs — every build arg this tree's Dockerfiles use to name a
// base image, paired with the upstream ref that arg defaults to. One entry per
// `ARG <name>=<ref>` + `FROM ${<name>}` pair.
//
// The indirection exists because a host-side pre-pull cannot reach a `FROM`:
// the `docker-container` BuildKit driver resolves it against the registry from
// its own content store. Naming the ref is the only lever, so the ref has to be
// substitutable — and substitutable only from the outside, because the DEFAULT
// must stay the upstream ref. The mirror packages are private, so a Dockerfile
// that defaulted to them would be unbuildable by anyone outside this repo.
//
// This table is the single place the arg name and its upstream ref are paired.
// `buildBaseImageRef` reads it to decide what CI passes, and the regression pin
// checks it against the Dockerfiles and against the inventory above, so an arg
// renamed in one place and not the other fails rather than silently reverting
// every build to Docker Hub.
export const buildBaseImageArgs = Object.freeze({ GO_IMAGE: 'golang:1.26' });

const inventory = new Set(mirroredImages);

// A ref's first path segment is a REGISTRY HOST rather than a Docker Hub owner
// exactly when it looks like a host: it carries a dot (`ghcr.io`, `gcr.io`) or
// a port (`localhost:5000`). This is the same rule the docker CLI applies, and
// it is why `minio/mc` resolves to Docker Hub while `gcr.io/distroless/static`
// does not.
function isDockerHubRef(ref) {
  const slash = ref.indexOf('/');
  if (slash === -1) return true; // a bare name is a Docker Hub `library/` image
  const first = ref.slice(0, slash);
  return !first.includes('.') && !first.includes(':');
}

// mirroredRef — the GHCR ref holding a copy of `ref`, or null when there is
// none to reach for. Null is returned for anything not in the inventory, so an
// image the mirror has never been told about resolves upstream exactly as
// before rather than 404ing against a namespace that never held it.
export function mirroredRef(ref) {
  const upstream = String(ref ?? '').trim();
  if (upstream === '' || !inventory.has(upstream) || !isDockerHubRef(upstream)) return null;

  // Docker Hub's own canonical form for a bare name, so `golang:1.26` and
  // `library/golang:1.26` cannot mirror to two different packages.
  const path = upstream.includes('/') ? upstream : `library/${upstream}`;
  return `${mirrorRegistry}/${path}`;
}
