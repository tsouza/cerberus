/**
 * SWEEP_DEPTH plumbing.
 *
 * The dashboard sweeps run in two depths:
 *
 *   - 'lean' (default) — the per-PR gate. Probes every consumption
 *     surface at the API layer; browser renders are restricted to
 *     the ops-family dashboards.
 *   - 'full' — the nightly lane. Everything 'lean' does PLUS the
 *     browser render of every dashboard (including the
 *     showcase-prefixed family as it lands in P2+).
 *
 * THE INVARIANT: depth never changes which RULES run — only how many
 * STATES they run against. A check that exists at 'lean' asserts the
 * exact same contract at 'full'; 'full' just visits more (dashboard
 * × render-path × range) states. Anything else would make the PR
 * gate and the nightly lane diverge on semantics rather than
 * coverage, and a nightly-only failure would stop being "more
 * surface" and start being "different rules".
 *
 * CI wiring: .github/workflows/e2e.yml sets SWEEP_DEPTH=full on the
 * nightly schedule and leaves the default ('lean') for pull_request
 * + push lanes.
 */

export type SweepDepth = 'lean' | 'full';

/**
 * Resolve the active sweep depth from the SWEEP_DEPTH env var.
 * Default 'lean'; anything other than 'lean' / 'full' throws — a
 * typo'd depth must fail the run, not silently fall back.
 */
export function sweepDepth(): SweepDepth {
  const raw = process.env.SWEEP_DEPTH ?? 'lean';
  if (raw === 'lean' || raw === 'full') return raw;
  throw new Error(
    `sweepDepth: SWEEP_DEPTH must be 'lean' or 'full', got ${JSON.stringify(raw)}`,
  );
}

/**
 * One-line description of the active depth for the CI run record.
 * Specs that branch on depth log this once (the document pattern) so
 * a run's coverage shape is visible in the job output without
 * reverse-engineering it from the test counts.
 */
export function describeSweepDepth(depth: SweepDepth = sweepDepth()): string {
  return depth === 'full'
    ? 'sweep depth: full (nightly lane — browser-renders every dashboard, including the showcase family)'
    : 'sweep depth: lean (PR gate — API-layer probes everywhere; browser renders for ops-family dashboards only)';
}
