// merge-risk.test.mjs — node:test guard for #2899's merge-risk gate.
//
// The gate has one BLOCKING half (the stale-base golden collision) and one
// REPORTING half (the lanes that gate `main` and do not run on this PR), and
// each needs both directions pinned. A stale-base check that only ever finds a
// collision would block every PR; one that can never find a collision is the
// invisible-skip posture this exists to end, wearing a green tick. Both shapes
// are asserted below, and so is the ONE case the blocking half deliberately
// stays quiet on — two PRs adding independent fixtures with no generator-code
// change between them, where the merged tree really is the union.

import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import {
  DEFAULT_MERGE_TARGET_REF,
  DEFAULT_REGISTRY_PATH,
  changedFiles,
  collisionReport,
  gatingLanesThatSkipPRs,
  goldenRootsOf,
  revExists,
  shardsWritten,
  staleBaseCollisions,
  underGoldenRoot,
  unvalidatedLanes,
} from './merge-risk.mjs';

const REGISTRY = JSON.parse(readFileSync(resolve(DEFAULT_REGISTRY_PATH), 'utf8'));

// The two sides of the real #2895 / post-merge-drift races, as file sets.
const CARDINALITY_A = ['internal/chsql/emit.go', 'test/perf/cardinality-baseline/promql/rate_5m.json'];
const CARDINALITY_B = ['internal/promql/lower.go', 'test/perf/cardinality-baseline/promql/sum_by.json'];

// --- golden-root resolution ---------------------------------------------------

test('goldenRootsOf reads the shard table rather than restating it', () => {
  const roots = goldenRootsOf();
  assert.ok(Object.keys(roots).length > 0, 'an empty shard table makes the stale-base half vacuous');
  assert.ok(roots.cardinality.includes('test/perf/cardinality-baseline'));
  assert.ok(roots.promql.includes('test/spec/promql'));
});

test('underGoldenRoot matches inside a root and the root itself, not a sibling', () => {
  assert.equal(underGoldenRoot('test/perf/cardinality-baseline/promql/a.json', 'test/perf/cardinality-baseline'), true);
  assert.equal(underGoldenRoot('test/perf/cardinality-baseline', 'test/perf/cardinality-baseline'), true);
  assert.equal(underGoldenRoot('test/perf/cardinality-baseline-notes.md', 'test/perf/cardinality-baseline'), false);
});

test('underGoldenRoot honours a globbed golden root', () => {
  assert.equal(
    underGoldenRoot('test/e2e/migration/archetypes/kube/expected/plan.sql', 'test/e2e/migration/archetypes/*/expected'),
    true,
  );
});

test('shardsWritten attributes a written artefact to its shard', () => {
  const hit = shardsWritten(CARDINALITY_A);
  assert.deepEqual([...hit.keys()], ['cardinality']);
  assert.deepEqual(hit.get('cardinality'), ['test/perf/cardinality-baseline/promql/rate_5m.json']);
});

test('shardsWritten ignores a change that writes no golden at all', () => {
  assert.equal(shardsWritten(['internal/chsql/emit.go', 'docs/engine.md']).size, 0);
});

// --- the blocking half: FIRES -------------------------------------------------

test('FIRES: two code changes regenerating the same shard against different bases collide', () => {
  const c = staleBaseCollisions({ ours: CARDINALITY_A, theirs: CARDINALITY_B });
  assert.equal(c.length, 1);
  assert.equal(c[0].shard, 'cardinality');
  assert.deepEqual(c[0].ours, ['test/perf/cardinality-baseline/promql/rate_5m.json']);
  assert.deepEqual(c[0].theirs, ['test/perf/cardinality-baseline/promql/sum_by.json']);
});

test('FIRES even when only ONE side moved generator code — the other golden is stale under it', () => {
  const c = staleBaseCollisions({
    ours: ['test/spec/promql/new_case.txtar', 'test/perf/cardinality-baseline/promql/new_case.json'],
    theirs: ['internal/chsql/emit.go', 'test/perf/cardinality-baseline/promql/rate_5m.json'],
  });
  assert.equal(c.length, 1);
  assert.equal(c[0].shard, 'cardinality');
});

test('FIRES on every colliding shard, not just the first', () => {
  const c = staleBaseCollisions({
    ours: ['internal/chsql/emit.go', 'test/spec/promql/a.txtar', 'test/perf/solver-decision-baseline/a.json'],
    theirs: ['test/spec/promql/b.txtar', 'test/perf/solver-decision-baseline/b.json'],
  });
  assert.deepEqual(c.map((x) => x.shard), ['promql', 'solver']);
});

test('the collision report names the shard, both sides, and the exact remedy command', () => {
  const [c] = staleBaseCollisions({ ours: CARDINALITY_A, theirs: CARDINALITY_B });
  const text = collisionReport(c, DEFAULT_MERGE_TARGET_REF);
  assert.match(text, /cardinality/);
  assert.match(text, /rate_5m\.json/);
  assert.match(text, /sum_by\.json/);
  assert.match(text, /just update-golden cardinality/);
});

// --- the blocking half: DOES NOT FIRE -----------------------------------------

test('DOES NOT FIRE: two independent fixture additions with no generator change', () => {
  // The union really is correct here: a golden is a function of its corpus and
  // its generator code, and neither side moved the code.
  assert.deepEqual(
    staleBaseCollisions({
      ours: ['test/spec/promql/a.txtar', 'test/perf/cardinality-baseline/promql/a.json'],
      theirs: ['test/spec/promql/b.txtar', 'test/perf/cardinality-baseline/promql/b.json'],
    }),
    [],
  );
});

test('DOES NOT FIRE: the target moved code but regenerated no shard this change writes', () => {
  assert.deepEqual(
    staleBaseCollisions({ ours: CARDINALITY_A, theirs: ['internal/api/loki/handler.go', 'docs/engine.md'] }),
    [],
  );
});

test('DOES NOT FIRE: different shards on each side', () => {
  assert.deepEqual(
    staleBaseCollisions({
      ours: ['internal/chsql/emit.go', 'test/perf/cardinality-baseline/promql/a.json'],
      theirs: ['internal/promql/lower.go', 'test/surface-parity/promql.json'],
    }),
    [],
  );
});

test('DOES NOT FIRE on an empty or missing file set', () => {
  assert.deepEqual(staleBaseCollisions({ ours: [], theirs: [] }), []);
  assert.deepEqual(staleBaseCollisions({}), []);
});

// --- the reporting half --------------------------------------------------------

test('the gating-but-skipping lane set is non-empty and every member really skips PRs', () => {
  const lanes = gatingLanesThatSkipPRs(REGISTRY);
  assert.ok(lanes.length > 0, 'an empty set would make the disclosure permanently silent');
  for (const l of lanes) {
    assert.equal(l.merge_posture, 'never', `${l.id} runs on PRs and is not an unvalidated-skip risk`);
    assert.ok(
      l.main_posture !== 'never' || l.release_posture === 'required',
      `${l.id} gates nothing after merge and must not be reported as a merge risk`,
    );
  }
});

test('a lane that gates nothing after merge is NOT reported as a risk', () => {
  const ids = gatingLanesThatSkipPRs(REGISTRY).map((l) => l.id);
  // `governance.release-gate-drift` never runs on a PR and never gates main or
  // a release; reporting it would be noise with no remedy.
  assert.ok(!ids.includes('governance.release-gate-drift'));
});

test('the real compatibility lanes ARE in the skipping set — that is the #2895 shape', () => {
  const ids = gatingLanesThatSkipPRs(REGISTRY).map((l) => l.id);
  for (const want of ['compatibility.loki', 'compatibility.prometheus', 'compatibility.tempo']) {
    assert.ok(ids.includes(want), `${want} gates main and does not run on PRs`);
  }
});

test('unvalidatedLanes names the LogQL reference lane for a LogQL change', () => {
  const found = unvalidatedLanes(REGISTRY, ['internal/logql/lsyntax/parser.go']).map((u) => u.lane.id);
  assert.ok(found.includes('compatibility.loki'));
});

test('unvalidatedLanes is silent on a change no gating-but-skipping lane declares', () => {
  assert.deepEqual(unvalidatedLanes(REGISTRY, ['.github/scripts/merge-risk.mjs']), []);
});

test('unvalidatedLanes is silent on an empty or missing file set', () => {
  assert.deepEqual(unvalidatedLanes(REGISTRY, []), []);
  assert.deepEqual(unvalidatedLanes(REGISTRY, undefined), []);
});

// --- git plumbing ---------------------------------------------------------------

test('revExists is false for a ref that names nothing, true for HEAD', () => {
  assert.equal(revExists('definitely-not-a-ref-in-this-repo'), false);
  assert.equal(revExists(''), false);
  assert.equal(revExists('HEAD'), true);
});

test('changedFiles reads a real range out of this checkout', () => {
  const files = changedFiles('HEAD~1...HEAD');
  assert.ok(Array.isArray(files));
});

test('changedFiles surfaces a bad range instead of returning an empty set', () => {
  assert.throws(() => changedFiles('not-a-ref...also-not-a-ref'), /git diff --name-only/);
});

// --- wiring pin ------------------------------------------------------------------

const forbidDeferralWorkflow = readFileSync(resolve('.github/workflows/forbid-deferral.yml'), 'utf8');

test('the merge-risk gate rides forbid-deferral.yml and invokes the script', () => {
  assert.match(forbidDeferralWorkflow, /run: node \.github\/scripts\/merge-risk\.mjs/);
  assert.match(forbidDeferralWorkflow, /run: node --test \.github\/scripts\/merge-risk\.test\.mjs/);
});
