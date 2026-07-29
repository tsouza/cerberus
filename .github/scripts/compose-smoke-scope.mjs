// compose-smoke-scope.mjs — decides whether a pull request has to boot the
// repo-root compose stack (e2e.yml, the `compose-smoke-scope` job).
//
// Why this exists
// ---------------
// compose-smoke is a RELEASE gate, not a PR gate: on an ordinary pull request
// the whole lane short-circuits in seconds and the aggregate reports green, and
// the real ~25-minute stack boot happens on the merge commit, where release.yml
// reads it by name before anything publishes. That trade was made deliberately
// to cut PR wall time, and most of it is still right — the average PR has no
// business booting ClickHouse, an OTel collector, a seeder and Grafana.
//
// What it cost is a class of bug this lane is the ONLY layer that catches.
// Every other execution layer — the TXTAR roundtrips, the property lane, the
// compatibility harnesses — runs against chDB, and chDB is more forgiving than
// a real ClickHouse server: it coerces column types the server type-checks and
// rejects outright. An emit-type or response-shape defect therefore stays green
// everywhere and surfaces as a 502 against a production server. That is exactly
// how a Tempo handler shipped a non-JSON body for a multi-phi
// quantile_over_time: nothing failed until the release lane ran, where it
// blocked the release rather than the PR that introduced it.
//
// So the gate is neither "always" nor "never": a PR that touches the surface
// this stack can see boots it, and every other PR short-circuits exactly as
// before. Push, schedule, dispatch and `release/*` PRs are untouched — they
// always ran the full lane and still do.
//
// Two modes (env MODE, or argv[2]; default `verify`):
//   - verify : assert every declared path still exists in the tree. A scope
//              entry that has been renamed away matches nothing and silently
//              stops gating — the failure mode is a lane that quietly goes back
//              to never running, which looks identical to a lane that is
//              correctly short-circuiting.
//   - emit   : run that assertion, decide, and write `in_scope=true|false` to
//              $GITHUB_OUTPUT.
//
// Env:
//   MODE          `emit` | `verify` (also argv[2]); default `verify`.
//   EVENT_NAME    github.event_name.
//   HEAD_REF      github.head_ref (empty off pull_request).
//   BASE_SHA      (emit, PR) github.event.pull_request.base.sha.
//   HEAD_SHA      (emit, PR) github.event.pull_request.head.sha.
//   GITHUB_OUTPUT (emit) runner file `in_scope` is appended to.
//
// Exit: 0 clean; 1 on a stale path or a bad MODE. A decision this module cannot
// make resolves to `true` (boot the stack), never to `false`.
//
// node: builtins only — no npm deps, no setup-node needed.

import { statSync } from 'node:fs';

import { changedPaths, matchesAny, runsFullLane } from './lib/scope-gate.mjs';
import { error, log, notice, setOutput } from './lib/gh.mjs';

// The stack's own definition and the harness that drives it. A change here can
// break the lane in ways no application diff predicts, so it always boots.
export const HARNESS_PATHS = [
  'docker-compose.yml',
  'Dockerfile',
  'Dockerfile.local',
  'test/e2e/playwright',
  'test/e2e/seed',
  'test/e2e/otel-collector',
  'test/e2e/grafana/compose',
  '.github/workflows/e2e.yml',
  '.github/scripts/compose-smoke-matrix.mjs',
  '.github/scripts/compose-smoke-scope.mjs',
  '.github/scripts/lib/shard-coverage.mjs',
  '.github/scripts/lib/scope-gate.mjs',
];

// The application surface a real ClickHouse can see differently from chDB.
//
// Deliberately NOT all of `internal/**`. Widening it there would re-gate the
// lane on essentially every PR and undo the wall-time win the de-gating bought,
// for coverage the chdb-backed layers already provide: a lowering or optimizer
// change shows up in the TXTAR goldens, the property lane, and the
// compatibility harnesses long before it reaches a socket.
//
// What those layers CANNOT see is the boundary between a plan and a live
// server:
//
//   internal/chsql    — the emitted column types. This is the strictness axis:
//                       chDB accepts a UInt8 where the response decoder wants a
//                       *float64 and quietly coerces it; the production server
//                       does not, and the query 502s. No chdb-backed layer can
//                       observe the difference.
//   internal/api      — response shape and handler wiring, over a real socket
//                       with a real Grafana on the other end.
//   internal/chclient — the driver conversation itself: connection lifecycle,
//                       cancellation, teardown.
//   cmd/cerberus      — the binary's own startup and configuration path, which
//                       only this lane boots for real.
export const SCOPE_PATHS = ['internal/chsql', 'internal/api', 'internal/chclient', 'cmd/cerberus'];

// stalePaths — declared paths that no longer exist in the tree.
//
// Checked because the two lists are pure strings: a package rename turns an
// entry into one that matches nothing, and the lane silently stops gating on
// the very surface the entry was added to guard. Nothing else would notice,
// because "no PR was in scope" and "the scope is broken" produce the same
// green.
export function stalePaths(paths) {
  return paths.filter((p) => {
    try {
      statSync(p);

      return false;
    } catch {
      return true;
    }
  });
}

// decide — should this event boot the compose stack, and why?
//
// The `null` changed-set (no merge base, shallow clone, a git failure) resolves
// to true. An uncomputable diff is not evidence that nothing changed, and the
// cost of guessing wrong in that direction is a release-blocking bug that
// reached main unexamined.
export function decide({ eventName, headRef, changed }) {
  if (runsFullLane({ eventName, headRef })) {
    return { inScope: true, reason: `event "${eventName}" always runs the full lane` };
  }
  if (changed === null) {
    return { inScope: true, reason: 'the changed-path set could not be computed' };
  }

  const paths = [...changed];
  const harnessHit = paths.filter((p) => matchesAny(p, HARNESS_PATHS));
  if (harnessHit.length > 0) {
    return { inScope: true, reason: `the lane's own harness changed (${harnessHit.join(', ')})` };
  }

  const scopeHit = paths.filter((p) => matchesAny(p, SCOPE_PATHS));
  if (scopeHit.length > 0) {
    return { inScope: true, reason: `a stack-visible path changed (${scopeHit.join(', ')})` };
  }

  return { inScope: false, reason: 'no changed path is visible to the compose stack' };
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`compose-smoke-scope: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  const declared = [...HARNESS_PATHS, ...SCOPE_PATHS];
  const stale = stalePaths(declared);
  for (const p of stale) {
    error(
      `compose-smoke-scope: "${p}" is declared in scope but does not exist. It matches no diff, so the ` +
        `surface it was added to guard is no longer gated — and the lane would report that as a clean ` +
        `short-circuit. Re-point it at the path it moved to, or drop it.`,
    );
  }
  if (stale.length > 0) process.exit(1);
  log(`compose-smoke-scope: ${declared.length} declared paths, all present.`);

  if (mode === 'verify') return;

  const eventName = (process.env.EVENT_NAME || '').trim();
  const headRef = (process.env.HEAD_REF || '').trim();
  const changed = runsFullLane({ eventName, headRef })
    ? null
    : changedPaths({ baseSha: process.env.BASE_SHA, headSha: process.env.HEAD_SHA });

  const { inScope, reason } = decide({ eventName, headRef, changed });
  notice(`compose-smoke-scope: ${inScope ? 'booting the stack' : 'short-circuiting'} — ${reason}`);
  setOutput('in_scope', String(inScope));
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
