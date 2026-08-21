// perf-profile-run-heavy.mjs — decides whether `perf-profile.yml`'s heavy
// lane (the corpus-wide fan-out profile, both the `profile-shard` matrix and
// the `profile` aggregator) does real work for THIS event, closing part of
// tsouza/cerberus#2426 (a port of coverage-run-heavy.mjs's #2416 fix).
//
// The shape before this module existed
// --------------------------------------
// `perf-profile.yml` computed RUN_HEAVY via the SAME condition repeated in
// two places — `profile-shard`'s job-level `if:` (gating whether the whole
// 8-leg matrix spins up at all) and `profile`'s job-level `env:` (gating its
// own steps) — both byte-identical modulo the missing `merge_group` term
// coverage.yml's pre-#2425 expression had:
//
//   github.event_name != 'pull_request' || startsWith(github.head_ref, 'release/')
//
// true on every `push`, unconditionally. Correct for a push produced by an
// ORDINARY PR (the push is the FIRST real profile for that tree) but
// REDUNDANT for a push produced by a `release/*`-headed PR: that PR's own
// `pull_request` run already profiled the identical tree and posted
// `profile`'s required check-run on its own tip commit.
//
// Why this lane needs its OWN leading job (unlike coverage.yml/property.yml)
// ----------------------------------------------------------------------------
// coverage.yml and property.yml each compute RUN_HEAVY inside their own
// single job, where a `push` event's network call can run as an ordinary
// step and its result read by later steps in the SAME job via `steps.*`.
// perf-profile.yml's decision instead gates a JOB-LEVEL `if:` on
// `profile-shard` — skipping the whole 8-leg matrix, not just its steps, so
// an ordinary PR does not spin up 8 legs that would each immediately no-op —
// and a job's `if:` can only read the `needs`/`github`/`vars`/`secrets`/
// `inputs` contexts, never `steps` from within its own job. So the decision
// has to be made in an EARLIER job (`decide-run-heavy` in the workflow) and
// exposed as a job OUTPUT, which both `profile-shard` (`if:
// needs.decide-run-heavy.outputs.run_heavy == 'true'`) and `profile` (`env:
// RUN_HEAVY: ${{ needs.decide-run-heavy.outputs.run_heavy }}`, feeding the
// SAME `perf-profile-aggregate.mjs` drift check as before) can read.
//
// Everything else — the decision itself, the fail-safe default, the
// SOURCE-PR CREDIT cross-reference — is coverage-run-heavy.mjs's design
// verbatim; see that module's header for the full rationale. Only the lane
// name in the log/notice text differs, and `merge_group` is included in the
// non-push branch even though perf-profile.yml carries no `merge_group:`
// trigger today — `scope-gate.mjs`'s `runsFullLane` already handles it
// uniformly with every other lane that reuses it, so there is nothing
// lane-specific to special-case here.
//
// Modes (MODE, or argv[2]; default `verify`):
//   - verify: validate the module loads and the mode is known. No network.
//   - emit: decide and append `run_heavy=true|false` to GITHUB_OUTPUT. Calls
//     the GitHub API (via lib/resolve-source-pr.mjs) ONLY on a `push` event.
//
// Environment:
//   MODE                `emit` | `verify` (also argv[2]).
//   EVENT_NAME           github.event_name.
//   HEAD_REF             github.head_ref (pull_request only — perf-profile.yml
//                         carries no merge_group trigger).
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
        ? `event "${event || '<unknown>'}" runs the heavy profile lane`
        : 'ordinary pull_request — green no-op, same as before',
    };
  }

  if (sourcePR && typeof sourcePR.headRef === 'string' && sourcePR.headRef.startsWith('release/')) {
    return {
      runHeavy: false,
      reason:
        `redundant with PR #${sourcePR.number} (${sourcePR.headRef}), which already ran the heavy profile ` +
        `lane and posted its own "profile" check-run on its tip commit — release-preflight.mjs's ` +
        `SOURCE-PR CREDIT (tsouza/cerberus#2394) reads that run instead of demanding a fresh one here`,
    };
  }

  return {
    runHeavy: true,
    reason: sourcePR
      ? `source PR #${sourcePR.number} (${sourcePR.headRef ?? '<no head ref>'}) was not release/*-headed — ` +
        'this push is the first real profile run for this tree'
      : 'no source PR resolved for this push (ordinary-PR merge, unresolved match, or a maintenance-line ' +
        'hotfix with no PR at all) — running heavy for real (fail-safe default)',
  };
}

async function resolvePushSourcePR({ repo, sha, token, apiBase }) {
  if (!repo || !sha || !token) {
    notice(
      'perf-profile-run-heavy: GITHUB_REPOSITORY/GITHUB_SHA/GITHUB_TOKEN not all set for a push event — ' +
        'cannot resolve a source PR, running heavy for real (fail-safe default).',
    );
    return null;
  }
  try {
    return await resolveSourcePR({ repo, sha, token, apiBase });
  } catch (e) {
    notice(
      `perf-profile-run-heavy: could not resolve a source PR for ${String(sha).slice(0, 8)} (${e.message}) — ` +
        'running heavy for real (fail-safe default).',
    );
    return null;
  }
}

async function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`perf-profile-run-heavy: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log('perf-profile-run-heavy: RUN_HEAVY decision policy loaded.');
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
  notice(`perf-profile-run-heavy: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
  setOutput('run_heavy', String(verdict.runHeavy));
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-profile-run-heavy failed: ${e.message}`);
    process.exit(1);
  });
}
