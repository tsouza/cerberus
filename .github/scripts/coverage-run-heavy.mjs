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
// in the log/notice text differs, supplied below.
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

import { createRunHeavyDecider } from './lib/run-heavy.mjs';

const { decide, runCLI } = createRunHeavyDecider({
  scriptName: 'coverage-run-heavy',
  heavyLanesPhrase: 'the heavy coverage lane',
  ordinaryPhrase: 'package-enrollment gate only, same as before',
  redundantPhrase: 'the heavy coverage lane and posted its own "coverage" check-run on its tip commit',
  firstRealPhrase: 'first real coverage run for this tree',
});

export { decide };

runCLI(import.meta.url);
