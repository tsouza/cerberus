// lane-closure.test.mjs — node:test guard for the lane affected-path
// derivation (#2902).
//
// The derivation has two failure directions and both are pinned here. Too
// NARROW is the bug it exists to fix: a lane that does not see the shared query
// pipeline it runs through, which is how #2824 merged without
// `compatibility.loki` considering itself touched. Too WIDE is the same
// blindness pointed the other way: if every package drags in its own test
// fixtures, every head reaches every other head and every lane is reported on
// every pull request, which is a list nobody reads. The seed-only test-edge rule
// is what separates them, so it is asserted directly rather than inferred from
// a lane's totals.
//
// The graph is supplied by a stubbed `runGoList` throughout: this suite pins
// the derivation, not the toolchain. `merge-risk.test.mjs` is where the real
// import graph of this repository is read, against PR #2824's real file set.

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  affectedGlobsFor,
  closureFrom,
  declaredGlobs,
  holdsGoSource,
  laneAffectedGlobs,
  laneSeedDirs,
  loadPackageGraph,
  splitGoListJSON,
  staticPrefix,
} from './lane-closure.mjs';

const ROOT = process.cwd();
const MODULE = 'github.com/tsouza/cerberus';

/** One `go list -json` record, rendered the way the command renders it. */
function record({ dir, imports = [], testImports = [], xtestImports = [] }) {
  return JSON.stringify(
    {
      ImportPath: `${MODULE}/${dir}`,
      Dir: `/repo/${dir}`,
      Imports: imports.map((d) => `${MODULE}/${d}`),
      TestImports: testImports.map((d) => `${MODULE}/${d}`),
      XTestImports: xtestImports.map((d) => `${MODULE}/${d}`),
    },
    null,
    2,
  );
}

function stubGoList(records) {
  const calls = [];
  const run = (repoRoot, args) => {
    calls.push(args);
    return records.map(record).join('\n');
  };
  return { run, calls };
}

// --- glob anchoring ------------------------------------------------------------

test('staticPrefix stops at the first wildcard segment', () => {
  assert.equal(staticPrefix('internal/logql/**'), 'internal/logql');
  assert.equal(staticPrefix('internal/chsql/rate_window_fanout_bound.go'), 'internal/chsql/rate_window_fanout_bound.go');
  assert.equal(staticPrefix('**/*.go'), '');
  assert.equal(staticPrefix('**'), '');
});

// --- seed derivation -----------------------------------------------------------

test('a directory glob over Go source seeds that directory', () => {
  assert.deepEqual(
    laneSeedDirs({ package_globs: ['internal/logql/**', 'internal/api/loki/**'] }, { repoRoot: ROOT }),
    ['internal/api/loki', 'internal/logql'],
  );
});

test('a glob naming ONE Go file seeds the package that file belongs to', () => {
  // Declaring a single file is a claim about which file matters, never a claim
  // that the file depends on less than its package does. Without this, a lane
  // could opt out of the closure by listing files instead of a directory.
  assert.deepEqual(
    laneSeedDirs({ package_globs: ['internal/chsql/rate_window_fanout_bound.go'] }, { repoRoot: ROOT }),
    ['internal/chsql'],
  );
});

test('an unanchored glob seeds nothing — it already matches every path', () => {
  assert.deepEqual(laneSeedDirs({ package_globs: ['**', '**/*.go', '**/*.md'] }, { repoRoot: ROOT }), []);
});

test('a directory holding no Go source is not a seed', () => {
  assert.deepEqual(laneSeedDirs({ package_globs: ['deploy/helm/**', 'docs/**'] }, { repoRoot: ROOT }), []);
  assert.equal(holdsGoSource(ROOT, 'internal/chsql'), true);
  assert.equal(holdsGoSource(ROOT, 'docs'), false);
});

test('a glob whose prefix does not exist is not a seed', () => {
  assert.deepEqual(laneSeedDirs({ package_globs: ['internal/not-a-package/**'] }, { repoRoot: ROOT }), []);
});

test('a lane with no globs at all seeds nothing rather than throwing', () => {
  assert.deepEqual(laneSeedDirs({}, { repoRoot: ROOT }), []);
});

// --- graph loading -------------------------------------------------------------

test('splitGoListJSON reads the concatenated objects go list emits', () => {
  const parsed = splitGoListJSON([record({ dir: 'a' }), record({ dir: 'b', imports: ['a'] })].join('\n'));
  assert.deepEqual(parsed.map((p) => p.ImportPath), [`${MODULE}/a`, `${MODULE}/b`]);
});

test('loadPackageGraph keeps first-party edges and drops everything else', () => {
  const run = (repoRoot, args) => [
    JSON.stringify(
      {
        ImportPath: `${MODULE}/internal/api/loki`,
        Dir: '/repo/internal/api/loki',
        Imports: [`${MODULE}/internal/engine`, 'net/http', 'github.com/ClickHouse/clickhouse-go/v2'],
        TestImports: [`${MODULE}/test/spec`],
      },
      null,
      2,
    ),
  ].join('\n');
  const graph = loadPackageGraph({ repoRoot: '/repo', runGoList: run });
  assert.deepEqual([...graph.get('internal/api/loki').imports], ['internal/engine']);
  assert.deepEqual([...graph.get('internal/api/loki').testImports], ['test/spec']);
});

test('loadPackageGraph passes the lane build tags through to go list', () => {
  const stub = stubGoList([]);
  loadPackageGraph({ repoRoot: '/repo', tags: ['chdb', 'agpl_oracle'], runGoList: stub.run });
  assert.deepEqual(stub.calls[0].slice(-3), ['-tags', 'chdb,agpl_oracle', './...']);
});

// --- the closure itself ---------------------------------------------------------

const PIPELINE = [
  { dir: 'internal/api/loki', imports: ['internal/logql', 'internal/engine'], xtestImports: ['test/spec'] },
  { dir: 'internal/logql', imports: ['internal/chplan'] },
  { dir: 'internal/api/prom', imports: ['internal/promql', 'internal/engine'] },
  { dir: 'internal/promql', imports: ['internal/chplan'] },
  { dir: 'internal/engine', imports: ['internal/chclient'] },
  { dir: 'internal/chclient', imports: ['internal/chopt'] },
  { dir: 'internal/chopt', imports: [] },
  { dir: 'internal/chplan', imports: [] },
  { dir: 'test/spec', imports: ['internal/promql'], testImports: ['internal/traceql'] },
  { dir: 'internal/traceql', imports: [] },
];

function pipelineGraph() {
  const stub = stubGoList(PIPELINE);
  return loadPackageGraph({ repoRoot: '/repo', runGoList: stub.run });
}

test('NARROW DIRECTION: the closure reaches the shared pipeline the seed runs through', () => {
  // The #2902 shape: a Loki-seeded lane must see chclient/chopt/engine, which
  // is what PR #2824 moved and what the declared globs never named.
  const reached = closureFrom(pipelineGraph(), ['internal/api/loki', 'internal/logql']);
  for (const want of ['internal/engine', 'internal/chclient', 'internal/chopt', 'internal/chplan']) {
    assert.ok(reached.has(want), `${want} is on the LogQL query path and must be reachable`);
  }
});

test('WIDE DIRECTION: a seed does not reach a sibling head it never imports', () => {
  const reached = closureFrom(pipelineGraph(), ['internal/api/loki', 'internal/logql']);
  assert.equal(reached.has('internal/api/prom'), false);
});

test('a seed contributes its TEST imports; a package merely reached does not', () => {
  // This one rule is what keeps the closure from collapsing into "everything".
  // `test/spec` is an external test import of `internal/api/loki`, so a Loki
  // seed reaches it and then follows its PRODUCTION import to internal/promql —
  // but never its test import of internal/traceql, because test/spec is a
  // transitive dependency here, not a package the lane runs the tests of.
  const reached = closureFrom(pipelineGraph(), ['internal/api/loki']);
  assert.equal(reached.has('test/spec'), true, 'a seed links what its own tests import');
  assert.equal(reached.has('internal/promql'), true, 'production imports are followed transitively');
  assert.equal(reached.has('internal/traceql'), false, 'a dependency does not drag in its own test imports');

  // Seeded directly, the same package DOES contribute its test imports.
  assert.equal(closureFrom(pipelineGraph(), ['test/spec']).has('internal/traceql'), true);
});

test('a seed directory stands for every package beneath it', () => {
  const graph = loadPackageGraph({
    repoRoot: '/repo',
    runGoList: stubGoList([
      { dir: 'internal/logql', imports: [] },
      { dir: 'internal/logql/lsyntax', imports: ['internal/chplan'] },
      { dir: 'internal/chplan', imports: [] },
    ]).run,
  });
  assert.equal(closureFrom(graph, ['internal/logql']).has('internal/chplan'), true);
});

// --- assembly -------------------------------------------------------------------

test('affectedGlobsFor unions the declared globs with the derived packages', () => {
  const globs = affectedGlobsFor(
    { package_globs: ['compatibility/loki/**', 'internal/api/loki/**'] },
    new Set(['internal/api/loki', 'internal/chclient']),
  );
  // The declared non-Go path survives — go list never sees a compose file or a
  // reference-stack config, so the declaration is still what carries them.
  assert.ok(globs.includes('compatibility/loki/**'));
  assert.ok(globs.includes('internal/chclient/**'));
});

test('laneAffectedGlobs loads ONE graph per distinct build-tag set', () => {
  const stub = stubGoList(PIPELINE);
  const lanes = [
    { id: 'a', package_globs: ['internal/logql/**'], build_tags: [] },
    { id: 'b', package_globs: ['internal/promql/**'], build_tags: [] },
    { id: 'c', package_globs: ['internal/promql/**'], build_tags: ['chdb'] },
    { id: 'd', package_globs: ['internal/promql/**'], build_tags: ['chdb'] },
  ];
  const out = laneAffectedGlobs(lanes, { repoRoot: ROOT, runGoList: stub.run });
  assert.equal(stub.calls.length, 2, 'tag sets, not lanes, decide how often go list runs');
  assert.deepEqual([...out.keys()], ['a', 'b', 'c', 'd']);
});

test('laneAffectedGlobs falls back to nothing extra for a lane with no Go seed', () => {
  const stub = stubGoList(PIPELINE);
  const out = laneAffectedGlobs([{ id: 'docs', package_globs: ['docs/**'], build_tags: [] }], {
    repoRoot: ROOT,
    runGoList: stub.run,
  });
  assert.deepEqual(out.get('docs'), ['docs/**']);
});

test('declaredGlobs returns the raw declaration — the pre-#2902 attribution', () => {
  const lanes = [{ id: 'x', package_globs: ['internal/logql/**'] }];
  assert.deepEqual(declaredGlobs(lanes).get('x'), ['internal/logql/**']);
});
