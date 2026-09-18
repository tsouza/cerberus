// release-tag.mjs — create and push the `v<appVersion>` annotated tag at the
// merge commit, for release.yml's `goreleaser` job.
//
// goreleaser reads the version from this tag, so it has to exist before
// goreleaser runs. The push does NOT recurse into another release run: the
// workflow's only trigger is push-to-main (the raw-tag trigger was retired).
// Idempotent: a re-run that finds the tag already present at the SAME commit
// is a no-op; a tag pointing at a DIFFERENT commit is a hard error, because a
// moved release tag is never correct.
//
// This script is invoked ONLY from release.yml. It must never be reachable
// from a Justfile recipe: release-version-gate.mjs decides whether to publish
// by asking whether `v<appVersion>` already exists, so a tag cut by hand does
// not START a release, it permanently CANCELS one
// (test/regression/release_required_checks_test.go,
// TestNoJustfileRecipePushesAReleaseTag).
//
// Env:
//   TAG         the tag to create (`v1.2.3`)
//   GITHUB_SHA  the merge commit the tag points at
//   REMOTE      the remote to push to (default `origin`)
//
// Exit: 0 when the tag exists at GITHUB_SHA (created now, or already there);
// 1 when it exists elsewhere or an input is missing.

import { execFileSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { error, log } from './lib/gh.mjs';

const BOT_NAME = 'github-actions[bot]';
const BOT_EMAIL = 'github-actions[bot]@users.noreply.github.com';

function git(args, { cwd, allowFailure = false } = {}) {
  try {
    return execFileSync('git', args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  } catch (e) {
    if (allowFailure) return null;
    throw new Error(`git ${args.join(' ')} failed: ${e.stderr?.toString().trim() || e.message}`);
  }
}

// tagCommit — the commit a tag points at, or null when the tag does not exist.
export function tagCommit(tag, { cwd } = {}) {
  if (git(['rev-parse', '-q', '--verify', `refs/tags/${tag}`], { cwd, allowFailure: true }) === null) return null;
  return git(['rev-list', '-n1', tag], { cwd });
}

// ensureReleaseTag — the decision, with its side effects behind `cwd`.
// Returns 'created' | 'present'; throws when the tag exists elsewhere.
export function ensureReleaseTag({ tag, sha, remote = 'origin', cwd }) {
  if (!tag || !sha) throw new Error('TAG and GITHUB_SHA are required');
  const existing = tagCommit(tag, { cwd });
  if (existing !== null) {
    if (existing !== sha) {
      throw new Error(`tag ${tag} already exists at ${existing}, not the merge commit ${sha} — refusing to move a release tag`);
    }
    return 'present';
  }
  git(['-c', `user.name=${BOT_NAME}`, '-c', `user.email=${BOT_EMAIL}`, 'tag', '-a', tag, '-m', `release ${tag}`, sha], { cwd });
  git(['push', remote, `refs/tags/${tag}`], { cwd });
  return 'created';
}

function main() {
  const tag = process.env.TAG;
  const sha = process.env.GITHUB_SHA;
  const remote = process.env.REMOTE || 'origin';
  const outcome = ensureReleaseTag({ tag, sha, remote });
  log(outcome === 'created' ? `created + pushed ${tag} at ${sha}` : `tag ${tag} already at ${sha} — nothing to do`);
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
