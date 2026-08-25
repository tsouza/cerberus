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
// The decision itself, the fail-safe default, the SOURCE-PR CREDIT
// cross-reference and the verify/emit CLI scaffold live in
// lib/run-heavy.mjs, shared verbatim with coverage-run-heavy.mjs,
// property-run-heavy.mjs and perf-profile-run-heavy.mjs — see that module's
// header for the full rationale (why `pull_request` / `merge_group` /
// `schedule` / `workflow_dispatch` stay unchanged). Only the lane name in
// the log/notice text differs, supplied below.
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
  scriptName: 'chdb-run-heavy',
  heavyLanesPhrase: 'the heavy chdb lanes',
  ordinaryPhrase: 'package-enrollment gate only, same as before',
  redundantPhrase: 'the heavy chdb lanes and posted their check-runs on its tip commit',
  firstRealPhrase: 'first real chdb run for this tree',
});

export { decide };

runCLI(import.meta.url);
