// mutation-matrix.test.mjs — unit tests for the mutation lane's phase selector.
//
// Two things are pinned here, and both are gates rather than documentation:
//
//   1. The REAL PHASES table is sound against the REAL tree — every scope is an
//      existing directory, every exclude_files pattern is RE2-legal. This is the
//      cheap in-process version of the check that otherwise costs a full
//      checkout + toolchain setup per leg before failing.
//
//   2. The selection is EXACT, not merely non-empty. Asserting "chplan.go
//      selects at least one leg" would pass while quietly running all fifteen;
//      asserting the precise leg set is what proves the scoping actually scopes,
//      and that a file claimed by one leg's exclude alternation is not silently
//      claimed by a sibling's too.
//
// Run: node --test .github/scripts/mutation-matrix.test.mjs (from the repo root).

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { test } from 'node:test';

import { phaseClaims, selectPhases, tableViolations } from './mutation-matrix.mjs';
import { HARNESS_PATHS, PHASES } from './mutation-phases.mjs';
import { underPrefix } from './lib/scope-gate.mjs';

const select = (changed, over = {}) =>
  selectPhases({
    phases: PHASES,
    harnessPaths: HARNESS_PATHS,
    eventName: 'pull_request',
    headRef: 'feat/some-branch',
    changed: changed === null ? null : new Set(changed),
    ...over,
  });

const names = (result) => result.phases.map((p) => p.phase).sort();

test('the shipped PHASES table is sound against this tree', () => {
  assert.deepEqual(tableViolations(PHASES), []);
});

test('duplicate phase names are rejected', () => {
  const dup = [PHASES[0], { ...PHASES[1], phase: PHASES[0].phase }];
  const problems = tableViolations(dup);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /duplicate phase name/);
});

test('a scope that is not a directory is rejected', () => {
  const problems = tableViolations([{ ...PHASES[0], scope: './internal/does-not-exist' }]);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /not a directory/);
});

test('an exclude_files pattern using RE2-illegal lookahead is rejected', () => {
  // The exact shape that errored out round 11 of the traceql split at run time.
  const problems = tableViolations([{ ...PHASES[0], exclude_files: '^(?!parser)\\w+\\.go$' }]);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /negative lookahead/);
});

test('a non-percentage efficacy and a negative worker count are rejected', () => {
  const problems = tableViolations([{ ...PHASES[0], efficacy: 140, workers: -1 }]);
  assert.equal(problems.length, 2);
});

test('push, schedule, dispatch and release PRs all run the full matrix', () => {
  for (const eventName of ['push', 'schedule', 'workflow_dispatch']) {
    assert.equal(select(['docs/engine.md'], { eventName }).phases.length, PHASES.length, eventName);
  }
  const release = select(['CHANGELOG.md'], { headRef: 'release/v1.13.2-chart-0.13.2' });
  assert.equal(release.phases.length, PHASES.length);
});

test('a merge-queue batch selects legs from its own diff, like a pull request', () => {
  // The queue entry is a pre-merge gate on a projected trunk: `base_sha..head_sha`
  // is the union of the batched PRs' diffs, so it selects the same legs those PRs
  // selected. Sweeping the full matrix here instead would bill every batch ~15
  // gremlins legs on top of the push-to-main sweep that lands the same SHA — the
  // wall-clock cost scoping exists to remove, paid twice.
  const inQueue = { eventName: 'merge_group', headRef: '' };
  assert.deepEqual(names(select(['internal/chplan/plan.go'], inQueue)), ['phase1']);
  assert.deepEqual(select(['docs/engine.md'], inQueue).phases, []);

  // The safety net is unchanged: a batch whose diff cannot be computed sweeps
  // everything rather than selecting nothing.
  assert.equal(select(null, inQueue).phases.length, PHASES.length);
});

test('an uncomputable diff runs the full matrix rather than nothing', () => {
  const result = select(null);
  assert.equal(result.phases.length, PHASES.length);
  assert.match(result.reason, /could not be computed/);
});

test('a change to the lane harness runs the full matrix', () => {
  for (const path of HARNESS_PATHS) {
    assert.equal(select([path]).phases.length, PHASES.length, path);
  }
});

test('a docs-only PR runs no phase at all', () => {
  const result = select(['docs/engine.md', 'README.md', '.github/workflows/ci.yml']);
  assert.deepEqual(result.phases, []);
  assert.deepEqual(result.gaps, []);
});

test('a change under one scope selects exactly that scope’s leg', () => {
  assert.deepEqual(names(select(['internal/chplan/plan.go'])), ['phase1']);
  assert.deepEqual(names(select(['internal/spansscan/match.go'])), ['phase6-spansscan']);
});

test('the chsql legs partition the package by exclude_files, one leg per file', () => {
  assert.deepEqual(names(select(['internal/chsql/metrics_compare.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/emit_node.go'])), ['phase2-builder']);
  // `emit.go` and `emit_node.go` differ only by suffix, and `range_window.go` is
  // a prefix of three siblings split across all three legs — the `$` anchor on
  // every alternation is what keeps those from colliding.
  assert.deepEqual(names(select(['internal/chsql/emit.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/range_window_native.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/range_window_resample.go'])), ['phase2-builder']);
  // The catch-all leg owns range_window.go and everything no sibling names.
  assert.deepEqual(names(select(['internal/chsql/range_window.go'])), ['phase2-other']);
  assert.deepEqual(names(select(['internal/chsql/tableshape.go'])), ['phase2-other']);
});

test('the logql legs partition the package by exclude_files, one leg per file', () => {
  assert.deepEqual(names(select(['internal/logql/lower.go'])), ['phase4-logql-lower']);
  assert.deepEqual(names(select(['internal/logql/range_aggregation.go'])), ['phase4-logql-aggregation']);
  assert.deepEqual(names(select(['internal/logql/dotted_labels.go'])), ['phase4-logql-other-a']);
  // The catch-all leg owns everything no sibling names.
  assert.deepEqual(names(select(['internal/logql/variants.go'])), ['phase4-logql-other-b']);
  assert.deepEqual(names(select(['internal/logql/logpattern/parse.go'])), ['phase4-logql-other-b']);
});

test('the lsyntax subtree is excluded from the logql legs and owned by its own', () => {
  // The round-13 incident in reverse: `scope: ./internal/logql` RECURSES into
  // lsyntax/, so without the `^lsyntax/` excludes these four legs would each
  // claim the parser too.
  assert.deepEqual(names(select(['internal/logql/lsyntax/parser.go'])), ['phase4-logql-parser']);
  assert.deepEqual(names(select(['internal/logql/lsyntax/lexer.go'])), ['phase4-logql-lsyntax']);
});

test('the traceql ast subtree is excluded from the package legs and owned by its own', () => {
  assert.deepEqual(names(select(['internal/traceql/lower.go'])), ['phase4-traceql-lower']);
  assert.deepEqual(names(select(['internal/traceql/metrics_compare.go'])), ['phase4-traceql-other']);
  assert.deepEqual(names(select(['internal/traceql/ast/parser.go'])), ['phase4-traceql-parser']);
  assert.deepEqual(names(select(['internal/traceql/ast/lexer.go'])), ['phase4-traceql-ast']);
});

test('several changed paths union their legs', () => {
  assert.deepEqual(names(select(['internal/chplan/plan.go', 'internal/traceql/ast/parser.go', 'docs/engine.md'])), [
    'phase1',
    'phase4-traceql-parser',
  ]);
});

test('a file every leg excludes is reported as a coverage gap, not dropped', () => {
  // A scope whose only leg excludes the changed file: the file is inside a
  // package the lane owns, yet nothing mutates it on any event.
  const phases = [{ phase: 'solo', scope: './internal/chplan', efficacy: 95, workers: 0, exclude_files: '^plan\\.go$' }];
  const result = selectPhases({
    phases,
    harnessPaths: HARNESS_PATHS,
    eventName: 'pull_request',
    headRef: 'feat/x',
    changed: new Set(['internal/chplan/plan.go']),
  });
  assert.deepEqual(result.phases, []);
  assert.deepEqual(result.gaps, ['internal/chplan/plan.go']);
});

test('scope matching is segment-wise, so a prefix cannot claim a sibling package', () => {
  assert.equal(underPrefix('internal/logql/lower.go', './internal/log'), false);
  assert.equal(underPrefix('internal/logql/lower.go', './internal/logql'), true);
  assert.equal(underPrefix('internal/logql', './internal/logql'), true);
});

test('phaseClaims applies the exclude to the SCOPE-RELATIVE path, as gremlins does', () => {
  const leg = PHASES.find((p) => p.phase === 'phase4-traceql-parser');
  // `parser.go` is scope-relative to ./internal/traceql/ast; the same basename
  // one directory up is a different file and belongs to a different leg.
  assert.equal(phaseClaims(leg, 'internal/traceql/ast/parser.go'), true);
  assert.equal(phaseClaims(leg, 'internal/traceql/lower.go'), false);
});

test('dump mode writes the table to stdout as JSON and nothing else', () => {
  // test/regression/mutation_leg_partition_test.go parses this stream to check
  // an invariant no single leg's regex can express — every mutable file under a
  // mutated package claimed by exactly one leg. A stray log line on stdout
  // breaks that parse, so the contract is pinned here rather than discovered in
  // the Go suite.
  const dumped = execFileSync(process.execPath, ['.github/scripts/mutation-matrix.mjs', 'dump'], {
    encoding: 'utf8',
  });
  const legs = JSON.parse(dumped);
  assert.deepEqual(
    legs.map((leg) => leg.phase),
    PHASES.map((p) => p.phase),
  );
  for (const leg of legs) assert.ok(leg.scope, `dumped leg ${leg.phase} lost its scope`);
});
