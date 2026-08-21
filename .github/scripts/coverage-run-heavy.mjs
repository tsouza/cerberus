// coverage-run-heavy.mjs — decides whether `coverage.yml`'s heavy lane
// (`just coverage`: the full instrumented test run + chdb install) does real
// work for THIS event, closing part of tsouza/cerberus#2416 (part 2 of
// #2394).
//
// The shape before this module existed
// --------------------------------------
// `coverage.yml` computed RUN_HEAVY as a single job-level GHA expression:
//
//   (event != pull_request && event != merge_group) || startsWith(head_ref, 'release/')
//
// which is true on every `push`, unconditionally. That is correct for a push
// produced by an ORDINARY PR — RUN_HEAVY was false on that PR's own run, so
// the push is the FIRST real coverage measurement for that tree — but it is
// REDUNDANT for a push produced by a `release/*`-headed PR: that PR's own
// `pull_request` run already had RUN_HEAVY=true (same expression, its own
// head_ref matched `release/*`) and posted `coverage`'s required check-run on
// its own tip commit. The push-to-main run after such a PR merges re-runs the
// identical `just coverage` work against a byte-identical tree for nothing.
//
// The fix
// -------
// For `pull_request` / `merge_group`, behaviour is UNCHANGED — delegated to
// `scope-gate.mjs`'s `runsFullLane`, the same helper `mutation.yml` and
// `e2e.yml`'s `compose-smoke-scope` already share, so this lane's PR-time
// decision cannot drift from theirs. For `schedule` / `workflow_dispatch`,
// also unchanged: always heavy (the nightly/manual safety net every scoped
// path leans on).
//
// For `push`, `decide()` takes an ADDITIONAL input: the PR (if any) that
// produced the pushed commit, resolved via `lib/resolve-source-pr.mjs`'s
// exact `merged && merge_commit_sha === pushedSha` match (tsouza/cerberus#2394).
// RUN_HEAVY is false ONLY when that PR resolved AND its own head ref started
// with `release/` — i.e. only when we can PROVE the identical tree already
// ran the heavy lane and posted `coverage`'s check-run somewhere
// `release-preflight.mjs`'s SOURCE-PR CREDIT will find it (on the PR's own
// tip commit). Every other push — an ordinary-PR merge, an unresolved source
// PR (network hiccup, or a maintenance-line hotfix pushed with no PR at all,
// which release.yml's preflight requires this check-run to exist for) —
// keeps running heavy. Fail-safe by construction: uncertainty always resolves
// to "run it for real", never to "skip it".
//
// Modes (MODE, or argv[2]; default `verify`):
//   - verify: validate the module loads and the mode is known. No network.
//   - emit: decide and append `run_heavy=true|false` to GITHUB_OUTPUT. Calls
//     the GitHub API (via lib/resolve-source-pr.mjs) ONLY on a `push` event.
//
// Environment:
//   MODE                `emit` | `verify` (also argv[2]).
//   EVENT_NAME           github.event_name.
//   HEAD_REF             github.head_ref (pull_request / merge_group only).
//   GITHUB_SHA           the pushed commit sha (push event only).
//   GITHUB_REPOSITORY    "owner/name" (push event only).
//   GITHUB_TOKEN         token with pull-requests:read (push event only).
//   GITHUB_API_URL       API base, default https://api.github.com.
//   GITHUB_OUTPUT        emit destination.
//
// node: builtins only — no npm dependencies or setup-node step.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { runsFullLane } from './lib/scope-gate.mjs';
import { resolveSourcePR } from './lib/resolve-source-pr.mjs';
import { error, log, notice, setOutput } from './lib/gh.mjs';

// decide — pure. `sourcePR` is only meaningful (and only ever passed) for a
// `push` event: `{ number, headRef }` for an exact-match resolved PR, or
// `null` when none resolved (no PR, ambiguous match, or a resolution
// failure the caller already logged). Ignored for every other event.
export function decide({ eventName, headRef, sourcePR = null }) {
  const event = String(eventName ?? '').trim();

  if (event !== 'push') {
    const runHeavy = runsFullLane({ eventName: event, headRef });
    return {
      runHeavy,
      reason: runHeavy
        ? `event "${event || '<unknown>'}" runs the heavy coverage lane`
        : 'ordinary pull_request/merge_group — package-enrollment gate only, same as before',
    };
  }

  if (sourcePR && typeof sourcePR.headRef === 'string' && sourcePR.headRef.startsWith('release/')) {
    return {
      runHeavy: false,
      reason:
        `redundant with PR #${sourcePR.number} (${sourcePR.headRef}), which already ran the heavy coverage ` +
        `lane and posted its own "coverage" check-run on its tip commit — release-preflight.mjs's ` +
        `SOURCE-PR CREDIT (tsouza/cerberus#2394) reads that run instead of demanding a fresh one here`,
    };
  }

  return {
    runHeavy: true,
    reason: sourcePR
      ? `source PR #${sourcePR.number} (${sourcePR.headRef ?? '<no head ref>'}) was not release/*-headed — ` +
        'this push is the first real coverage run for this tree'
      : 'no source PR resolved for this push (ordinary-PR merge, unresolved match, or a maintenance-line ' +
        'hotfix with no PR at all) — running heavy for real (fail-safe default)',
  };
}

async function resolvePushSourcePR({ repo, sha, token, apiBase }) {
  if (!repo || !sha || !token) {
    notice(
      'coverage-run-heavy: GITHUB_REPOSITORY/GITHUB_SHA/GITHUB_TOKEN not all set for a push event — ' +
        'cannot resolve a source PR, running heavy for real (fail-safe default).',
    );
    return null;
  }
  try {
    return await resolveSourcePR({ repo, sha, token, apiBase });
  } catch (e) {
    notice(
      `coverage-run-heavy: could not resolve a source PR for ${String(sha).slice(0, 8)} (${e.message}) — ` +
        'running heavy for real (fail-safe default).',
    );
    return null;
  }
}

async function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`coverage-run-heavy: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log('coverage-run-heavy: RUN_HEAVY decision policy loaded.');
    return;
  }

  const eventName = process.env.EVENT_NAME;
  const headRef = process.env.HEAD_REF;

  let sourcePR = null;
  if (String(eventName ?? '').trim() === 'push') {
    sourcePR = await resolvePushSourcePR({
      repo: process.env.GITHUB_REPOSITORY,
      sha: process.env.GITHUB_SHA,
      token: process.env.GITHUB_TOKEN,
      apiBase: process.env.GITHUB_API_URL || 'https://api.github.com',
    });
  }

  const verdict = decide({ eventName, headRef, sourcePR });
  notice(`coverage-run-heavy: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
  setOutput('run_heavy', String(verdict.runHeavy));
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`coverage-run-heavy failed: ${e.message}`);
    process.exit(1);
  });
}
