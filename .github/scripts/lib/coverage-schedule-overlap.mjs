// coverage-schedule-overlap.mjs — the one GitHub-API check that keeps
// coverage.yml's nightly schedule run from ever holding cerberus-heavy at
// the same time as a main-push run (tsouza/cerberus#3708).
//
// The problem this closes: coverage.yml's push-to-main and equivalent-cron
// runs deliberately sit in SEPARATE concurrency groups
// (`latest-main-push` vs `latest-main-schedule-<cron>`,
// main-coalescing.mjs's GROUP_EXPRESSION) — a routine push must never cancel
// the nightly's deeper evidence, and vice versa. That is correct for
// mutual cancellation, but it also means the two runs' heavy lanes can be
// IN FLIGHT together. A single heavy run already holds up to 2 of the 3
// `cerberus-heavy` runners (coverage.yml's own header comment); two
// overlapping heavy runs can together want 4, which starves the pool's
// other tenants — `lint` / `check-test` / `check-build` in ci.yml.
//
// The fix: before the nightly schedule run commits to its own heavy lanes,
// it asks the GitHub Actions API whether a push-triggered coverage.yml run
// against `main` is already QUEUED or IN_PROGRESS. If one is, the push run
// is already about to measure (or is measuring) a tip at least as recent as
// what the schedule would measure, so the schedule skips its heavy lanes for
// this run — coverage-run-heavy.mjs's package-enrollment gate still runs,
// and the NEXT nightly tick gets another chance. A push run's own RUN_HEAVY
// decision never depends on the schedule (coverage-run-heavy.mjs's
// push-only source-PR logic is untouched), so this is one-directional by
// design: only the schedule ever yields.
//
// Fails safe like every other run-heavy input in this codebase: a network
// error or an unreadable response resolves to "no overlap detected", i.e.
// the schedule still runs heavy. Silently skipping the ONE nightly safety
// net over an API hiccup would be worse than the rare double-occupancy this
// module exists to avoid.

import { DEFAULT_API_BASE, ghJSON, NOT_FOUND_THROW, withPage } from './gh-api.mjs';

export const COVERAGE_WORKFLOW_FILE = 'coverage.yml';

// Statuses that mean "this run either wants a cerberus-heavy runner right
// now or is about to". `completed` (any conclusion) never counts — it has
// already released whatever runners it held.
const OVERLAPPING_STATUSES = new Set(['queued', 'in_progress', 'requested', 'waiting']);

/**
 * Pure: does `runs` (the raw `workflow_runs` array from the "list workflow
 * runs" endpoint) contain a push-to-main run that is still holding or about
 * to hold runners?
 */
export function findOverlappingPushRun(runs) {
  const candidate = (runs ?? []).find(
    (run) => run && OVERLAPPING_STATUSES.has(String(run.status ?? '')),
  );
  return candidate
    ? { runId: candidate.id, status: candidate.status, headSha: candidate.head_sha ?? null }
    : null;
}

/**
 * Network wrapper. Returns the same shape as findOverlappingPushRun, or
 * `null` on "no overlap" (including any failure — fail-safe, see header).
 */
export async function checkOverlappingMainPushRun({
  repo,
  token,
  apiBase = DEFAULT_API_BASE,
  fetchImpl = globalThis.fetch,
}) {
  if (!repo || !token) return null;
  try {
    const url = withPage(
      `${apiBase}/repos/${repo}/actions/workflows/${COVERAGE_WORKFLOW_FILE}/runs?event=push&branch=main`,
      1,
      // A handful of recent runs is always enough: only a run still queued
      // or in progress can overlap, and GitHub returns newest-first.
      5,
    );
    const body = await ghJSON(url, {
      token,
      what: `GET actions/workflows/${COVERAGE_WORKFLOW_FILE}/runs`,
      notFound: NOT_FOUND_THROW,
      fetchImpl,
    });
    return findOverlappingPushRun(body?.workflow_runs);
  } catch {
    return null;
  }
}
