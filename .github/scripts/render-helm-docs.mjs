#!/usr/bin/env node
// render-helm-docs.mjs — regenerate the chart README through the containerized
// helm-docs, for a caller that is a workflow step rather than another script.
//
// WHY THIS EXISTS (issue #3187). chart-ci.yml's `chart-validate` job — a
// REQUIRED status check — ran helm-docs as `uses: docker://jnorwood/helm-docs`.
// A Docker container action makes the runner pull that image, and with no
// registry login that pull is anonymous against Docker Hub: it spends the
// shared per-runner-IP quota and is refused as UNAUTHENTICATED under load,
// reddening a required check for a reason unrelated to the change.
//
// It was also invisible. assert-image-jobs-authenticate.mjs returned early on
// any `uses:` that was neither `docker/login-action` nor a local composite, so
// the gate reported "26 jobs, all authenticated" with `chart-validate` not
// among them. Both halves are fixed: the scanner now reads `uses: docker://`
// as the acquisition it is, and this script routes the acquisition through the
// shared mirror-first, transport-retrying policy in lib/registry.mjs.
//
// The identical `uses: docker://jnorwood/helm-docs` step was removed from
// prepare-release.yml in #3100 and folded into lib/helm-docs.mjs. This is the
// same remedy applied to the copy that was left behind — one implementation of
// "run helm-docs", called from both places, rather than a second one here.
//
// Env: none. The render target is fixed in lib/helm-docs.mjs (deploy/helm,
// README.md.gotmpl), so both callers regenerate the same thing; a caller that
// could point it elsewhere would be able to make the drift check pass by
// rendering something other than what ships.
//
// Exit codes: 0 = the README was regenerated, 1 = the image could not be
// acquired or helm-docs failed.

import process from 'node:process';

import { renderHelmDocs } from './lib/helm-docs.mjs';
import { error } from './lib/gh.mjs';

try {
  renderHelmDocs();
} catch (e) {
  error(`render-helm-docs: ${e.message}`);
  process.exit(1);
}
