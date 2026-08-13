// coverage-summary.test.mjs — pins for the coverage floor gate.
//
// The failure this gate exists to prevent is a report that cannot fail, so the
// load-bearing cases here are the refusals: a drop below a floor, a package
// with no floor, a floor with no package, and an update that would lower a
// floor. A green assertion on the happy path proves nothing on its own — the
// awk this replaced was green on every input ever handed to it.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { compare, floorFor, nextFloors, packageOf, parseProfile, resolveLanes, rows } from './coverage-summary.mjs';

const SCRIPT = fileURLToPath(new URL('./coverage-summary.mjs', import.meta.url));

// profile renders a cover profile from `[file, stmts, count]` triples.
function profile(blocks) {
  const lines = ['mode: set'];
  let line = 1;
  for (const [file, stmts, count] of blocks) {
    lines.push(`github.com/tsouza/cerberus/${file}:${line}.1,${line + 1}.2 ${stmts} ${count}`);
    line += 2;
  }
  return `${lines.join('\n')}\n`;
}

// fixture lays down a profile and a floor ledger in a scratch directory.
function fixture(blocks, floors) {
  const dir = mkdtempSync(path.join(tmpdir(), 'coverage-floor-'));
  const profilePath = path.join(dir, 'cover-merged.out');
  const floorsPath = path.join(dir, 'coverage-floor.json');
  writeFileSync(profilePath, profile(blocks));
  if (floors !== undefined) writeFileSync(floorsPath, `${JSON.stringify(floors, null, 2)}\n`);
  return { dir, profilePath, floorsPath };
}

function run(profilePath, floorsPath, env = {}) {
  const res = spawnSync(process.execPath, [SCRIPT], {
    encoding: 'utf8',
    env: {
      ...process.env,
      GITHUB_STEP_SUMMARY: '',
      COVERAGE_PROFILE: profilePath,
      COVERAGE_FLOORS: floorsPath,
      COVERAGE_LANES: 'default+chdb',
      COVERAGE_REQUIRE_LANES: '',
      COVERAGE_UPDATE_FLOORS: '',
      ...env,
    },
  });
  // Workflow commands percent-encode their payload; decode the two escapes
  // these messages carry so the assertions can read as the human does.
  const out = `${res.stdout}${res.stderr}`.replaceAll('%0A', '\n').replaceAll('%25', '%');
  return { status: res.status, out };
}

test('a package path is the profile path minus the module prefix and the file name', () => {
  assert.equal(packageOf('github.com/tsouza/cerberus/internal/chsql/emit.go'), 'internal/chsql');
  assert.equal(packageOf('github.com/tsouza/cerberus/cmd/cerberus/main.go'), 'cmd/cerberus');
  // A path outside the module keeps its shape rather than being silently
  // reattributed to some in-tree package.
  assert.equal(packageOf('example.com/other/pkg/file.go'), 'example.com/other/pkg');
});

test('blocks aggregate per package, counting a block covered when its count is non-zero', () => {
  const { packages, err } = parseProfile(
    profile([
      ['internal/a/one.go', 4, 1],
      ['internal/a/two.go', 6, 0],
      ['internal/b/one.go', 5, 3],
    ]),
  );
  assert.equal(err, undefined);
  assert.deepEqual(packages.get('internal/a'), { total: 10, covered: 4 });
  assert.deepEqual(packages.get('internal/b'), { total: 5, covered: 5 });
});

test('a block reported by several test binaries is folded, not counted once per binary', () => {
  // The `-coverpkg=./...` shape: every test binary that links a package emits
  // its own row for every block, and the same block arrives covered from one
  // binary and uncovered from another. Summing the repeats would inflate
  // `total` by the number of binaries and make the percentage a fact about the
  // suite's shape rather than about the package.
  const one = 'github.com/tsouza/cerberus/internal/a/one.go:1.1,2.2 4';
  const { packages, err } = parseProfile(`mode: set\n${one} 0\n${one} 3\n${one} 0\n`);
  assert.equal(err, undefined);
  assert.deepEqual(packages.get('internal/a'), { total: 4, covered: 4 });
});

test('an empty or malformed profile is an error, not an empty report', () => {
  // A zero-package profile scoring 100% would be the purest hollow green
  // available: nothing measured, nothing to compare, job green.
  assert.match(parseProfile('mode: set\n').err, /no coverage blocks/);
  assert.match(parseProfile('mode: set\nnot-a-block-line\n').err, /malformed/);
});

test('the table is widest coverage first, in the column shape the summary has always used', () => {
  const { packages } = parseProfile(
    profile([
      ['internal/low/a.go', 10, 0],
      ['internal/high/a.go', 10, 1],
      ['internal/mid/a.go', 4, 1],
      ['internal/mid/b.go', 4, 0],
    ]),
  );
  assert.deepEqual(rows(packages), [
    '100.00%     10 / 10     internal/high',
    ' 50.00%      4 / 8      internal/mid',
    '  0.00%      0 / 10     internal/low',
  ]);
});

test('a floor sits a fixed slack below the measurement and never goes negative', () => {
  assert.equal(floorFor(82.55), 81.5);
  assert.equal(floorFor(100), 99);
  assert.equal(floorFor(0.4), 0);
});

test('comparison reports drops, unfloored packages and vanished floors', () => {
  const { packages } = parseProfile(
    profile([
      ['internal/dropped/a.go', 10, 0],
      ['internal/fresh/a.go', 10, 1],
    ]),
  );
  const { below, unfloored, missing } = compare(packages, {
    'internal/dropped': 90,
    'internal/renamed': 50,
  });
  assert.deepEqual(
    below.map((b) => b.pkg),
    ['internal/dropped'],
  );
  assert.deepEqual(unfloored, ['internal/fresh']);
  assert.deepEqual(missing, ['internal/renamed']);
});

test('a floor of 0 is treated as no floor at all, not as a floor that always passes', () => {
  // The hole this closes: a 0 floor satisfies the "carries statements but no
  // floor" check while nothing can fall through it. Every statement in the
  // package could be deleted and the comparison would still pass.
  const { packages } = parseProfile(profile([['internal/zero/a.go', 10, 1]]));
  const { below, unfloored, unfailable } = compare(packages, { 'internal/zero': 0 });
  assert.deepEqual(unfailable, ['internal/zero']);
  assert.deepEqual(below, [], 'a 0 floor is not reported as a drop — it is reported as no floor');
  assert.deepEqual(unfloored, []);
});

test('the update ratchets floors up and refuses to lower one', () => {
  const { packages } = parseProfile(
    profile([
      ['internal/up/a.go', 10, 1],
      ['internal/down/a.go', 10, 0],
    ]),
  );
  const { next, regressions } = nextFloors(packages, { 'internal/up': 40, 'internal/down': 70 });
  assert.equal(next['internal/up'], 99, 'a package that improved raises its floor');
  assert.equal(next['internal/down'], 70, 'a package that dropped keeps the floor it failed');
  assert.deepEqual(
    regressions.map((r) => r.pkg),
    ['internal/down'],
  );
});

test('the update returns an unfloorable package instead of writing a 0 into the ledger', () => {
  const { packages } = parseProfile(
    profile([
      ['internal/untested/a.go', 10, 0],
      ['internal/thin/a.go', 1, 1],
      ['internal/thin/b.go', 199, 0],
      ['internal/real/a.go', 10, 1],
    ]),
  );
  const { next, unfloorable } = nextFloors(packages, {});
  assert.deepEqual(
    unfloorable.map((r) => r.pkg),
    // 0% justifies nothing; 0.5% is inside the slack, so it justifies nothing
    // either — the slack is jitter absorption, not a floor of its own.
    ['internal/thin', 'internal/untested'],
  );
  assert.deepEqual(Object.keys(next), ['internal/real'], 'only the floorable package is written');
});

test('a lane set narrower than the caller requires fails instead of disarming the gate', () => {
  // The dangerous shape: `just chdb-install` no-ops, the profile shrinks, and a
  // gate that "skipped because chdb is missing" reports green forever.
  assert.match(resolveLanes('default', 'default+chdb').err, /disarm/);
  assert.equal(resolveLanes('default+chdb', 'default+chdb').enforce, true);
  assert.equal(resolveLanes('default', '').enforce, false, 'a local run without chdb only reports');
});

test('end to end: a drop below the floor exits 1 and names the package', () => {
  const { dir, profilePath, floorsPath } = fixture([['internal/thin/a.go', 10, 0]], { 'internal/thin': 80 });
  try {
    const { status, out } = run(profilePath, floorsPath);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /internal\/thin: 0\.00% < floor 80%/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a freshly updated ledger passes the comparison on the same profile', () => {
  // The round trip. If this ever fails, every coverage change costs an extra
  // red CI cycle to discover the tool cannot satisfy its own gate.
  const blocks = [
    ['internal/a/one.go', 7, 1],
    ['internal/a/two.go', 3, 0],
    ['internal/b/one.go', 5, 2],
  ];
  const { dir, profilePath, floorsPath } = fixture(blocks, {});
  try {
    assert.equal(run(profilePath, floorsPath, { COVERAGE_UPDATE_FLOORS: '1' }).status, 0);
    const written = JSON.parse(readFileSync(floorsPath, 'utf8'));
    assert.deepEqual(Object.keys(written), ['internal/a', 'internal/b']);
    const { status, out } = run(profilePath, floorsPath);
    assert.equal(status, 0, `gate rejected a freshly updated ledger:\n${out}`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: an update that would lower a floor exits 1 and leaves the ledger alone', () => {
  const { dir, profilePath, floorsPath } = fixture([['internal/thin/a.go', 10, 0]], { 'internal/thin': 80 });
  try {
    const { status, out } = run(profilePath, floorsPath, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /reviewable line/);
    assert.equal(JSON.parse(readFileSync(floorsPath, 'utf8'))['internal/thin'], 80);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a 0 floor on a statement-carrying package exits 1', () => {
  // The negative control for the whole change. Before it, this exact ledger
  // was green, and stayed green with every statement in the package deleted.
  const { dir, profilePath, floorsPath } = fixture([['test/spec/promqlsweep/sweep.go', 61, 1]], {
    'test/spec/promqlsweep': 0,
  });
  try {
    const { status, out } = run(profilePath, floorsPath);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /floor of 0/);
    assert.match(out, /test\/spec\/promqlsweep/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: an update refuses to record a 0 floor and leaves the ledger alone', () => {
  const { dir, profilePath, floorsPath } = fixture([['internal/untested/a.go', 10, 0]], {});
  try {
    const { status, out } = run(profilePath, floorsPath, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /0 is not a floor/);
    assert.match(out, /internal\/untested: 0\.00% of 10 statements/);
    assert.deepEqual(JSON.parse(readFileSync(floorsPath, 'utf8')), {}, 'the ledger gained no entry');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a package with no floor fails rather than being waved through', () => {
  const { dir, profilePath, floorsPath } = fixture([['internal/new/a.go', 10, 1]], {});
  try {
    const { status, out } = run(profilePath, floorsPath);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /no floor/);
    assert.match(out, /internal\/new/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
