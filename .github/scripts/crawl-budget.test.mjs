// crawl-budget.test.mjs — node:test guard for the crawl's two-budget ordering.
//
// Runs on the CHEAP check lane (`node --test .github/scripts/*.test.mjs`) — no
// setup-node, no deps, no stack — so the invariant that #1861 violated is
// caught on a PR rather than three nights later on a cancelled nightly.
//
// Guards three things:
//   1. every crawl job cap sits STRICTLY above the spec budget it must clear
//      (the real invariant: the spec times out first, with evidence);
//   2. the budgets in lib/crawl-budget.mjs still match the ones crawl.spec.ts
//      actually sets, so the two files cannot drift apart in silence;
//   3. the shard planners hand the crawl shard the depth-appropriate cap — the
//      nightly full-depth crawl must not be given the lean cap again.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import {
  CRAWL_SHARD_TIMEOUT_FULL_MIN,
  CRAWL_SHARD_TIMEOUT_LEAN_MIN,
  CRAWL_SPEC_BUDGET_FULL_MIN,
  CRAWL_SPEC_BUDGET_LEAN_MIN,
  crawlShardTimeoutMinutes,
  crawlSpecBudgetMinutes,
  parseSpecBudgetsFromSource,
} from './lib/crawl-budget.mjs';
import { shardTimeoutMinutes as composeShardTimeout } from './compose-smoke-matrix.mjs';
import { shardTimeoutMinutes as dashboardShardTimeout } from './dashboard-matrix.mjs';

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const crawlSpecPath = resolve(repoRoot, 'test/e2e/playwright/crawl/crawl.spec.ts');

test('every job cap clears the spec budget it has to outlive', () => {
  for (const depth of ['lean', 'full']) {
    assert.ok(
      crawlShardTimeoutMinutes(depth) > crawlSpecBudgetMinutes(depth),
      `depth=${depth}: job cap ${crawlShardTimeoutMinutes(depth)}min must exceed the ` +
        `${crawlSpecBudgetMinutes(depth)}min spec budget, or the runner kills the job first and ` +
        'the shard can only ever report `cancelled` (#1861)',
    );
  }
  // The full-depth cap is the one that regressed: it must clear the FULL
  // budget, not merely the lean one.
  assert.ok(
    CRAWL_SHARD_TIMEOUT_FULL_MIN > CRAWL_SPEC_BUDGET_FULL_MIN,
    'the full-depth cap must clear the full-depth spec budget',
  );
  assert.ok(
    CRAWL_SHARD_TIMEOUT_LEAN_MIN < CRAWL_SHARD_TIMEOUT_FULL_MIN,
    'the lean cap must stay tighter than the full one — a lean hang still has to fail fast',
  );
});

test('the pinned budgets match what crawl.spec.ts actually sets', () => {
  const budgets = parseSpecBudgetsFromSource(readFileSync(crawlSpecPath, 'utf8'));
  assert.notEqual(
    budgets,
    null,
    `could not find the depth-keyed testInfo.setTimeout(...) call in ${crawlSpecPath} — ` +
      'the drift guard cannot read the real budget, which is a failure, not a pass',
  );
  assert.equal(
    budgets.fullMin,
    CRAWL_SPEC_BUDGET_FULL_MIN,
    'CRAWL_SPEC_BUDGET_FULL_MIN drifted from crawl.spec.ts',
  );
  assert.equal(
    budgets.leanMin,
    CRAWL_SPEC_BUDGET_LEAN_MIN,
    'CRAWL_SPEC_BUDGET_LEAN_MIN drifted from crawl.spec.ts',
  );
});

test('the drift guard fires — a changed spec budget is detected, not shrugged off', () => {
  const tampered = readFileSync(crawlSpecPath, 'utf8').replace(
    `depth === 'full' ? ${CRAWL_SPEC_BUDGET_FULL_MIN} * 60_000`,
    `depth === 'full' ? ${CRAWL_SPEC_BUDGET_FULL_MIN + 1} * 60_000`,
  );
  const budgets = parseSpecBudgetsFromSource(tampered);
  assert.notEqual(budgets, null, 'the tampered source must still parse');
  assert.notEqual(
    budgets.fullMin,
    CRAWL_SPEC_BUDGET_FULL_MIN,
    'the parser must report the tampered budget, otherwise the drift guard is a no-op',
  );
});

test('both shard planners give the crawl shard the depth-appropriate cap', () => {
  // compose-smoke: nightly schedule sweeps full, PR/push sweep lean.
  assert.equal(
    composeShardTimeout('shard-crawl', { isSchedule: true }),
    CRAWL_SHARD_TIMEOUT_FULL_MIN,
    'the nightly compose crawl shard must get the full-depth cap',
  );
  assert.equal(
    composeShardTimeout('shard-crawl', { isSchedule: false }),
    CRAWL_SHARD_TIMEOUT_LEAN_MIN,
    'the PR/push compose crawl shard must get the lean cap',
  );
  // dashboard (k3d): the crawl shard always sweeps full.
  assert.equal(
    dashboardShardTimeout({ crawlStack: 'k3d' }),
    CRAWL_SHARD_TIMEOUT_FULL_MIN,
    'the k3d crawl shard always sweeps full, so it must always get the full-depth cap',
  );
});
