// property-run-heavy.mjs — decides whether `property.yml`'s heavy lane (the
// rapid N=500 property sweep) does real work for THIS event, closing part of
// tsouza/cerberus#2426 (a straight port of coverage-run-heavy.mjs's fix for
// tsouza/cerberus#2416).
//
// The shape before this module existed
// --------------------------------------
// `property.yml` computed RUN_HEAVY as a single job-level GHA expression,
// byte-identical to coverage.yml's pre-#2425 one:
//
//   (event != pull_request && event != merge_group) || startsWith(head_ref, 'release/')
//
// true on every `push`, unconditionally. Correct for a push produced by an
// ORDINARY PR — RUN_HEAVY was false on that PR's own run, so the push is the
// FIRST real rapid sweep for that tree — but REDUNDANT for a push produced by
// a `release/*`-headed PR: that PR's own `pull_request` run already had
// RUN_HEAVY=true (same expression, its own head_ref matched `release/*`) and
// posted `property (…)`'s required check-run on its own tip commit. The
// push-to-main run after such a PR merges re-runs the identical rapid N=500
// sweep against a byte-identical tree for nothing.
//
// The fix, and everything below, is coverage-run-heavy.mjs's own design
// verbatim — see that module's header for the full rationale (the fail-safe
// default, the SOURCE-PR CREDIT cross-reference, why `pull_request` /
// `merge_group` / `schedule` / `workflow_dispatch` stay unchanged). Only the
// lane name in the log/notice text differs.
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
        ? `event "${event || '<unknown>'}" runs the heavy property lane`
        : 'ordinary pull_request/merge_group — green no-op, same as before',
    };
  }

  if (sourcePR && typeof sourcePR.headRef === 'string' && sourcePR.headRef.startsWith('release/')) {
    return {
      runHeavy: false,
      reason:
        `redundant with PR #${sourcePR.number} (${sourcePR.headRef}), which already ran the heavy property ` +
        `sweep and posted its own "property (…)" check-run on its tip commit — release-preflight.mjs's ` +
        `SOURCE-PR CREDIT (tsouza/cerberus#2394) reads that run instead of demanding a fresh one here`,
    };
  }

  return {
    runHeavy: true,
    reason: sourcePR
      ? `source PR #${sourcePR.number} (${sourcePR.headRef ?? '<no head ref>'}) was not release/*-headed — ` +
        'this push is the first real property sweep for this tree'
      : 'no source PR resolved for this push (ordinary-PR merge, unresolved match, or a maintenance-line ' +
        'hotfix with no PR at all) — running heavy for real (fail-safe default)',
  };
}

async function resolvePushSourcePR({ repo, sha, token, apiBase }) {
  if (!repo || !sha || !token) {
    notice(
      'property-run-heavy: GITHUB_REPOSITORY/GITHUB_SHA/GITHUB_TOKEN not all set for a push event — ' +
        'cannot resolve a source PR, running heavy for real (fail-safe default).',
    );
    return null;
  }
  try {
    return await resolveSourcePR({ repo, sha, token, apiBase });
  } catch (e) {
    notice(
      `property-run-heavy: could not resolve a source PR for ${String(sha).slice(0, 8)} (${e.message}) — ` +
        'running heavy for real (fail-safe default).',
    );
    return null;
  }
}

async function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`property-run-heavy: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log('property-run-heavy: RUN_HEAVY decision policy loaded.');
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
  notice(`property-run-heavy: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
  setOutput('run_heavy', String(verdict.runHeavy));
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`property-run-heavy failed: ${e.message}`);
    process.exit(1);
  });
}
