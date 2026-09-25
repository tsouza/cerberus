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
// decision cannot drift from theirs. For `workflow_dispatch`, also
// unchanged: always heavy (the manual safety net every scoped path leans
// on).
//
// For `push`, the decision takes an ADDITIONAL input: the PR (if any) that
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
// This decision logic, the fail-safe default, the SOURCE-PR CREDIT
// cross-reference and the verify/emit CLI scaffold live in
// lib/run-heavy.mjs, shared verbatim with chdb-run-heavy.mjs,
// property-run-heavy.mjs and perf-profile-run-heavy.mjs — only the lane name
// in the log/notice text differs, supplied to it below.
//
// # Schedule is not unconditional here (tsouza/cerberus#3708)
//
// The other three `lib/run-heavy.mjs` consumers run on runner pools with no
// cross-run contention, so their `schedule` events stay unconditionally
// heavy via the shared decider. `coverage.yml` is different: it is the only
// coalesced workflow that shares the small `cerberus-heavy` pool (3 runners)
// with ci.yml's required `lint` / `check-test` / `check-build`, and
// main-coalescing.mjs deliberately gives a main push and the equivalent
// nightly cron SEPARATE concurrency groups (neither may cancel the other's
// evidence) — so the two runs' heavy lanes can be in flight together,
// together wanting up to 4 of those 3 runners. Before this run's
// schedule-triggered lane commits to its own heavy work, it asks whether a
// push-to-main run of this same workflow is already queued or in progress
// (lib/coverage-schedule-overlap.mjs); if so, that push run is already
// measuring a tip at least as recent, so THIS run skips its heavy lanes —
// coverage-enrollment still runs regardless — and yields the pool. The next
// nightly tick gets another chance. This is one-directional: a push's own
// decision never depends on the schedule.
//
// Modes (MODE, or argv[2]; default `verify`):
//   - verify: validate the module loads and the mode is known. No network.
//   - emit: decide and append `run_heavy=true|false` to GITHUB_OUTPUT. Calls
//     the GitHub API on a `push` event (resolve the source PR) and on a
//     `schedule` event (check for an overlapping push run).
//
// Environment:
//   MODE                `emit` | `verify` (also argv[2]).
//   EVENT_NAME           github.event_name.
//   HEAD_REF             github.head_ref (pull_request / merge_group only).
//   GITHUB_SHA           the pushed commit sha (push event only).
//   GITHUB_REPOSITORY    "owner/name" (push / schedule events only).
//   GITHUB_TOKEN         token with pull-requests:read + actions:read
//                        (push / schedule events only).
//   GITHUB_API_URL       API base, default https://api.github.com.
//   GITHUB_OUTPUT        emit destination.
//
// node: builtins only — no npm dependencies or setup-node step.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { createRunHeavyDecider } from './lib/run-heavy.mjs';
import { checkOverlappingMainPushRun } from './lib/coverage-schedule-overlap.mjs';
import { error, log, notice, setOutput } from './lib/gh.mjs';

const SCRIPT_NAME = 'coverage-run-heavy';

const { decide: baseDecide, resolvePushSourcePR } = createRunHeavyDecider({
  scriptName: SCRIPT_NAME,
  heavyLanesPhrase: 'the heavy coverage lane',
  ordinaryPhrase: 'package-enrollment gate only, same as before',
  redundantPhrase: 'the heavy coverage lane and posted its own "coverage" check-run on its tip commit',
  firstRealPhrase: 'first real coverage run for this tree',
});

// decide — pure, wrapping the shared lib/run-heavy.mjs decision with the ONE
// coverage-specific override: a schedule event yields when `scheduleOverlap`
// (an already-resolved overlap record, or null/undefined) says a push run
// already holds or is about to hold cerberus-heavy for this tip. Every other
// event, and a schedule run with no overlap, delegates unchanged.
export function decide({ eventName, headRef, sourcePR = null, scheduleOverlap = null }) {
  const event = String(eventName ?? '').trim();
  if (event === 'schedule' && scheduleOverlap) {
    return {
      runHeavy: false,
      reason:
        `main-push run #${scheduleOverlap.runId} (${scheduleOverlap.status}) already holds or is about to hold ` +
        'cerberus-heavy for a tip at least as recent — skipping the nightly heavy lanes so combined occupancy ' +
        'cannot exceed the pool (tsouza/cerberus#3708); package-enrollment still runs, and the next nightly ' +
        'tick gets another chance',
    };
  }
  return baseDecide({ eventName, headRef, sourcePR });
}

async function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`${SCRIPT_NAME}: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log(`${SCRIPT_NAME}: RUN_HEAVY decision policy loaded.`);
    return;
  }

  const eventName = process.env.EVENT_NAME;
  const headRef = process.env.HEAD_REF;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
  const event = String(eventName ?? '').trim();

  let sourcePR = null;
  let scheduleOverlap = null;
  if (event === 'push') {
    sourcePR = await resolvePushSourcePR({
      repo: process.env.GITHUB_REPOSITORY,
      sha: process.env.GITHUB_SHA,
      token: process.env.GITHUB_TOKEN,
      apiBase,
    });
  } else if (event === 'schedule') {
    scheduleOverlap = await checkOverlappingMainPushRun({
      repo: process.env.GITHUB_REPOSITORY,
      token: process.env.GITHUB_TOKEN,
      apiBase,
    });
    if (scheduleOverlap) {
      notice(`${SCRIPT_NAME}: found overlapping push run #${scheduleOverlap.runId} (${scheduleOverlap.status})`);
    }
  }

  const verdict = decide({ eventName, headRef, sourcePR, scheduleOverlap });
  notice(`${SCRIPT_NAME}: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
  setOutput('run_heavy', String(verdict.runHeavy));
}

function runCLI(moduleUrl) {
  const invokedDirectly = process.argv[1] && moduleUrl === pathToFileURL(process.argv[1]).href;
  if (!invokedDirectly) return;
  main().catch((e) => {
    error(`${SCRIPT_NAME} failed: ${e.message}`);
    process.exit(1);
  });
}

runCLI(import.meta.url);
