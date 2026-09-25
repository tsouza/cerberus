import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  PRODUCING_WORKFLOW,
  aggregateProblems,
  collectKeyInputs,
  gitBlobSha,
  harnessBlobs,
  provenanceDir,
  rapidDeterminismProblems,
  toolchainFingerprint,
  verifyProducingRun,
  literalRelativePaths,
  runnerScriptHashes,
} from './mutation-cache.mjs';
import {
  KEY_INPUT_CLASSES,
  buildEntry,
  canonicalJson,
  legCacheKey,
  phaseRowForKey,
  timingStability,
  validateEntry,
  validateProvenance,
} from './lib/mutation-cache.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const runUrl = 'https://github.com/o/r/actions/runs/123';

function sh(cwd, cmd, args) {
  const r = spawnSync(cmd, args, { cwd, encoding: 'utf8' });
  assert.equal(r.status, 0, `${cmd} ${args.join(' ')}: ${r.stderr}`);
  return r.stdout;
}

// repo builds a real Go module: scope `a` imports `b`, a's test reads the
// literal path ../fixtures, and `c` plus `notes/` sit outside the closure.
function repo(t) {
  const root = mkdtempSync(join(tmpdir(), 'mutation-cache-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const files = {
    'go.mod': 'module example.com/m\n\ngo 1.21\n',
    'go.sum': '',
    '.gremlins.yaml': 'unleash:\n  workers: 0\n',
    'a/a.go': 'package a\n\nimport "example.com/m/b"\n\nfunc A() int { return b.B() + 1 }\n',
    'a/a_test.go':
      'package a\n\nimport (\n\t"path/filepath"\n\t"testing"\n)\n\n' +
      'var fixtures = filepath.Join("..", "fixtures")\n\nfunc TestA(t *testing.T) { _ = fixtures; if A() != 2 { t.Fatal() } }\n',
    'a/testdata/golden.txt': 'golden\n',
    'b/b.go': 'package b\n\nfunc B() int { return 1 }\n',
    'c/c.go': 'package c\n\nfunc C() int { return 3 }\n',
    'fixtures/data.txt': 'data\n',
    'notes/readme.txt': 'unrelated\n',
  };
  for (const [p, body] of Object.entries(files)) {
    mkdirSync(dirname(join(root, p)), { recursive: true });
    writeFileSync(join(root, p), body);
  }
  sh(root, 'git', ['init', '-q']);
  sh(root, 'git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'add', '-A']);
  sh(root, 'git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'commit', '-qm', 'base']);
  return root;
}

const baseParams = {
  scope: './a',
  phaseRow: { phase: 'p1', scope: './a', efficacy: 90, workers: 0, exclude_files: '' },
  diffRef: '',
  runnerEnv: { MUTANT_TIMEOUT_MIN: '15s', MUTANT_TIMEOUT_MAX: '120s' },
  toolchain: {
    version: 'go version go1.26.4 linux/amd64',
    env: { CGO_ENABLED: '1', GOEXPERIMENT: '', GOFLAGS: '' },
    git: 'git version 2.55.0',
    gitConfig: { 'diff.renames': 'true', 'diff.renameLimit': '1000', 'diff.algorithm': 'myers' },
  },
  gremlins: { module: 'github.com/tsouza/gremlins/cmd/gremlins', ref: 'r1', commit: 'a'.repeat(40) },
  scripts: { 'mutation-run.mjs': 'h1', 'lib/gh.mjs': 'h2' },
  runner: { RUNNER_OS: 'Linux', RUNNER_ARCH: 'X64', ImageOS: 'ubuntu24', ImageVersion: '20260901.1', kernel: '6.8.0', node: 'v24.0.0' },
};

function keyOf(root, over = {}) {
  return legCacheKey(collectKeyInputs({ root, ...baseParams, ...over }));
}

function edit(root, path, body) {
  writeFileSync(join(root, path), body);
}

test('the key is stable for identical input and carries no absolute path', (t) => {
  const root = repo(t);
  assert.equal(keyOf(root), keyOf(root));
  const inputs = collectKeyInputs({ root, ...baseParams });
  assert.ok(!canonicalJson(inputs).includes(root), 'key inputs must be relative to the checkout');
  assert.deepEqual(Object.keys(inputs).sort(), [...KEY_INPUT_CLASSES].sort());
});

// Each case flips one input class and asserts the key moves. Dropping that
// class from the key (or from the collector) makes its case fail.
const flips = [
  ['a dependency package source file', (root) => edit(root, 'b/b.go', 'package b\n\nfunc B() int { return 2 }\n')],
  ['a scope source file', (root) => edit(root, 'a/a.go', 'package a\n\nimport "example.com/m/b"\n\nfunc A() int { return b.B() }\n')],
  ['a test file', (root) => edit(root, 'a/a_test.go', `${readFileSync(join(root, 'a/a_test.go'), 'utf8')}// more\n`)],
  ['a testdata file', (root) => edit(root, 'a/testdata/golden.txt', 'changed\n')],
  ['an embedded subdirectory file', (root) => {
    mkdirSync(join(root, 'a/templates'), { recursive: true });
    edit(root, 'a/templates/t.tmpl', 'changed\n');
  }],
  ['a data root a test reads by literal path', (root) => edit(root, 'fixtures/data.txt', 'changed\n')],
  ['go.sum', (root) => edit(root, 'go.sum', 'example.com/x v1.0.0 h1:AAAA=\n')],
  ['go.mod', (root) => edit(root, 'go.mod', 'module example.com/m\n\ngo 1.22\n')],
  ['.gremlins.yaml', (root) => edit(root, '.gremlins.yaml', 'unleash:\n  workers: 2\n')],
];
for (const [name, mutate] of flips) {
  test(`the key changes with ${name}`, (t) => {
    const root = repo(t);
    const before = keyOf(root);
    mutate(root);
    assert.notEqual(keyOf(root), before);
  });
}

const paramFlips = [
  ['the toolchain', { toolchain: { ...baseParams.toolchain, version: 'go version go1.27.0 linux/amd64' } }],
  ['the Go environment', { toolchain: { ...baseParams.toolchain, env: { ...baseParams.toolchain.env, CGO_ENABLED: '0' } } }],
  ['the gremlins ref', { gremlins: { ...baseParams.gremlins, ref: 'r2' } }],
  ['the gremlins commit', { gremlins: { ...baseParams.gremlins, commit: 'b'.repeat(40) } }],
  ['the phase row threshold', { phaseRow: { ...baseParams.phaseRow, efficacy: 91 } }],
  ['the phase row excludes', { phaseRow: { ...baseParams.phaseRow, exclude_files: '^x\\.go$' } }],
  ['the phase row workers', { phaseRow: { ...baseParams.phaseRow, workers: 1 } }],
  ['the runner bounds', { runnerEnv: { ...baseParams.runnerEnv, MUTANT_TIMEOUT_MAX: '90s' } }],
  ['a runner script', { scripts: { ...baseParams.scripts, 'mutation-run.mjs': 'h9' } }],
  ['a runner lib', { scripts: { ...baseParams.scripts, 'lib/gh.mjs': 'h9' } }],
  ['the runner image version', { runner: { ...baseParams.runner, ImageVersion: '20260915.2' } }],
  ['the node version', { runner: { ...baseParams.runner, node: 'v26.0.0' } }],
  ['the git version', { toolchain: { ...baseParams.toolchain, git: 'git version 2.56.0' } }],
  ['the pinned rename setting', { toolchain: { ...baseParams.toolchain, gitConfig: { ...baseParams.toolchain.gitConfig, 'diff.renames': 'false' } } }],
  ['the build tags in GOFLAGS', { toolchain: { ...baseParams.toolchain, env: { ...baseParams.toolchain.env, GOFLAGS: '-tags=chdb' } } }],
];
for (const [name, over] of paramFlips) {
  test(`the key changes with ${name}`, (t) => {
    const root = repo(t);
    assert.notEqual(keyOf(root, over), keyOf(root));
  });
}

test('a file outside the closure does not change the key', (t) => {
  const root = repo(t);
  const before = keyOf(root);
  edit(root, 'c/c.go', 'package c\n\nfunc C() int { return 4 }\n');
  edit(root, 'notes/readme.txt', 'changed\n');
  assert.equal(keyOf(root), before);
});

test('a changed-line leg is keyed on the scope diff, never on the diff_ref commit', (t) => {
  const root = repo(t);
  const base = sh(root, 'git', ['rev-parse', 'HEAD']).trim();
  const fullKey = keyOf(root);
  // No change since the base: the diff is empty, but a changed-line leg is
  // still distinct from a full one.
  const emptyDiff = keyOf(root, { diffRef: base, phaseRow: { ...baseParams.phaseRow, diff_ref: base } });
  assert.notEqual(emptyDiff, fullKey);
  // Same content diff against a different commit yields the same key.
  sh(root, 'git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'commit', '-q', '--allow-empty', '-m', 'empty']);
  const other = sh(root, 'git', ['rev-parse', 'HEAD']).trim();
  assert.equal(keyOf(root, { diffRef: other, phaseRow: { ...baseParams.phaseRow, diff_ref: other } }), emptyDiff);
  // A scope edit moves it.
  edit(root, 'a/a.go', `${readFileSync(join(root, 'a/a.go'), 'utf8')}// edit\n`);
  sh(root, 'git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'commit', '-qam', 'edit']);
  assert.notEqual(keyOf(root, { diffRef: base, phaseRow: { ...baseParams.phaseRow, diff_ref: base } }), emptyDiff);
});

test('legCacheKey refuses inputs missing any class, or carrying an unknown one', (t) => {
  const root = repo(t);
  const inputs = collectKeyInputs({ root, ...baseParams });
  for (const cls of KEY_INPUT_CLASSES) {
    const { [cls]: _dropped, ...rest } = inputs;
    assert.throws(() => legCacheKey(rest), new RegExp(cls));
  }
  assert.throws(() => legCacheKey({ ...inputs, extra: 1 }), /unknown key input class/);
  assert.throws(() => legCacheKey({ ...inputs, phaseRow: { diff_ref: 'x' } }), /diff_ref/);
  assert.deepEqual(phaseRowForKey({ a: 1, diff_ref: 'x' }), { a: 1 });
});

test('runnerScriptHashes follows local imports transitively', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'mutation-cache-scripts-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, 'lib'));
  writeFileSync(join(dir, 'entry.mjs'), "import { x } from './lib/x.mjs';\nimport 'node:fs';\n");
  writeFileSync(join(dir, 'lib/x.mjs'), "export { y as x } from './y.mjs';\n");
  writeFileSync(join(dir, 'lib/y.mjs'), 'export const y = 1;\n');
  const before = runnerScriptHashes(dir, ['entry.mjs']);
  assert.deepEqual(Object.keys(before), ['entry.mjs', 'lib/x.mjs', 'lib/y.mjs']);
  writeFileSync(join(dir, 'lib/y.mjs'), 'export const y = 2;\n');
  assert.notEqual(runnerScriptHashes(dir, ['entry.mjs'])['lib/y.mjs'], before['lib/y.mjs']);
});

test('the real runner-script set covers the runner, the guard, the gate and their libs', () => {
  const names = Object.keys(runnerScriptHashes(here));
  for (const n of [
    'mutation-run.mjs', 'mutant-memory-guard.mjs', 'gremlins-threshold.mjs', 'lib/gh.mjs', 'lib/mutation-cache.mjs',
    '../workflows/mutation.yml', '../actions/setup-go/action.yml',
  ]) {
    assert.ok(names.includes(n), `${n} missing from ${names.join(', ')}`);
  }
});

test('literalRelativePaths reads all-literal relative paths only', () => {
  const src =
    'var a = filepath.Join("..", "..", "test", "spec")\n' +
    'var b = filepath.Join(dir, "x")\n' +
    'var c = "../chplan"\n' +
    'var d = filepath.Join(".", name)\n';
  assert.deepEqual(literalRelativePaths(src), ['../../test/spec', '../chplan']);
});

function report(statuses) {
  return { test_efficacy: 0, mutants_total: statuses.length, files: [{ file_name: 'a.go', mutations: statuses.map((status, i) => ({ type: 'X', status, line: i, column: 1 })) }] };
}
const rep = (k, l, t = 0, r = 0) =>
  report([...Array(k).fill('KILLED'), ...Array(l).fill('LIVED'), ...Array(t).fill('TIMED OUT'), ...Array(r).fill('RUN TIMED OUT')]);

test('timingStability stores a verdict no re-scoring of its timed-out mutants can flip, and refuses one it can', () => {
  assert.equal(timingStability(rep(9, 1), 80).stable, true);
  // 90 killed, 5 lived, 5 timed out: 90..95% under re-timing, all >= 80.
  assert.deepEqual([timingStability(rep(90, 5, 3, 2), 80).stable, timingStability(rep(90, 5, 3, 2), 80).pass], [true, true]);
  // Failing verdict that stays failing: cached too.
  assert.deepEqual([timingStability(rep(1, 9, 1), 80).stable, timingStability(rep(1, 9, 1), 80).pass], [true, false]);
  // 85 killed, 10 lived, 5 timed out: 85%..90% straddles 88.
  assert.equal(timingStability(rep(85, 10, 5), 88).stable, false);
  assert.equal(timingStability(rep(85, 10, 0, 5), 88).stable, false);
  // All-timed-out: completed can reach zero under re-timing.
  assert.equal(timingStability(rep(0, 0, 3), 50).stable, false);
  assert.equal(timingStability(report(['KILLED', 'RUNNABLE']), 50).stable, false);
  assert.equal(timingStability(report(['KILLED', 'WHAT']), 50).stable, false);
  assert.equal(timingStability(report([]), 50).stable, false);
});

const key = 'c'.repeat(64);
const good = () => buildEntry({ key, phase: 'p1', threshold: 90, sourceRunUrl: runUrl, report: rep(10, 0) });

test('validateEntry accepts a sound entry and treats every defect as a miss', () => {
  const ctx = { key, phase: 'p1', threshold: 90 };
  assert.equal(validateEntry(canonicalJson(good()), ctx).ok, true);
  const bad = {
    'corrupt JSON': '{not json',
    'a truncated file': canonicalJson(good()).slice(0, 40),
    'a tampered report': { ...good(), report: rep(9, 1) },
    'a tampered digest': { ...good(), digest: 'd'.repeat(64) },
    'another key': buildEntry({ ...good(), key: 'e'.repeat(64) }),
    'another phase': buildEntry({ ...good(), phase: 'p2' }),
    'another threshold': buildEntry({ ...good(), threshold: 80 }),
    'a malformed run URL': buildEntry({ ...good(), sourceRunUrl: 'nope' }),
    'an older schema': { ...good(), schema: 'v0' },
    'a timing-unstable verdict': buildEntry({ ...good(), report: rep(85, 10, 5), threshold: 90 }),
    null: 'null',
  };
  for (const [name, raw] of Object.entries(bad)) {
    assert.equal(validateEntry(raw, ctx).ok, false, name);
  }
});

const hitFixture = () => {
  const entry = good();
  return { entry, hit: { phase: 'p1', source: 'cache', key, digest: entry.digest, sourceRunUrl: runUrl, runner: {} } };
};
const agg = (matrix, rec, over = {}) =>
  aggregateProblems(matrix, () => rec, {
    cacheAllowed: true,
    recompute: async () => ({ key, cacheable: true, reason: '' }),
    verifyRun: async () => null,
    ...over,
  });

test('the aggregator accepts runs and verified hits, and rejects anything unverified', async () => {
  const matrix = { include: [{ phase: 'p1', efficacy: 90 }] };
  const { entry, hit } = hitFixture();
  assert.deepEqual(await agg(matrix, { provenance: { phase: 'p1', source: 'run', key } }), []);
  assert.deepEqual(await agg(matrix, { provenance: hit, entry }), []);
  const one = async (name, rec, over) => assert.equal((await agg(matrix, rec, over)).length, 1, name);
  await one('missing provenance', {});
  await one('hit without entry', { provenance: hit });
  await one('tampered entry', { provenance: hit, entry: { ...entry, report: rep(1, 9) } });
  await one('digest mismatch', { provenance: { ...hit, digest: 'f'.repeat(64) }, entry });
  await one('unknown source', { provenance: { ...hit, source: 'trust-me' }, entry });
  await one('wrong phase', { provenance: { ...hit, phase: 'p2' }, entry });
  // The key the aggregator holds against is its own recomputation. A leg that
  // wrote a matching key into its record and a matching forged entry is still
  // refused when this checkout keys differently.
  const forgedKey = 'f'.repeat(64);
  const forged = buildEntry({ key: forgedKey, phase: 'p1', threshold: 90, sourceRunUrl: runUrl, report: rep(10, 0) });
  await one('leg key differs from recomputed', { provenance: { ...hit, key: forgedKey, digest: forged.digest }, entry: forged });
  await one('recompute says not cacheable', { provenance: hit, entry }, { recompute: async () => ({ key, cacheable: false, reason: 'rapid' }) });
  await one('producing run unverified', { provenance: hit, entry }, { verifyRun: async () => 'harness differs' });
  await one('recompute throws', { provenance: hit, entry }, { recompute: async () => { throw new Error('go list failed'); } });
  await one('hit where the cache is off', { provenance: hit, entry }, { cacheAllowed: false });
  assert.deepEqual(await agg(matrix, { provenance: { phase: 'p1', source: 'run', key } }, { cacheAllowed: false }), []);
  assert.equal((await agg({ include: [{ phase: 'p1', efficacy: 80 }] }, { provenance: hit, entry })).length, 1, 'threshold drift');
  assert.notEqual(validateProvenance(null, { phase: 'p1', threshold: 90 }), null);
});

function fakeApi(over = {}) {
  const repo = 'o/r';
  const run = {
    repository: { full_name: repo },
    path: PRODUCING_WORKFLOW,
    event: 'pull_request',
    status: 'completed',
    pull_requests: [{ number: 7 }],
    head_repository: { full_name: repo },
    head_branch: 'feature',
    head_sha: 'h'.repeat(40),
    ...over.run,
  };
  const pr = { head: { repo: { full_name: over.prRepo ?? repo }, ref: 'feature' } };
  const blobs = { '.github/scripts/x.mjs': 'a1', ...over.blobs };
  return async (path) => {
    if (path === '/repos/o/r/actions/runs/123') return run;
    if (path === '/repos/o/r/pulls/7') return pr;
    const m = path.match(/^\/repos\/o\/r\/contents\/(.+)\?ref=(.+)$/);
    if (m && m[2] === run.head_sha) return { sha: blobs[m[1]] };
    throw new Error(`unexpected ${path}`);
  };
}

test('verifyProducingRun accepts only a completed mutation.yml pull_request run of this PR with this harness', async () => {
  const base = { repo: 'o/r', prNumber: 7, sourceRunUrl: 'https://github.com/o/r/actions/runs/123/attempts/1', harness: { '.github/scripts/x.mjs': 'a1' } };
  assert.equal(await verifyProducingRun({ ...base, api: fakeApi() }), null);
  // A fork PR's run lists no pull_requests; head repo + branch identify it.
  assert.equal(await verifyProducingRun({ ...base, api: fakeApi({ run: { pull_requests: [] } }) }), null);
  const bad = {
    'another repo in the URL': [{ sourceRunUrl: 'https://github.com/x/y/actions/runs/123' }, {}],
    'a malformed URL': [{ sourceRunUrl: 'nope' }, {}],
    'another workflow': [{}, { run: { path: '.github/workflows/evil.yml' } }],
    'a push event': [{}, { run: { event: 'push' } }],
    'a run still in progress': [{}, { run: { status: 'in_progress' } }],
    'another PR': [{}, { run: { pull_requests: [{ number: 8 }], head_branch: 'other' } }],
    'a modified harness': [{}, { blobs: { '.github/scripts/x.mjs': 'b2' } }],
    'another repository': [{}, { run: { repository: { full_name: 'x/y' } } }],
  };
  for (const [name, [args, over]] of Object.entries(bad)) {
    assert.notEqual(await verifyProducingRun({ ...base, ...args, api: fakeApi(over) }), null, name);
  }
});

test('harnessBlobs uses git blob ids and covers the workflow and scripts', () => {
  assert.equal(gitBlobSha('hello\n'), 'ce013625030ba8dba906f756967f9e9ca394464a');
  const blobs = harnessBlobs(join(here, '..', '..'));
  for (const p of ['.github/workflows/mutation.yml', '.github/scripts/mutation-run.mjs', '.github/scripts/lib/mutation-cache.mjs']) {
    assert.match(blobs[p] ?? '', /^[0-9a-f]{40}$/, p);
  }
});

test('provenanceDir reads a phase only from its own artifact', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'mutation-cache-prov-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  // Leg p2 wrote a p1 record into its own artifact.
  mkdirSync(join(dir, 'mutation-provenance-p2', 'p1'), { recursive: true });
  mkdirSync(join(dir, 'mutation-provenance-p1', 'p1'), { recursive: true });
  assert.equal(provenanceDir(dir, 'p1', 2), join(dir, 'mutation-provenance-p1', 'p1'));
  rmSync(join(dir, 'mutation-provenance-p1'), { recursive: true });
  assert.equal(provenanceDir(dir, 'p1', 2), join(dir, 'mutation-provenance-p1', 'p1'), 'never falls back to a foreign artifact');
  const lone = mkdtempSync(join(tmpdir(), 'mutation-cache-lone-'));
  t.after(() => rmSync(lone, { recursive: true, force: true }));
  assert.equal(provenanceDir(lone, 'p1', 1), join(lone, 'p1'));
});

// absent reports whether a path is gone by trying to read it, rather than a
// stat followed by a later write to the same path.
function absent(path) {
  try {
    readFileSync(path);
    return false;
  } catch (cause) {
    if (cause.code === 'ENOENT') return true;
    throw cause;
  }
}

function cli(t, env) {
  const r = spawnSync(process.execPath, [join(here, 'mutation-cache.mjs'), 'check'], { env: { ...process.env, ...env }, encoding: 'utf8' });
  return r;
}

test('check mode: a valid entry is a hit that writes the report; a corrupt or missing one is a miss', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'mutation-cache-cli-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = {
    KEY: key, PHASE: 'p1', THRESHOLD: '90',
    ENTRY: join(dir, 'entry.json'), REPORT: join(dir, 'gremlins.json'),
    PROVENANCE: join(dir, 'provenance.json'), GITHUB_OUTPUT: join(dir, 'out'),
    MUTATION_CACHE: 'read-write',
    RESTORED: 'true', KEY_CACHEABLE: 'true', RUNNER_FILE: join(dir, 'runner.json'),
  };
  writeFileSync(env.RUNNER_FILE, '{}');
  writeFileSync(env.GITHUB_OUTPUT, '');
  let r = cli(t, env);
  assert.equal(r.status, 0, r.stdout);
  assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /hit=false/);
  assert.ok(absent(env.REPORT));

  writeFileSync(env.ENTRY, '{corrupt');
  writeFileSync(env.GITHUB_OUTPUT, '');
  r = cli(t, env);
  assert.equal(r.status, 0, r.stdout);
  assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /hit=false/);
  assert.ok(absent(env.ENTRY), 'a corrupt entry is discarded');
  assert.ok(absent(env.PROVENANCE));

  writeFileSync(env.ENTRY, canonicalJson(good()));
  writeFileSync(env.GITHUB_OUTPUT, '');
  r = cli(t, env);
  assert.equal(r.status, 0, r.stdout);
  assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /hit=true/);
  assert.match(r.stdout, /cache HIT/);
  assert.match(r.stdout, new RegExp(runUrl));
  assert.deepEqual(JSON.parse(readFileSync(env.REPORT, 'utf8')), good().report);
  assert.equal(JSON.parse(readFileSync(env.PROVENANCE, 'utf8')).source, 'cache');

  // An entry that was already in the checkout (committed by the change) is
  // not a restore: without the restore step's hit signal it is discarded,
  // and so is any entry of a leg the key step found not cacheable.
  for (const over of [{ RESTORED: '' }, { RESTORED: 'false' }, { KEY_CACHEABLE: 'false' }]) {
    rmSync(env.REPORT, { force: true });
    writeFileSync(env.ENTRY, canonicalJson(good()));
    writeFileSync(env.GITHUB_OUTPUT, '');
    r = cli(t, { ...env, ...over });
    assert.equal(r.status, 0, r.stdout);
    assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /hit=false/, JSON.stringify(over));
    assert.ok(absent(env.REPORT));
    assert.ok(absent(env.ENTRY));
  }

  // The kill switch: the same valid entry is ignored and discarded.
  rmSync(env.REPORT, { force: true });
  writeFileSync(env.GITHUB_OUTPUT, '');
  for (const off of ['off', '']) {
    writeFileSync(env.ENTRY, canonicalJson(good()));
    r = cli(t, { ...env, MUTATION_CACHE: off });
    assert.equal(r.status, 0, r.stdout);
    assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /hit=false/);
    assert.ok(absent(env.REPORT));
    assert.ok(absent(env.ENTRY));
  }
});

function commitAll(root, msg) {
  sh(root, 'git', ['add', '-A']);
  sh(root, 'git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'commit', '-qm', msg]);
  return sh(root, 'git', ['rev-parse', 'HEAD']).trim();
}

test('a changed-line key moves with the merge base even when the checkout is identical', (t) => {
  const root = repo(t);
  const b1 = sh(root, 'git', ['rev-parse', 'HEAD']).trim();
  edit(root, 'notes/readme.txt', 'second base\n');
  const b2 = commitAll(root, 'b2');
  edit(root, 'a/a.go', `${readFileSync(join(root, 'a/a.go'), 'utf8')}// head\n`);
  commitAll(root, 'head');
  const at = (ref) => keyOf(root, { diffRef: ref, phaseRow: { ...baseParams.phaseRow, diff_ref: ref } });
  assert.notEqual(at(b1), at(b2));
});

test('a changed-line key sees a rename whose source lies outside the scope', (t) => {
  const body = `package a\n\n${Array.from({ length: 40 }, (_, i) => `func F${i}() int { return ${i} }`).join('\n')}\n`;
  const build = (keepSource) => {
    const root = repo(t);
    mkdirSync(join(root, 'other'), { recursive: true });
    edit(root, 'other/f.go', body);
    const base = commitAll(root, 'base');
    edit(root, 'a/f.go', body);
    if (!keepSource) rmSync(join(root, 'other/f.go'));
    commitAll(root, 'move');
    return keyOf(root, { diffRef: base, phaseRow: { ...baseParams.phaseRow, diff_ref: base } });
  };
  assert.notEqual(build(true), build(false));
});

test('toolchainFingerprint reads the real go env and the pinned git diff settings', (t) => {
  const root = repo(t);
  const plain = toolchainFingerprint(root, { ...process.env, GOFLAGS: '' });
  assert.match(plain.version, /^go version go/);
  assert.match(plain.git, /^git version /);
  const tagged = toolchainFingerprint(root, { ...process.env, GOFLAGS: '-tags=chdb' });
  assert.equal(tagged.env.GOFLAGS, '-tags=chdb');
  assert.notEqual(canonicalJson(tagged), canonicalJson(plain));
  const cgo = toolchainFingerprint(root, { ...process.env, GOFLAGS: '', CGO_ENABLED: plain.env.CGO_ENABLED === '1' ? '0' : '1' });
  assert.notEqual(cgo.env.CGO_ENABLED, plain.env.CGO_ENABLED);
  const renames = toolchainFingerprint(root, {
    ...process.env, GOFLAGS: '', GIT_CONFIG_COUNT: '1', GIT_CONFIG_KEY_0: 'diff.renames', GIT_CONFIG_VALUE_0: 'false',
  });
  assert.equal(renames.gitConfig['diff.renames'], 'false');
  assert.notEqual(canonicalJson(renames), canonicalJson(plain));
});

test('a leg whose tests link rapid is cacheable only with the seed set and the pin hook present', (t) => {
  const root = repo(t);
  const bin = join(root, 'fakebin');
  mkdirSync(bin);
  // A `go` that reports package a's test binary as linking rapid.
  writeFileSync(
    join(bin, 'go'),
    `#!/bin/sh\nprintf 'example.com/m/a\\t%s\\tfmt pgregory.net/rapid\\n' ${JSON.stringify(join(root, 'a'))}\n`,
    { mode: 0o755 },
  );
  const env = (seed) => ({ ...process.env, PATH: `${bin}:${process.env.PATH}`, CERBERUS_RAPID_SEED: seed });
  assert.equal(rapidDeterminismProblems(root, './a', env('')).length, 2, 'no seed, no hook');
  assert.equal(rapidDeterminismProblems(root, './a', env('1')).length, 1, 'seed, no hook');
  edit(root, 'a/rapid_seed_test.go', 'package a\n\n// CERBERUS_RAPID_SEED applies to flag.Lookup("rapid.seed")\n');
  assert.deepEqual(rapidDeterminismProblems(root, './a', env('1')), []);
  assert.equal(rapidDeterminismProblems(root, './a', env('')).length, 1, 'hook, no seed');
  // A scope whose tests do not link rapid needs neither.
  assert.deepEqual(rapidDeterminismProblems(root, './a', { ...process.env, CERBERUS_RAPID_SEED: '' }), []);
});
