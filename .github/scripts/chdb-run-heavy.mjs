// chdb-run-heavy.mjs — decides whether `chdb.yml`'s release-gate lanes
// (`chdb-build`, `roundtrip (<ql>)`, `perf-guards`/`perf-guards-shard`,
// `integration-<ql>`) do real work for THIS event, closing part of
// tsouza/cerberus#2426 (a port of coverage-run-heavy.mjs's #2416 fix).
//
// The shape before this module existed
// --------------------------------------
// `chdb.yml`'s `changes` job computed `run_heavy` inline, in its `compute`
// step's shell script:
//
//   if event != pull_request && event != merge_group: true
//   elif head_ref starts with release/: true
//   else: false
//
// Semantically identical to `scope-gate.mjs`'s `runsFullLane` (the same
// helper `mutation.yml`, `compose-smoke-scope.mjs`, `coverage-run-heavy.mjs`
// and `property-run-heavy.mjs` already share), but hand-duplicated in shell
// rather than delegated to it — and, like every one of those lanes before
// their own #2416/#2426 fix, `true` on every `push` unconditionally. Correct
// for a push produced by an ORDINARY PR (the push is the FIRST real run for
// that tree) but REDUNDANT for a push produced by a `release/*`-headed PR:
// that PR's own `pull_request` run already had `run_heavy=true` and posted
// every one of chdb.yml's release-gate check-runs on its own tip commit.
//
// Unlike coverage.yml/property.yml (one job, one lane) and perf-profile.yml
// (two jobs, one lane), chdb.yml's `run_heavy` output feeds SIX lanes at
// once — `chdb-build`, `roundtrip` (3 matrix legs, per-STEP gated — see that
// job's own comment for why job-level would hide the per-leg context names),
// `perf-guards-shard`/`perf-guards`, and all three `integration-<ql>` jobs —
// all downstream of the SAME `changes` job. This module changes nothing
// about that fan-out: it only replaces the `compute` step's inline
// `run_heavy` shell branch with the SAME decision every other #2416/#2426
// lane now makes, so every downstream consumer keeps reading
// `needs.changes.outputs.run_heavy` exactly as before. `docs_only` (also
// computed in `changes`) is untouched — that is a separate, PR-only decision
// this module has no opinion on.
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
        ? `event "${event || '<unknown>'}" runs the heavy chdb lanes`
        : 'ordinary pull_request/merge_group — package-enrollment gate only, same as before',
    };
  }

  if (sourcePR && typeof sourcePR.headRef === 'string' && sourcePR.headRef.startsWith('release/')) {
    return {
      runHeavy: false,
      reason:
        `redundant with PR #${sourcePR.number} (${sourcePR.headRef}), which already ran the heavy chdb ` +
        `lanes and posted their check-runs on its tip commit — release-preflight.mjs's SOURCE-PR CREDIT ` +
        `(tsouza/cerberus#2394) reads that run instead of demanding a fresh one here`,
    };
  }

  return {
    runHeavy: true,
    reason: sourcePR
      ? `source PR #${sourcePR.number} (${sourcePR.headRef ?? '<no head ref>'}) was not release/*-headed — ` +
        'this push is the first real chdb run for this tree'
      : 'no source PR resolved for this push (ordinary-PR merge, unresolved match, or a maintenance-line ' +
        'hotfix with no PR at all) — running heavy for real (fail-safe default)',
  };
}

async function resolvePushSourcePR({ repo, sha, token, apiBase }) {
  if (!repo || !sha || !token) {
    notice(
      'chdb-run-heavy: GITHUB_REPOSITORY/GITHUB_SHA/GITHUB_TOKEN not all set for a push event — ' +
        'cannot resolve a source PR, running heavy for real (fail-safe default).',
    );
    return null;
  }
  try {
    return await resolveSourcePR({ repo, sha, token, apiBase });
  } catch (e) {
    notice(
      `chdb-run-heavy: could not resolve a source PR for ${String(sha).slice(0, 8)} (${e.message}) — ` +
        'running heavy for real (fail-safe default).',
    );
    return null;
  }
}

async function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`chdb-run-heavy: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log('chdb-run-heavy: RUN_HEAVY decision policy loaded.');
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
  notice(`chdb-run-heavy: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
  setOutput('run_heavy', String(verdict.runHeavy));
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`chdb-run-heavy failed: ${e.message}`);
    process.exit(1);
  });
}
