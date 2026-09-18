// release-is-latest.mjs — is the tag being released the highest STABLE
// release? The single "is this the newest line?" signal every downstream
// owner-of-a-shared-resource decision in release.yml reads.
//
// A stable BACKPORT (v1.2.1 cut after v1.3.0) is NOT highest, and must not
// take the rolling `:latest` image tags (goreleaser's `dockers_v2` tag
// template renders empty unless `.Env.RELEASE_IS_LATEST` is "true"), the
// Homebrew cask (`homebrew_casks[].skip_upload` templates to `true` off the
// same variable — `auto` alone filters only prereleases), or the GitHub
// `Latest` release pointer (the `publish` job's explicit `--latest` flip,
// which otherwise follows publish ORDER). Prereleases (`v1.4.0-rc.1`) never
// count as a stable line.
//
// Env:
//   TAG          the tag being released (`v1.2.3`)
//   GITHUB_ENV   receives `RELEASE_IS_LATEST=<true|false>` for the later steps
//   GITHUB_OUTPUT receives `is_latest=<true|false>` for the later jobs
//
// The checkout needs the full tag list (release.yml's `fetch-depth: 0`),
// including the tag just pushed.

import { execFileSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { error, exportEnv, log, setOutput } from './lib/gh.mjs';

const STABLE_TAG = /^v(\d+)\.(\d+)\.(\d+)$/;

// parseStable — the [major, minor, patch] of a stable `vX.Y.Z` tag, or null
// for anything else (a prerelease suffix, a chart tag, a branch name).
export function parseStable(tag) {
  const m = STABLE_TAG.exec(tag);
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

function compare(a, b) {
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] - b[i];
  return 0;
}

// highestStable — the highest stable tag among `tags`, or null when none.
export function highestStable(tags) {
  let best = null;
  for (const tag of tags) {
    const v = parseStable(tag);
    if (v && (best === null || compare(v, best.v) > 0)) best = { tag, v };
  }
  return best?.tag ?? null;
}

// isLatest — the tag is stable AND nothing stable is higher.
export function isLatest(tag, tags) {
  const v = parseStable(tag);
  if (!v) return false;
  const top = highestStable([...tags, tag]);
  return top !== null && compare(parseStable(top), v) === 0;
}

function main() {
  const tag = process.env.TAG;
  if (!tag) throw new Error('TAG is required');
  const tags = execFileSync('git', ['tag', '-l', 'v*'], { encoding: 'utf8' }).split('\n').filter(Boolean);
  const latest = isLatest(tag, tags);
  exportEnv([['RELEASE_IS_LATEST', String(latest)]]);
  setOutput('is_latest', String(latest));
  log(`tag=${tag} highest_stable=${highestStable([...tags, tag]) ?? '(none)'} RELEASE_IS_LATEST=${latest}`);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (e) {
    error(e.message);
    process.exit(1);
  }
}
