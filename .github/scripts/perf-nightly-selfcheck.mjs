// perf-nightly-selfcheck.mjs — the periodic mutation-style self-check that
// #2370's nightly perf gate (test/perf/nightly/, `just
// perf-nightly-integration`) still catches a real regression, closing
// tsouza/cerberus#2437 (the one mitigation #2370's own "Bit-rot risk"
// paragraph named but did not build in PR A/B/C).
//
// Why this exists
// ----------------
// The nightly gate's two-pronged assertion (a committed per-sentinel memory
// ceiling, an absolute cap-relative ceiling, and an ExpectedStatus/
// ExpectedErrorSubstring check) can only ever be OBSERVED to work when a real
// regression happens to trip it. Nothing proves it stays load-bearing
// between real incidents: a change that silently widens a ceiling, breaks
// the status check, or otherwise defangs the ratchet would currently only be
// caught by a human reading a diff — exactly the failure mode #2364 itself
// was. This script proves the gate is still armed by deliberately breaking
// TWO real memory-bounding mechanisms, one at a time, and asserting the
// nightly gate ACTUALLY FAILS against each broken build. A gate that stays
// green through an injected regression is worse than no gate — it reads as
// coverage that is not there.
//
// The two mutations
// ------------------
// Each targets a DIFFERENT class of failure the nightly gate's own sentinels
// are calibrated to catch (test/perf/nightly/sentinels.go), verified
// empirically against a real testcontainers ClickHouse + the real #2411
// production sample before this script was written — see MUTATIONS below
// for the measured effect of each:
//
//   1. rate-window-fanout-bound-widened — internal/chsql/rate_window_fanout_
//      bound.go's maxRateWindowFanoutRows, 1000x. Defeats the #2429 resource
//      bound `request_rate_by_method` and `error_ratio_by_namespace` are
//      calibrated to be REJECTED by (ExpectedStatus 422); with the bound
//      defeated, ClickHouse instead runs the query for real and hits a raw
//      MEMORY_LIMIT_EXCEEDED (code 241) — a genuine OOM the gate's status
//      check still catches, just via a different failure mode than the one
//      it names.
//
//   2. spill-threshold-disabled — internal/engine/spill.go's spillThreshold,
//      inflated so the external-group-by/sort spill safety net never
//      engages. `pod_status_reason_gauge` (calibrated at ~77% of the memory
//      cap, #2435's own finding) OOMs for real once spilling stops covering
//      for it, flipping its expected 200 to a 422 the gate's status check
//      catches.
//
// Both were chosen because they are ORTHOGONAL memory-bounding mechanisms
// (a resource-bound throwIf vs. a disk-spill fallback) exercising different
// sentinels — proving one still works says nothing about the other. Per
// #2437's own scope, this is deliberately NOT exhaustive: one representative
// injected regression per mechanism is the goal, not covering every
// possible defeat of every sentinel.
//
// How it runs
// -----------
// For each mutation: assert the working tree is clean, apply the mutation
// (a plain, exact string replacement — if the target text has drifted since
// this script was written, it fails LOUDLY rather than silently mutating
// nothing), run `just perf-nightly-integration`, revert the file
// UNCONDITIONALLY (even on a script crash) via `git checkout --`, and record
// whether the gate failed as expected. This never commits or pushes
// anything — the mutation lives only in the ephemeral CI checkout for the
// span of one `just` invocation.
//
// Exit: 0 when every mutation was caught (the gate failed under each one);
// 1 when the working tree was not clean at the start, a mutation's target
// text was not found, or ANY mutation was NOT caught (the gate stayed green
// despite the injected regression — the actual bit-rot signal this script
// exists to surface).
//
// node: builtins only (via lib/gh.mjs's capture()/git()).

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, git, log, notice } from './lib/gh.mjs';

// PERF_NIGHTLY_TIMEOUT_MS bounds one `just perf-nightly-integration` run.
// Each real run measured under two minutes locally (see the module header);
// this leaves wide headroom for CI-runner variance across two sequential
// invocations without risking the workflow's own job timeout swallowing a
// genuine hang as a silent non-catch.
const PERF_NIGHTLY_TIMEOUT_MS = 10 * 60 * 1000;

export const MUTATIONS = [
  {
    id: 'rate-window-fanout-bound-widened',
    description:
      'Widen internal/chsql/rate_window_fanout_bound.go\'s maxRateWindowFanoutRows 1000x, defeating the ' +
      '#2429 resource bound request_rate_by_method/error_ratio_by_namespace rely on to be rejected (422).',
    file: 'internal/chsql/rate_window_fanout_bound.go',
    find: 'maxRateWindowFanoutRows = 2_800_000\n',
    replace: 'maxRateWindowFanoutRows = 2_800_000_000\n',
  },
  {
    id: 'spill-threshold-disabled',
    description:
      'Inflate internal/engine/spill.go\'s spillThreshold so the external-group-by/sort spill safety net ' +
      'never engages, letting pod_status_reason_gauge (~77% of the memory cap) OOM for real instead of spilling.',
    file: 'internal/engine/spill.go',
    find: 'return maxMemory / spillCapDenominator\n',
    replace: 'return maxMemory * 1000\n',
  },
];

// assertCleanTree — refuses to mutate on top of unknown working-tree state.
function assertCleanTree(label) {
  const status = git(['status', '--porcelain']);
  if (status.status !== 0) {
    throw new Error(`perf-nightly-selfcheck: \`git status\` failed (${label}): ${status.stderr.trim()}`);
  }
  if (status.stdout.trim() !== '') {
    throw new Error(
      `perf-nightly-selfcheck: working tree is not clean (${label}) — refusing to mutate on top of it:\n` +
        status.stdout,
    );
  }
}

// applyMutation — exact-string-replace `mutation.file`. Throws if the target
// text is not found EXACTLY once, so a source drift fails loudly instead of
// silently mutating nothing (a no-op mutation would make every subsequent
// "caught" verdict meaningless). Exported for the drift-detection unit
// tests, which drive it against a temp file rather than the real source
// tree.
export function applyMutation(mutation) {
  const original = readFileSync(mutation.file, 'utf8');
  const occurrences = original.split(mutation.find).length - 1;
  if (occurrences !== 1) {
    throw new Error(
      `perf-nightly-selfcheck: mutation "${mutation.id}"'s target text was found ${occurrences} time(s) in ` +
        `${mutation.file} (expected exactly 1) — the source has drifted since this mutation was written; ` +
        'update MUTATIONS in perf-nightly-selfcheck.mjs.',
    );
  }
  writeFileSync(mutation.file, original.replace(mutation.find, mutation.replace), 'utf8');
}

// revertMutation — unconditional `git checkout -- <file>`, called from a
// finally block so a crash mid-mutation never leaves the tree dirty.
function revertMutation(mutation) {
  const res = git(['checkout', '--', mutation.file]);
  if (res.status !== 0) {
    throw new Error(
      `perf-nightly-selfcheck: failed to revert ${mutation.file} after mutation "${mutation.id}": ` +
        res.stderr.trim(),
    );
  }
}

// runNightlyGate — `just perf-nightly-integration`. Returns { caught,
// output } where `caught` is true iff the recipe exited non-zero (the gate
// failed, as it must under an injected regression).
function runNightlyGate() {
  const res = capture('just', ['perf-nightly-integration'], { timeout: PERF_NIGHTLY_TIMEOUT_MS });
  return { caught: res.status !== 0, output: `${res.stdout}\n${res.stderr}` };
}

// runOneMutation — apply, run, revert (always), report. Never leaves the
// tree mutated even if `just` itself throws an unexpected error.
export function runOneMutation(mutation) {
  applyMutation(mutation);
  try {
    const { caught, output } = runNightlyGate();
    return { id: mutation.id, description: mutation.description, caught, output };
  } finally {
    revertMutation(mutation);
  }
}

async function main() {
  assertCleanTree('before any mutation');

  const results = [];
  for (const mutation of MUTATIONS) {
    notice(`perf-nightly-selfcheck: applying mutation "${mutation.id}" — ${mutation.description}`);
    const result = runOneMutation(mutation);
    assertCleanTree(`after reverting mutation "${mutation.id}"`);
    results.push(result);
    if (result.caught) {
      log(`perf-nightly-selfcheck: "${mutation.id}" was caught — the gate failed as expected.`);
    } else {
      error(
        `perf-nightly-selfcheck: "${mutation.id}" was NOT caught — the nightly gate stayed green despite ` +
          `this injected regression. ${mutation.description}`,
        { title: 'perf-nightly self-check: bit-rot detected' },
      );
      log(`--- last just perf-nightly-integration output for "${mutation.id}" ---\n${result.output}`);
    }
  }

  const uncaught = results.filter((r) => !r.caught);
  if (uncaught.length > 0) {
    error(
      `perf-nightly-selfcheck: ${uncaught.length}/${results.length} mutation(s) were not caught: ` +
        `${uncaught.map((r) => r.id).join(', ')}. The nightly perf gate has bit-rotted for at least one ` +
        'memory-bounding mechanism class — see tsouza/cerberus#2437.',
    );
    process.exit(1);
  }

  notice(`perf-nightly-selfcheck: all ${results.length} mutation(s) were caught — the nightly gate is still armed.`);
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-nightly-selfcheck failed: ${e.message}`);
    process.exit(1);
  });
}
