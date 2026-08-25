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
// SOURCE-PR CREDIT cross-reference, and the verify/emit CLI scaffold — lives
// in lib/run-heavy.mjs, shared verbatim with chdb-run-heavy.mjs,
// coverage-run-heavy.mjs and property-run-heavy.mjs; see that module's
// header for the full rationale. Only the lane name in the log/notice text
// differs, supplied below, and `merge_group` is included in the non-push
// branch even though perf-profile.yml carries no `merge_group:` trigger
// today — `scope-gate.mjs`'s `runsFullLane` already handles it uniformly
// with every other lane that reuses it, so there is nothing lane-specific to
// special-case here.
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

import { createRunHeavyDecider } from './lib/run-heavy.mjs';

const { decide, runCLI } = createRunHeavyDecider({
  scriptName: 'perf-profile-run-heavy',
  heavyLanesPhrase: 'the heavy profile lane',
  ordinaryPhrase: 'green no-op, same as before',
  ordinaryEventLabel: 'pull_request',
  redundantPhrase: 'the heavy profile lane and posted its own "profile" check-run on its tip commit',
  firstRealPhrase: 'first real profile run for this tree',
});

export { decide };

runCLI(import.meta.url);
