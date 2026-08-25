// lib/run-heavy.mjs — the shared RUN_HEAVY decision scaffold behind
// chdb-run-heavy.mjs, coverage-run-heavy.mjs, property-run-heavy.mjs and
// perf-profile-run-heavy.mjs.
//
// Those four lane wrappers differ only in which check-run(s)
// release-preflight.mjs's SOURCE-PR CREDIT (tsouza/cerberus#2394) ends up
// crediting, and the noun phrases naming them in the decision's `reason`
// text. Everything else — decide()'s branching, resolvePushSourcePR()'s
// fail-safe network wrapper, and main()'s verify/emit dispatch — was
// ~95% byte-identical duplication across all four (and had already started
// to drift: one test file was missing an explanatory comment its sibling
// had). createRunHeavyDecider() is the one place that scaffold lives now;
// each wrapper supplies only its own scriptName and the four phrase
// fragments its own header comment still documents, and re-exports
// `decide` (what its `.test.mjs` imports) plus calls `runCLI`.
//
// See coverage-run-heavy.mjs's header for the full rationale this
// implements: the fail-safe default (uncertainty always resolves to "run it
// for real", never "skip it"), the SOURCE-PR CREDIT cross-reference, and why
// pull_request / merge_group / schedule / workflow_dispatch behave exactly
// as the pre-existing inline GHA expression did.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { runsFullLane } from './scope-gate.mjs';
import { resolveSourcePR } from './resolve-source-pr.mjs';
import { error, log, notice, setOutput } from './gh.mjs';

/**
 * Builds one lane's { decide, resolvePushSourcePR, main, runCLI }, closed
 * over that lane's own message phrasing.
 *
 * @param {string} scriptName     e.g. "chdb-run-heavy" — every log/notice/
 *                                 error line is prefixed with this.
 * @param {string} heavyLanesPhrase  fills `... runs ${phrase}` when a
 *                                 non-push event runs heavy (e.g.
 *                                 "the heavy chdb lanes").
 * @param {string} ordinaryPhrase fills `ordinary ${ordinaryEventLabel} —
 *                                 ${phrase}` when a non-push event does not
 *                                 (e.g. "green no-op, same as before").
 * @param {string} [ordinaryEventLabel] the event name(s) named alongside
 *                                 ordinaryPhrase — default "pull_request/
 *                                 merge_group"; pass "pull_request" for a
 *                                 lane whose workflow carries no
 *                                 merge_group: trigger at all.
 * @param {string} redundantPhrase  fills `... which already ran ${phrase} —
 *                                 release-preflight.mjs's SOURCE-PR CREDIT
 *                                 ...` when a push is credited to a
 *                                 release/*-headed source PR.
 * @param {string} firstRealPhrase  fills `this push is the ${phrase}` when a
 *                                 push is the first real run for its tree.
 */
export function createRunHeavyDecider({
  scriptName,
  heavyLanesPhrase,
  ordinaryPhrase,
  ordinaryEventLabel = 'pull_request/merge_group',
  redundantPhrase,
  firstRealPhrase,
}) {
  // decide — pure. `sourcePR` is only meaningful (and only ever passed) for
  // a `push` event: `{ number, headRef }` for an exact-match resolved PR, or
  // `null` when none resolved (no PR, ambiguous match, or a resolution
  // failure the caller already logged). Ignored for every other event.
  function decide({ eventName, headRef, sourcePR = null }) {
    const event = String(eventName ?? '').trim();

    if (event !== 'push') {
      const runHeavy = runsFullLane({ eventName: event, headRef });
      return {
        runHeavy,
        reason: runHeavy
          ? `event "${event || '<unknown>'}" runs ${heavyLanesPhrase}`
          : `ordinary ${ordinaryEventLabel} — ${ordinaryPhrase}`,
      };
    }

    if (sourcePR && typeof sourcePR.headRef === 'string' && sourcePR.headRef.startsWith('release/')) {
      return {
        runHeavy: false,
        reason:
          `redundant with PR #${sourcePR.number} (${sourcePR.headRef}), which already ran ${redundantPhrase} — ` +
          "release-preflight.mjs's SOURCE-PR CREDIT (tsouza/cerberus#2394) reads that run instead of demanding " +
          'a fresh one here',
      };
    }

    return {
      runHeavy: true,
      reason: sourcePR
        ? `source PR #${sourcePR.number} (${sourcePR.headRef ?? '<no head ref>'}) was not release/*-headed — ` +
          `this push is the ${firstRealPhrase}`
        : 'no source PR resolved for this push (ordinary-PR merge, unresolved match, or a maintenance-line ' +
          'hotfix with no PR at all) — running heavy for real (fail-safe default)',
    };
  }

  async function resolvePushSourcePR({ repo, sha, token, apiBase }) {
    if (!repo || !sha || !token) {
      notice(
        `${scriptName}: GITHUB_REPOSITORY/GITHUB_SHA/GITHUB_TOKEN not all set for a push event — ` +
          'cannot resolve a source PR, running heavy for real (fail-safe default).',
      );
      return null;
    }
    try {
      return await resolveSourcePR({ repo, sha, token, apiBase });
    } catch (e) {
      notice(
        `${scriptName}: could not resolve a source PR for ${String(sha).slice(0, 8)} (${e.message}) — ` +
          'running heavy for real (fail-safe default).',
      );
      return null;
    }
  }

  async function main() {
    const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
    if (mode !== 'verify' && mode !== 'emit') {
      error(`${scriptName}: MODE must be "verify" or "emit" (got "${mode}")`);
      process.exit(1);
    }

    if (mode === 'verify') {
      log(`${scriptName}: RUN_HEAVY decision policy loaded.`);
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
    notice(`${scriptName}: run_heavy=${verdict.runHeavy} — ${verdict.reason}`);
    setOutput('run_heavy', String(verdict.runHeavy));
  }

  // runCLI — the invoked-as-a-program boilerplate every wrapper used to
  // carry itself. Takes the WRAPPER's own `import.meta.url` (not this
  // module's) since that is what has to match `process.argv[1]` for "was I
  // run directly" to mean anything.
  function runCLI(moduleUrl) {
    const invokedDirectly = process.argv[1] && moduleUrl === pathToFileURL(process.argv[1]).href;
    if (!invokedDirectly) return;
    main().catch((e) => {
      error(`${scriptName} failed: ${e.message}`);
      process.exit(1);
    });
  }

  return { decide, resolvePushSourcePR, main, runCLI };
}
