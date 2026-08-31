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
//      selects at least one leg" would pass while quietly running all eighteen;
//      asserting the precise leg set is what proves the scoping actually scopes,
//      and that a file claimed by one leg's exclude alternation is not silently
//      claimed by a sibling's too.
//
// Run: node --test .github/scripts/mutation-matrix.test.mjs (from the repo root).

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { test } from 'node:test';

import {
  addChangedLineRefs,
  changedLineProjection,
  MUTATION_LANE_ID,
  MUTATION_MIN_EFFICACY,
  MUTATION_REGISTRY_PATH,
  mutationPackageGlobs,
  mutationRegistryProjection,
  mutationSemanticHarnessStatus,
  ownershipViolations,
  phaseClaims,
  registryMutableFiles,
  resolvePhases,
  selectPhases,
  tableViolations,
} from './mutation-matrix.mjs';
import { HARNESS_PATHS, MUTATION_PRODUCTION_GLOBS, PHASES } from './mutation-phases.mjs';
import { underPrefix } from './lib/scope-gate.mjs';

const REGISTRY_SOURCE = readFileSync('.github/ci-lanes.json', 'utf8');
const REGISTRY = JSON.parse(REGISTRY_SOURCE);
const REGISTRY_SURFACE = registryMutableFiles({ registry: REGISTRY });

const select = (changed, over = {}) =>
  selectPhases({
    phases: PHASES,
    harnessPaths: HARNESS_PATHS,
    registryGlobs: REGISTRY_SURFACE.globs,
    eventName: 'pull_request',
    headRef: 'feat/some-branch',
    changed: changed === null ? null : new Set(changed),
    ...over,
  });

const names = (result) => result.phases.map((p) => p.phase).sort();

test('the shipped PHASES table is sound against this tree', () => {
  assert.deepEqual(tableViolations(PHASES), []);
  assert.deepEqual(REGISTRY_SURFACE.problems, []);
  assert.deepEqual(
    ownershipViolations({ phases: PHASES, registryFiles: REGISTRY_SURFACE.files }),
    [],
  );
});

// A non-empty matrix.diff_ref makes the `mutate` job's own `gremlins
// unleash` step run `git diff DIFF_REF` internally (mutation-run.mjs's
// header: "A non-empty DIFF_REF first invokes gremlins' native merge-base
// line filter"), which needs that ref's object actually fetched. A shallow
// (depth-1) checkout only has the job's own head commit, so any diff_ref
// pointing anywhere else fails with `fatal: bad object <sha>` — observed
// on PRs #2282-#2284, all opened against the same recent main tip, and
// silent on later PRs only because their diff_ref happened to come back
// empty (no changed-line scoping needed), not because the checkout was
// sufficient. Pins the `mutate` job's checkout to the same fetch-depth: 0
// the `select` job right above it already carries, and for the identical
// documented reason.
test("the mutate job's checkout can resolve any diff_ref gremlins might diff against", () => {
  const workflow = readFileSync('.github/workflows/mutation.yml', 'utf8');
  const mutateStart = workflow.indexOf('\n  mutate:');
  assert.ok(mutateStart >= 0, 'mutation.yml: missing mutate job');
  const nextJob = workflow.indexOf('\n  mutation:', mutateStart);
  const mutateJob = workflow.slice(mutateStart, nextJob >= 0 ? nextJob : undefined);
  const checkoutIndex = mutateJob.indexOf('actions/checkout@');
  assert.ok(checkoutIndex >= 0, 'mutate job: no actions/checkout step');
  const nextStepIndex = mutateJob.indexOf('\n      - uses:', checkoutIndex);
  const checkoutStep = mutateJob.slice(checkoutIndex, nextStepIndex >= 0 ? nextStepIndex : undefined);
  assert.match(
    checkoutStep,
    /fetch-depth:\s*0/,
    'mutate job: checkout needs fetch-depth: 0 — gremlins diffs matrix.diff_ref internally and a ' +
      'shallow clone cannot resolve an arbitrary ref',
  );
});

test('the registry-owned universe survives deletion of a complete phase', () => {
  const withoutChplan = PHASES.filter((phase) => phase.phase !== 'phase1');
  const problems = ownershipViolations({
    phases: withoutChplan,
    registryFiles: REGISTRY_SURFACE.files,
  });
  assert.ok(problems.some((problem) => /internal\/chplan\/.+claimed by no mutation phase/.test(problem)));
});

test('the canonical production anchor survives synchronized registry and phase deletion', () => {
  const narrowed = structuredClone(REGISTRY);
  const lane = narrowed.lanes.find((candidate) => candidate.id === MUTATION_LANE_ID);
  lane.package_globs = lane.package_globs.filter((glob) => glob !== 'internal/chplan/**');
  const surface = registryMutableFiles({ registry: narrowed });
  assert.deepEqual(PHASES.filter((phase) => phase.phase !== 'phase1').some((phase) => phase.scope === './internal/chplan'), false);
  assert.equal(MUTATION_PRODUCTION_GLOBS.includes('internal/chplan/**'), true);
  assert.ok(surface.problems.some((problem) => /canonical mutation surface.*internal\/chplan/.test(problem)));
});

test('the canonical production anchor rejects synchronized registry and phase expansion', () => {
  const expanded = structuredClone(REGISTRY);
  const lane = expanded.lanes.find((candidate) => candidate.id === MUTATION_LANE_ID);
  lane.package_globs.push('cmd/cerberus/**');
  const expandedPhases = [
    ...PHASES,
    { phase: 'phase-cmd', scope: './cmd/cerberus', efficacy: 95, workers: 0 },
  ];
  assert.deepEqual(tableViolations(expandedPhases), []);

  const surface = registryMutableFiles({ registry: expanded });
  assert.ok(surface.problems.some((problem) => /canonical mutation surface.*extra cmd\/cerberus\/\*\*/.test(problem)));
});

test('registry projection ignores unrelated metadata but includes mutation ownership', () => {
  const unrelated = structuredClone(REGISTRY);
  unrelated.lanes.find((lane) => lane.id !== MUTATION_LANE_ID).description = 'unrelated metadata changed';
  assert.deepEqual(mutationRegistryProjection(JSON.stringify(unrelated)), mutationRegistryProjection(REGISTRY_SOURCE));

  const relevant = structuredClone(REGISTRY);
  relevant.lanes.find((lane) => lane.id === MUTATION_LANE_ID).package_globs.push('scripts/mutation-helper.sh');
  assert.notDeepEqual(mutationRegistryProjection(JSON.stringify(relevant)), mutationRegistryProjection(REGISTRY_SOURCE));
});

test('registry semantic projection comparison fails closed and distinguishes relevant changes', () => {
  const base = 'a'.repeat(40);
  const head = 'b'.repeat(40);
  const unrelated = structuredClone(REGISTRY);
  unrelated.lanes.find((lane) => lane.id !== MUTATION_LANE_ID).description = 'changed';
  const relevant = structuredClone(REGISTRY);
  relevant.lanes.find((lane) => lane.id === MUTATION_LANE_ID).command = 'changed mutation command';
  const sources = new Map([
    [`${base}:${MUTATION_REGISTRY_PATH}`, REGISTRY_SOURCE],
    [`${head}:${MUTATION_REGISTRY_PATH}`, JSON.stringify(unrelated)],
  ]);
  const status = (over = {}) =>
    mutationSemanticHarnessStatus({
      eventName: 'pull_request',
      changed: new Set([MUTATION_REGISTRY_PATH]),
      baseSha: base,
      headSha: head,
      readAtRevision: (revision, path) => sources.get(`${revision}:${path}`),
      ...over,
    });
  assert.deepEqual(status(), { changed: false, failed: false, paths: [] });

  sources.set(`${head}:${MUTATION_REGISTRY_PATH}`, JSON.stringify(relevant));
  assert.deepEqual(status(), { changed: true, failed: false, paths: [MUTATION_REGISTRY_PATH] });
  assert.deepEqual(status({ eventName: 'merge_group' }), {
    changed: true,
    failed: false,
    paths: [MUTATION_REGISTRY_PATH],
  });

  sources.set(`${head}:${MUTATION_REGISTRY_PATH}`, '{');
  assert.equal(status().failed, true);
  assert.equal(select([MUTATION_REGISTRY_PATH], { semanticHarness: status() }).phases.length, PHASES.length);
});

test('phase claims outside the registry surface fail bidirectionally', () => {
  const narrowed = new Set(
    [...REGISTRY_SURFACE.files].filter((path) => !path.startsWith('internal/chplan/')),
  );
  const problems = ownershipViolations({ phases: PHASES, registryFiles: narrowed });
  assert.ok(problems.some((problem) => /internal\/chplan\/.+outside the registry surface/.test(problem)));
});

test('a phase that owns zero registry mutable files is rejected', () => {
  const phases = PHASES.map((phase) =>
    phase.phase === 'phase1' ? { ...phase, exclude_files: '.*' } : phase,
  );
  const problems = ownershipViolations({ phases, registryFiles: REGISTRY_SURFACE.files });
  assert.ok(
    problems.some((problem) => problem === 'mutation phase phase1 claims zero registry-owned mutable Go files'),
  );
});

test('empty phases and unknown registry glob shapes fail closed', () => {
  assert.deepEqual(
    ownershipViolations({ phases: [], registryFiles: REGISTRY_SURFACE.files }),
    ['the mutation phase table is empty'],
  );
  const parsed = mutationPackageGlobs({
    lanes: [{ id: MUTATION_LANE_ID, package_globs: ['internal/{chplan,chsql}/**'] }],
  });
  assert.equal(parsed.problems.length, 1);
  assert.match(parsed.problems.join('\n'), /unsupported shape/);
});

test('a changed registry-owned file with its complete phase removed is a selector gap', () => {
  const result = select(['internal/chplan/plan.go'], {
    phases: PHASES.filter((phase) => phase.phase !== 'phase1'),
  });
  assert.deepEqual(result.phases, []);
  assert.deepEqual(result.gaps, ['internal/chplan/plan.go']);
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

test('the committed mutation efficacy floor rejects weakening', () => {
  assert.equal(MUTATION_MIN_EFFICACY, 95);
  const problems = tableViolations([{ ...PHASES[0], efficacy: 94 }]);
  assert.deepEqual(problems, ['phase "phase1" has efficacy 94, below the committed minimum 95']);
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
  // selected. Sweeping the full matrix here instead would bill every batch 18
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

test('changed-line projection is bound to the exact merge base and added-line roster', () => {
  const base = 'a'.repeat(40);
  const head = 'b'.repeat(40);
  const mergeBase = 'c'.repeat(40);
  const calls = [];
  const projection = changedLineProjection({
    baseSha: base,
    headSha: head,
    runGit: (args) => {
      calls.push(args);
      if (args[0] === 'merge-base') return { status: 0, stdout: `${mergeBase}\n`, stderr: '' };
      return {
        status: 0,
        stdout:
          [
            '4\t2\tinternal/chplan/plan.go',
            '0\t7\tinternal/chsql/deleted.go',
            '-\t-\tinternal/chsql/blob.go',
          ].join('\0') + '\0',
        stderr: '',
      };
    },
  });
  assert.deepEqual(calls, [
    ['merge-base', base, head],
    ['diff', '--numstat', '-z', '--no-renames', mergeBase, head],
  ]);
  assert.equal(projection.ref, mergeBase);
  assert.equal(projection.additions.get('internal/chplan/plan.go'), 4);
  assert.equal(projection.additions.get('internal/chsql/deleted.go'), 0);
  assert.equal(projection.additions.get('internal/chsql/blob.go'), null);
});

test('changed-line projection fails closed on missing refs, git failure, and malformed numstat', () => {
  assert.equal(changedLineProjection({ baseSha: '', headSha: 'b'.repeat(40) }), null);
  assert.equal(
    changedLineProjection({
      baseSha: 'a'.repeat(40),
      headSha: 'b'.repeat(40),
      runGit: () => ({ status: 1, stdout: '', stderr: 'missing' }),
    }),
    null,
  );
  let call = 0;
  assert.equal(
    changedLineProjection({
      baseSha: 'a'.repeat(40),
      headSha: 'b'.repeat(40),
      runGit: () =>
        call++ === 0
          ? { status: 0, stdout: `${'c'.repeat(40)}\n`, stderr: '' }
          : { status: 0, stdout: 'not-numstat\0', stderr: '' },
    }),
    null,
  );
});

test('only production additions receive changed-line mutation refs', () => {
  const ref = 'c'.repeat(40);
  const phase = PHASES.find((candidate) => candidate.phase === 'phase1');
  const project = (changed, additions) =>
    addChangedLineRefs({
      phases: [phase],
      changed: new Set(changed),
      projection: { ref, additions: new Map(additions) },
    })[0].diff_ref;

  assert.equal(project(['internal/chplan/plan.go'], [['internal/chplan/plan.go', 3]]), ref);
  assert.equal(project(['internal/chplan/plan.go'], [['internal/chplan/plan.go', 0]]), '');
  assert.equal(project(['internal/chplan/plan_test.go'], [['internal/chplan/plan_test.go', 3]]), '');
  assert.equal(
    project(
      ['internal/chplan/plan.go', 'internal/chplan/plan_test.go'],
      [
        ['internal/chplan/plan.go', 3],
        ['internal/chplan/plan_test.go', 2],
      ],
    ),
    '',
  );
  assert.equal(
    addChangedLineRefs({ phases: [phase], changed: null, projection: null })[0].diff_ref,
    '',
  );
});

test('a change to the lane harness runs the full matrix', () => {
  assert.equal(HARNESS_PATHS.includes('.github/scripts/mutation-run.mjs'), true);
  for (const path of HARNESS_PATHS) {
    assert.equal(select([path]).phases.length, PHASES.length, path);
  }
});

test('only the registry uses a semantic harness projection', () => {
  assert.equal(HARNESS_PATHS.includes(MUTATION_REGISTRY_PATH), false);
  assert.deepEqual(select([MUTATION_REGISTRY_PATH]), {
    phases: [],
    reason: 'no changed path falls in any phase scope',
    gaps: [],
  });
  assert.deepEqual(select(['Justfile']), {
    phases: [],
    reason: 'no changed path falls in any phase scope',
    gaps: [],
  });
  const result = select([MUTATION_REGISTRY_PATH], {
    semanticHarness: { changed: true, failed: false, paths: [MUTATION_REGISTRY_PATH] },
  });
  assert.equal(result.phases.length, PHASES.length);
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
  assert.deepEqual(names(select(['internal/chsql/emit_node.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/emit.go'])), ['phase2-compare']);
  // `histogram_quantile.go` is a prefix of `histogram_quantile_native.go`, and
  // `range_window.go` is a prefix of `range_window_fused.go` — each pair lands
  // in a DIFFERENT leg, so these pin that the `$` anchor on every alternation
  // is what keeps a prefix match from swallowing its longer sibling.
  assert.deepEqual(names(select(['internal/chsql/histogram_quantile.go'])), ['phase2-other']);
  assert.deepEqual(names(select(['internal/chsql/histogram_quantile_native.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/range_window.go'])), ['phase2-range']);
  assert.deepEqual(names(select(['internal/chsql/range_window_fused.go'])), ['phase2-compare']);
  assert.deepEqual(names(select(['internal/chsql/range_window_variants.go'])), ['phase2-builder']);
  // The catch-all leg owns range_window_grid_native.go, tableshape.go, and
  // everything no sibling names.
  assert.deepEqual(names(select(['internal/chsql/range_window_grid_native.go'])), ['phase2-other']);
  assert.deepEqual(names(select(['internal/chsql/tableshape.go'])), ['phase2-other']);
});

test('resolvePhases derives an include_files leg\'s real exclude_files from a directory walk', () => {
  // cerberus issue #2814: a new file under a scope shared by several
  // exclude_files-shaped legs matched none of their hand-written patterns and
  // was therefore claimed by ALL of them (a hard `ownershipViolations`
  // failure), because an exclude-shaped leg claims anything it does not name.
  // An include_files-shaped leg cannot make that mistake — a new file is
  // never in its static allowlist — but gremlins itself only understands
  // `--exclude-files`, so resolvePhases has to translate the allowlist into
  // an equivalent denylist by actually walking the scope's real files. This
  // test proves that translation against a real (temporary) directory, not
  // just against mutation-phases.mjs's own hand-curated table.
  // resolvePhases' own `walkMutableGoFiles` joins `root` (process.cwd() below)
  // with the phase's `scope`, exactly as production scopes do — so the
  // fixture has to live UNDER cwd, not under the OS tmpdir, or that join
  // would silently walk the wrong (nonexistent) path.
  const dir = mkdtempSync(join(process.cwd(), 'mutation-resolve-fixture-'));
  const scope = `./${basename(dir)}`;
  try {
    for (const name of ['owned.go', 'sibling.go', 'shared_test.go']) {
      writeFileSync(join(dir, name), '// fixture\n');
    }
    const phases = [
      { phase: 'curated', scope, efficacy: 95, workers: 0, include_files: '^owned\\.go$' },
      { phase: 'catchall', scope, efficacy: 95, workers: 0 },
    ];

    const resolved = resolvePhases(phases, process.cwd());
    const curated = resolved.find((p) => p.phase === 'curated');
    assert.equal(curated.include_files, undefined, 'the resolved leg must not still carry include_files');
    assert.equal(curated.exclude_files, '^(sibling)\\.go$', 'sibling.go is the only OTHER real .go file in scope');

    // The property under test: a file that exists in neither owned.go's
    // include pattern NOR the derived exclude pattern (because it did not
    // exist at derivation time) is claimed by exactly the catch-all — never
    // by both, and never by neither.
    writeFileSync(join(dir, 'brand_new.go'), '// added after resolution\n');
    const reResolved = resolvePhases(phases, process.cwd());
    const newFilePath = `${basename(dir)}/brand_new.go`;
    const claimers = reResolved.filter((p) => phaseClaims(p, newFilePath));
    assert.deepEqual(
      claimers.map((p) => p.phase),
      ['catchall'],
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('resolvePhases leaves an exclude_files (or bare) leg completely untouched', () => {
  const [rangeLeg] = PHASES.filter((p) => p.phase === 'phase3-optimizer');
  const [resolved] = resolvePhases([rangeLeg], process.cwd());
  assert.deepEqual(resolved, rangeLeg);
});

test('the promql legs partition the package by exclude_files, one leg per file', () => {
  // The two oversized single files each get a dedicated leg.
  assert.deepEqual(names(select(['internal/promql/lower.go'])), ['phase4-promql-lower']);
  assert.deepEqual(names(select(['internal/promql/histogram_quantile.go'])), ['phase4-promql-quantile']);
  // `histogram_quantile.go` is a prefix of `histogram_quantile_window.go`, and
  // they land in different legs — pins the `$` anchor the same way the chsql
  // test above does.
  assert.deepEqual(names(select(['internal/promql/histogram_quantile_window.go'])), ['phase4-promql-b']);
  assert.deepEqual(names(select(['internal/promql/subquery.go'])), ['phase4-promql-a']);
  // The catch-all leg owns histogram_quantile_range.go and everything no
  // sibling names.
  assert.deepEqual(names(select(['internal/promql/histogram_quantile_range.go'])), ['phase4-promql-other']);
  assert.deepEqual(names(select(['internal/promql/doc.go'])), ['phase4-promql-other']);
});

test('the logql legs partition the package by exclude_files, one leg per file', () => {
  assert.deepEqual(names(select(['internal/logql/lower.go'])), ['phase4-logql-lower']);
  assert.deepEqual(names(select(['internal/logql/range_aggregation.go'])), ['phase4-logql-aggregation']);
  assert.deepEqual(names(select(['internal/logql/dotted_labels.go'])), ['phase4-logql-other-a']);
  // The catch-all leg owns everything no sibling names.
  assert.deepEqual(names(select(['internal/logql/drop_keep.go'])), ['phase4-logql-other-b']);
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
    registryGlobs: REGISTRY_SURFACE.globs,
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
