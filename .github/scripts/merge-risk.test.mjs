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
//
// The reporting half's ATTRIBUTION carries its own acceptance case (#2902),
// read against this repository's real import graph rather than a fixture: PR
// #2824's real changed-file set — the change that caused the #2895 outage — is
// replayed through the derivation and must name `compatibility.loki`, and the
// same file set replayed through the raw `package_globs` must NOT, because that
// contrast is the whole evidence that the derivation is what closed the hole.

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
import { declaredGlobs, laneAffectedGlobs } from './lib/lane-closure.mjs';

const REGISTRY = JSON.parse(readFileSync(resolve(DEFAULT_REGISTRY_PATH), 'utf8'));

// The three reference harnesses plus their variants: the lanes #2895 was red on
// for 31 runs, and the only lanes whose attribution these tests interrogate.
// Restricting the registry keeps the real `go list` load to the ONE untagged
// import graph they share — the production gate loads one per build-tag set.
const COMPAT_REGISTRY = { lanes: REGISTRY.lanes.filter((l) => l.id.startsWith('compatibility.')) };
const COMPAT_LANES = gatingLanesThatSkipPRs(COMPAT_REGISTRY);

/** Derived attribution: the declared globs plus their real dependency closure. */
const DERIVED = laneAffectedGlobs(COMPAT_LANES, { repoRoot: process.cwd() });
/** The pre-#2902 attribution, kept only so the contrast below can be asserted. */
const DECLARED = declaredGlobs(COMPAT_LANES);

const laneIDs = (registry, files, globs) => unvalidatedLanes(registry, files, globs).map((u) => u.lane.id);

// PR #2824 `0d32cc96d` ("gate the ClickHouse query result cache on closed
// windows"), verbatim from `gh pr view 2824 --json files`. It stamped
// `use_query_cache=1` without `query_cache_nondeterministic_function_handling`,
// whose ClickHouse default fails any query containing `arrayJoin` — which is
// how every non-native range lowering fans samples across the step grid. 144
// LogQL and 871 PromQL cases diverged and `compatibility` was red on `main` for
// 31 consecutive runs (#2895).
const PR_2824_FILES = [
  'cmd/cerberus/bootstrap_config_test.go',
  'cmd/cerberus/chopt_reprobe.go',
  'cmd/cerberus/main.go',
  'docs/clickhouse-optimizations.md',
  'docs/configuration.md',
  'internal/api/info/info.go',
  'internal/api/info/info_test.go',
  'internal/chclient/client.go',
  'internal/chclient/result_cache.go',
  'internal/chclient/result_cache_hit_integration_test.go',
  'internal/chclient/result_cache_metrics.go',
  'internal/chclient/result_cache_probe.go',
  'internal/chclient/result_cache_probe_integration_test.go',
  'internal/chclient/result_cache_probe_test.go',
  'internal/chclient/ts_grid_probe.go',
  'internal/chopt/capability.go',
  'internal/chopt/registry.go',
  'internal/chopt/resolve.go',
  'internal/chopt/resolve_test.go',
  'internal/config/config.go',
  'internal/config/envdocs.go',
  'internal/engine/query_settings_rules.go',
  'internal/engine/result_cache_test.go',
  'test/perf/solver_decision_ratchet_test.go',
];

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
  assert.ok(laneIDs(COMPAT_REGISTRY, ['internal/logql/lsyntax/parser.go'], DERIVED).includes('compatibility.loki'));
});

test('unvalidatedLanes is silent on a change that reaches no lane at all', () => {
  assert.deepEqual(laneIDs(COMPAT_REGISTRY, ['.github/scripts/merge-risk.mjs'], DERIVED), []);
  assert.deepEqual(laneIDs(COMPAT_REGISTRY, ['docs/engine.md'], DERIVED), []);
});

test('unvalidatedLanes is silent on an empty or missing file set', () => {
  assert.deepEqual(unvalidatedLanes(COMPAT_REGISTRY, [], DERIVED), []);
  assert.deepEqual(unvalidatedLanes(COMPAT_REGISTRY, undefined, DERIVED), []);
});

test('unvalidatedLanes refuses to attribute a lane it was given no derived set for', () => {
  // A default back to `lane.package_globs` would be the pre-#2902 answer wearing
  // the post-#2902 name, so the missing case throws instead of degrading.
  assert.throws(
    () => unvalidatedLanes(COMPAT_REGISTRY, ['internal/chclient/client.go'], new Map()),
    /no affected-path set was derived for lane/,
  );
});

// --- #2902: attribution across the shared query pipeline -------------------------

test('ACCEPTANCE: PR #2824 — the change that caused #2895 — now attributes compatibility.loki', () => {
  assert.ok(
    laneIDs(COMPAT_REGISTRY, PR_2824_FILES, DERIVED).includes('compatibility.loki'),
    'the LogQL reference lane must consider itself touched by the change it would have caught',
  );
});

test('CONTRAST: the same file set attributes NOTHING to compatibility.loki by declared globs', () => {
  // If this ever passes for compatibility.loki, the contrast has stopped
  // proving anything and the acceptance test above has become a tautology.
  assert.ok(
    !laneIDs(COMPAT_REGISTRY, PR_2824_FILES, DECLARED).includes('compatibility.loki'),
    'compatibility.loki declares no glob that PR #2824 matches — that is the hole #2902 names',
  );
});

test('compatibility.loki inherits the shared pipeline, and not the sibling heads', () => {
  const globs = DERIVED.get('compatibility.loki');
  for (const want of [
    'internal/chclient/**',
    'internal/chopt/**',
    'internal/engine/**',
    'internal/chplan/**',
    'internal/chsql/**',
    'internal/optimizer/**',
  ]) {
    assert.ok(globs.includes(want), `${want} is on the LogQL query path and must be attributed`);
  }
  for (const notWant of ['internal/promql/**', 'internal/traceql/**', 'internal/api/prom/**', 'internal/api/tempo/**']) {
    assert.ok(!globs.includes(notWant), `${notWant} cannot move a LogQL parity verdict; attributing it is noise`);
  }
});

test('NOT everything depends on everything: a single-head change stays on its own head', () => {
  const promql = laneIDs(COMPAT_REGISTRY, ['internal/promql/lower.go'], DERIVED);
  assert.ok(promql.includes('compatibility.prometheus'));
  assert.ok(!promql.includes('compatibility.loki'));
  assert.ok(!promql.includes('compatibility.tempo'));

  const traceql = laneIDs(COMPAT_REGISTRY, ['internal/traceql/ast/parser.go'], DERIVED);
  assert.ok(traceql.includes('compatibility.tempo'));
  assert.ok(!traceql.includes('compatibility.loki'));
  assert.ok(!traceql.includes('compatibility.prometheus'));
});

test('every lane the gate reports on has a derived affected-path set', () => {
  // The production run derives over EVERY gating-but-skipping lane, tag sets
  // included; a lane the derivation cannot answer for would throw at run time.
  const all = laneAffectedGlobs(gatingLanesThatSkipPRs(REGISTRY), {
    repoRoot: process.cwd(),
    // Structural check only: an empty graph leaves each lane with its declared
    // globs, which is enough to prove every lane gets an entry without paying
    // for one `go list` per build-tag set here.
    runGoList: () => '',
  });
  for (const lane of gatingLanesThatSkipPRs(REGISTRY)) {
    assert.ok(all.has(lane.id), `${lane.id} would throw at run time with no derived set`);
  }
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

test('the job sets Go up through the hardened wrapper, which the attribution needs', () => {
  // Without a toolchain in this job the derivation cannot read the import graph
  // and merge-risk.mjs exits 1 rather than falling back to the declared globs.
  assert.match(forbidDeferralWorkflow, /uses: \.\/\.github\/actions\/setup-go/);
});
