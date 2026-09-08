// forbid-skip.test.mjs — node:test guard for the GA test-discipline gate's
// CLI, run end-to-end against a real throwaway git repository.
//
// forbid-skip.mjs has no importable pure functions (unlike repo-hygiene.mjs):
// every scan is a closure over `process.env.CHECK` and the module runs its
// dispatch as a side effect of being loaded, so the only way to exercise it
// is to spawn the real CLI as a subprocess — exactly as
// repo-hygiene.test.mjs already does for its own end-to-end assertions.
//
// The case this file exists to pin (#1938): every scan derives its file set
// from lib/gh.mjs's lsFiles(), which used to run a bare `git ls-files` — the
// git INDEX only. A file a generator just wrote but never `git add`-ed was
// therefore invisible: the gate reported clean on content it would reject
// the instant the file was staged. lsFiles() now also reads
// `--others --exclude-standard`, so an untracked-but-not-ignored violation
// is caught too, while a `.gitignore`d path stays out of scope exactly as
// before. gh.test.mjs pins lsFiles() itself directly; this file pins the
// same gap from the CLI's own vantage point, against the actual scan a
// contributor runs locally after CI reds (CLAUDE.md's own narrowed-local-
// repro rule).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'forbid-skip.mjs');

// newFixtureRepo — a throwaway git repo with one committed, clean Go file so
// `git log` / `git status` behave normally. Returns a `write(relPath,
// content)` helper that writes a file WITHOUT staging it — the point of
// every "untracked" case below is that the file is never `git add`-ed.
function newFixtureRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-skip-'));
  const run = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  run(['init', '--quiet']);
  writeFileSync(join(dir, 'ok_test.go'), 'package main\n\nfunc TestOK(t *testing.T) {}\n');
  run(['add', 'ok_test.go']);
  run(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'seed']);
  return {
    dir,
    write(relPath, content) {
      mkdirSync(join(dir, dirname(relPath)), { recursive: true });
      writeFileSync(join(dir, relPath), content);
    },
  };
}

function runGate(check, cwd) {
  const res = spawnSync(process.execPath, [CLI], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, CHECK: check },
  });
  return { status: res.status, out: `${res.stdout}${res.stderr}` };
}

test('the CLI passes CHECK=t-skip on a clean fixture tree', () => {
  const { dir } = newFixtureRepo();
  const { status, out } = runGate('t-skip', dir);
  assert.equal(status, 0, `t-skip must pass on a clean tree; got:\n${out}`);
});

test('the CLI FAILS CHECK=t-skip on an UNTRACKED violating file, naming it (#1938)', () => {
  const { dir, write } = newFixtureRepo();
  // Deliberately not `git add`-ed — this is the exact gap #1938 describes: a
  // generator wrote the file but nobody staged it yet.
  write('generated_test.go', 'package main\n\nfunc TestBad(t *testing.T) { t.Skip("flaky") }\n');
  const { status, out } = runGate('t-skip', dir);
  assert.notEqual(
    status,
    0,
    `t-skip must fail on an untracked t.Skip file; a bare \`git ls-files\` regression would exit 0 here:\n${out}`,
  );
  assert.match(out, /::error::/);
  assert.match(out, /generated_test\.go/);
});

test('the CLI still ignores an untracked violating file under the upstream exclude', () => {
  const { dir, write } = newFixtureRepo();
  write(
    'compatibility/promql/upstream/vendored_test.go',
    'package main\n\nfunc TestVendored(t *testing.T) { t.Skip("upstream") }\n',
  );
  const { status, out } = runGate('t-skip', dir);
  assert.equal(status, 0, `the upstream exclude pathspec must still apply to an untracked path; got:\n${out}`);
});

test('the CLI FAILS CHECK=t-skip on a TRACKED violating file, same as before', () => {
  const { dir, write } = newFixtureRepo();
  write('tracked_test.go', 'package main\n\nfunc TestBad(t *testing.T) { t.Skip("flaky") }\n');
  const add = spawnSync('git', ['add', 'tracked_test.go'], { cwd: dir, encoding: 'utf8' });
  assert.equal(add.status, 0, add.stderr);
  const { status, out } = runGate('t-skip', dir);
  assert.notEqual(status, 0, `t-skip must still fail on a tracked t.Skip file; got:\n${out}`);
  assert.match(out, /tracked_test\.go/);
});

test('the CLI rejects an unknown CHECK rather than passing silently', () => {
  const { dir } = newFixtureRepo();
  const { status, out } = runGate('', dir);
  assert.notEqual(status, 0);
  assert.match(out, /unknown CHECK/);
});

// ---------------------------------------------------------------------------
// The arms that had NO test proving they can go red (#3182).
//
// Three of the six CHECK arms — should-skip, escape-hatch and playwright-skip
// — were exercised nowhere: not here, not in scripts/test-forbid-skip.sh, not
// in test/regression. docs/forbid-skip.md described that state as a design and
// said their regexes were "pinned by the CI and lefthook copies alone", which
// pins nothing: those two are RUNNERS of the regex, not assertions about it.
// Neither can fail on a clean tree, so a regex mutated to match nothing stays
// green in both, twice.
//
// Each arm gets both directions against the REAL CLI: a violation must be
// found and named, and the shape that merely resembles one must not be. Only
// the pair proves the scan discriminates rather than always-matching.
// ---------------------------------------------------------------------------

// Each of these arms reads a corpus the seed repo does not have, and a scan
// whose pathspec matches zero files now exits 1 rather than passing having read
// nothing (lsFilesRequired). So every case below seeds the arm's own corpus
// first — which also means the clean cases prove a real clean pass, not an
// empty one.

test('CHECK=should-skip passes on a compatibility corpus with only empty blocks', () => {
  const { dir, write } = newFixtureRepo();
  write('compatibility/loki/cases/basic.yaml', 'name: basic\nshould_skip: []\n');
  const { status, out } = runGate('should-skip', dir);
  assert.equal(status, 0, `an empty should_skip block is the ACCEPTED form; got:\n${out}`);
});

test('CHECK=should-skip FAILS on a non-empty should_skip block, naming it', () => {
  const { dir, write } = newFixtureRepo();
  write('compatibility/loki/cases/basic.yaml', 'name: basic\nshould_skip:\n  - upstream is wrong\n');
  const { status, out } = runGate('should-skip', dir);
  assert.notEqual(status, 0, `a non-empty should_skip block must fail the gate; got:\n${out}`);
  assert.match(out, /non-empty should_skip block/);
  assert.match(out, /basic\.yaml/);
});

test('CHECK=should-skip sees through comments and blank lines to the first entry', () => {
  // The perl program tolerates comment and blank lines between the key and the
  // first `-`. A scan that stopped at the first non-entry line would let a
  // commented-out justification hide the block it justifies.
  const { dir, write } = newFixtureRepo();
  write(
    'compatibility/loki/cases/basic.yaml',
    'name: basic\nshould_skip:\n  # waiting on upstream\n\n  - upstream is wrong\n',
  );
  const { status, out } = runGate('should-skip', dir);
  assert.notEqual(status, 0, `a block behind a comment must still fail; got:\n${out}`);
});

test('CHECK=should-skip ignores a vendored upstream corpus file', () => {
  const { dir, write } = newFixtureRepo();
  write('compatibility/loki/cases/basic.yaml', 'name: basic\nshould_skip: []\n');
  write('compatibility/loki/upstream/cases/vendored.yaml', 'should_skip:\n  - upstream owns this\n');
  const { status, out } = runGate('should-skip', dir);
  assert.equal(status, 0, `the upstream exclude must apply to the should-skip corpus too; got:\n${out}`);
});

test('CHECK=escape-hatch passes on an ordinary assertion', () => {
  const { dir, write } = newFixtureRepo();
  write('probe.ts', 'export const check = (n: number) => { if (n !== 1) throw new Error("bad"); };\n');
  const { status, out } = runGate('escape-hatch', dir);
  assert.equal(status, 0, `a hard assertion must pass; got:\n${out}`);
});

test('CHECK=escape-hatch FAILS on a tolerance marker, naming the file', () => {
  const { dir, write } = newFixtureRepo();
  write('probe.ts', 'const EXPECTED_TOLERATED = ["panel 4 is empty"];\n');
  const { status, out } = runGate('escape-hatch', dir);
  assert.notEqual(status, 0, `a tolerance list must fail the gate; got:\n${out}`);
  assert.match(out, /probe\.ts/);
});

test('CHECK=escape-hatch FAILS on a soft assertion in a spec', () => {
  const { dir, write } = newFixtureRepo();
  write('probe.ts', 'await expect.soft(locator).toBeVisible();\n');
  const { status, out } = runGate('escape-hatch', dir);
  assert.notEqual(status, 0, `expect.soft must fail the gate; got:\n${out}`);
});

test('CHECK=playwright-skip passes on a spec that runs and asserts', () => {
  const { dir, write } = newFixtureRepo();
  write('e2e/ok.spec.ts', "test('renders', async () => { expect(1).toBe(1); });\n");
  const { status, out } = runGate('playwright-skip', dir);
  assert.equal(status, 0, `an ordinary spec must pass; got:\n${out}`);
});

test('CHECK=playwright-skip FAILS on test.skip, test.fixme and test.only alike', () => {
  // All three are the same defect: .skip and .fixme silence THIS spec, .only
  // silences every other spec in the file. A gate that caught two of the three
  // would leave the third as the obvious way round it.
  for (const form of ['test.skip(', 'test.fixme(', 'test.only(']) {
    const { dir, write } = newFixtureRepo();
    write('e2e/bad.spec.ts', `${form}'flaky', async () => {});\n`);
    const { status, out } = runGate('playwright-skip', dir);
    assert.notEqual(status, 0, `${form} must fail the gate; got:\n${out}`);
    assert.match(out, /bad\.spec\.ts/);
  }
});

test('CHECK=playwright-skip FAILS on a conditional skip wearing an env check', () => {
  // The exact shape #3181 removed from tempo_two_phase_compare.spec.ts: a
  // conditional skip is what let a spec that ran in NO lane look healthy.
  const { dir, write } = newFixtureRepo();
  write('e2e/bad.spec.ts', "test.skip(!process.env.CERBERUS_NOSPLIT_URL, 'needs the second head');\n");
  const { status, out } = runGate('playwright-skip', dir);
  assert.notEqual(status, 0, `a conditional skip must fail the gate; got:\n${out}`);
});

test('CHECK=playwright-skip does not fire on an identifier that merely ends in .skip', () => {
  // The discrimination that makes the scan more than a substring search: a
  // property access on something that is not the test runner must not trip it.
  const { dir, write } = newFixtureRepo();
  write('e2e/ok.spec.ts', 'const n = report.latest.skip (0);\nconst m = counters.skip(1);\n');
  const { status, out } = runGate('playwright-skip', dir);
  assert.equal(status, 0, `a non-runner .skip must not trip the scan; got:\n${out}`);
});
