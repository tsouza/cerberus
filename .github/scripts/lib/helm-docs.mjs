// lib/helm-docs.mjs — the one place this repo runs the containerized
// helm-docs image, extracted out of prepare-release.mjs (#3100) so that
// module's changelog/version utilities stay import-safe for a consumer that
// has no business acquiring a Docker image.
//
// verify-changelog-fresh.mjs imports prepare-release.mjs for its changelog
// helpers alone, and pr-hygiene.yml's `pr-body` job runs it unauthenticated
// (it never touches a registry). assert-image-jobs-authenticate.mjs follows
// a module's relative imports to attribute what a job acquires, so keeping
// the docker-run call in the SAME file as those changelog helpers made every
// consumer of that file — including one that never runs it — look like it
// acquires the helm-docs image too. Isolating the image-acquiring code in
// its own module means only prepare-release.mjs (the one caller that
// actually needs it, gated behind RENDER_HELM_DOCS=1) imports this file, and
// the authentication requirement lands on exactly the job that needs it:
// prepare-release.yml's `prepare` job.
import { spawnSync } from 'node:child_process'
import process from 'node:process'

import { pullImageWithRetry } from './registry.mjs'

export const HELM_DOCS_IMAGE = 'jnorwood/helm-docs:v1.14.2'

const HELM_CHART_SEARCH_ROOT = 'deploy/helm'
const HELM_DOCS_TEMPLATE = 'README.md.gotmpl'

// renderHelmDocs regenerates the chart README via the same containerized
// helm-docs both `just release-prep` and `just release-prep-backport` ran
// inline, against the Chart.yaml prepare-release.mjs has already rewritten.
// `cwd` (default `process.cwd()`, i.e. the repo root every caller runs this
// script from) is the volume mount source, matching the replaced recipes'
// own `$PWD/deploy/helm`.
//
// `docker run` pulls a missing image itself, single-attempt, so a Docker Hub
// blip or a quota refusal would have surfaced here as an opaque container
// start failure — the same class of fault issue #1562 fixed everywhere else
// a script drives docker directly (promql-surface-gate.mjs's reference
// Prometheus, migration-artifact.mjs's release image). `pullImageWithRetry`
// acquires the image through the shared transport-retry / rate-limit-fails-
// fast policy first, so this site is judged the identical way.
export function renderHelmDocs(cwd = process.cwd()) {
  if (!pullImageWithRetry(HELM_DOCS_IMAGE, { consequence: 'the chart README cannot be regenerated' })) {
    throw new Error(`could not acquire the helm-docs image ${HELM_DOCS_IMAGE}`)
  }

  const res = spawnSync(
    'docker',
    [
      'run',
      '--rm',
      '-v',
      `${cwd}/${HELM_CHART_SEARCH_ROOT}:/helm-docs`,
      '-u',
      String(process.getuid()),
      HELM_DOCS_IMAGE,
      '--chart-search-root=/helm-docs',
      `--template-files=${HELM_DOCS_TEMPLATE}`,
    ],
    { stdio: 'inherit' },
  )
  if (res.error) throw res.error
  if (res.status !== 0) throw new Error(`helm-docs exited ${res.status}`)
}
