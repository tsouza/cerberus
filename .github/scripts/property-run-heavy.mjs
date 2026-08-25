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
// `merge_group` / `schedule` / `workflow_dispatch` stay unchanged). The
// decision logic, the fail-safe network wrapper and the verify/emit CLI
// scaffold itself live in lib/run-heavy.mjs, shared with chdb-run-heavy.mjs,
// coverage-run-heavy.mjs and perf-profile-run-heavy.mjs. Only the lane name
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
  scriptName: 'property-run-heavy',
  heavyLanesPhrase: 'the heavy property lane',
  ordinaryPhrase: 'green no-op, same as before',
  redundantPhrase: 'the heavy property sweep and posted its own "property (…)" check-run on its tip commit',
  firstRealPhrase: 'first real property sweep for this tree',
});

export { decide };

runCLI(import.meta.url);
