// coverage-summary.test.mjs — pins for the coverage floor gate.
//
// The failure this gate exists to prevent is a report that cannot fail, so the
// load-bearing cases here are the refusals: a drop below a floor, a package
// with no floor, a floor with no package, and an update that would lower a
// floor. A green assertion on the happy path proves nothing on its own — the
// awk this replaced was green on every input ever handed to it.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import {
  compare,
  floorFor,
  laneRecordPath,
  nextFloors,
  packageKeySegments,
  packageOf,
  parseProfile,
  profileDigest,
  resolveLanes,
  resolveUpdateLanes,
  rows,
} from './coverage-summary.mjs';
import { loadShardedMap, writeShardedMap } from './lib/sharded-json.mjs';

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

// laneRecord writes the provenance stamp `just coverage-merge` leaves beside a
// profile: the lane set that produced it, bound to that profile's own bytes.
// `lanes` of null writes no record at all — the "provenance unknown" case.
function laneRecord(profilePath, lanes) {
  if (lanes === null) return;
  const text = readFileSync(profilePath, 'utf8');
  writeFileSync(
    laneRecordPath(profilePath),
    `${JSON.stringify({ lanes, profileSha256: profileDigest(text) }, null, 2)}\n`,
  );
}

// fixture lays down a profile and a sharded floor ledger in a scratch
// directory — one shard file per package, matching the on-disk shape
// coverage-summary.mjs now reads/writes (see lib/sharded-json.mjs).
//
// It also stamps the both-lane record, because a fixture stands in for a
// COMPLETE profile: a test that wants the narrow or missing-provenance case
// says so with `lanes`, so those cases are visible at the call site rather than
// being whatever the helper happened to omit.
function fixture(blocks, floors, lanes = 'default+chdb') {
  const dir = mkdtempSync(path.join(tmpdir(), 'coverage-floor-'));
  const profilePath = path.join(dir, 'cover-merged.out');
  const floorsDir = path.join(dir, 'coverage-floor');
  writeFileSync(profilePath, profile(blocks));
  laneRecord(profilePath, lanes);
  if (floors !== undefined) writeShardedMap(floorsDir, floors, packageKeySegments);
  return { dir, profilePath, floorsDir };
}

function run(profilePath, floorsDir, env = {}) {
  const res = spawnSync(process.execPath, [SCRIPT], {
    encoding: 'utf8',
    env: {
      ...process.env,
      GITHUB_STEP_SUMMARY: '',
      COVERAGE_PROFILE: profilePath,
      COVERAGE_FLOORS: floorsDir,
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
  const { dir, profilePath, floorsDir } = fixture([['internal/thin/a.go', 10, 0]], { 'internal/thin': 80 });
  try {
    const { status, out } = run(profilePath, floorsDir);
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
  const { dir, profilePath, floorsDir } = fixture(blocks, {});
  try {
    assert.equal(run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' }).status, 0);
    const written = loadShardedMap(floorsDir);
    assert.deepEqual(Object.keys(written), ['internal/a', 'internal/b']);
    const { status, out } = run(profilePath, floorsDir);
    assert.equal(status, 0, `gate rejected a freshly updated ledger:\n${out}`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: an update that would lower a floor exits 1 and leaves the ledger alone', () => {
  const { dir, profilePath, floorsDir } = fixture([['internal/thin/a.go', 10, 0]], { 'internal/thin': 80 });
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /reviewable line/);
    assert.equal(loadShardedMap(floorsDir)['internal/thin'], 80);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a 0 floor on a statement-carrying package exits 1', () => {
  // The negative control for the whole change. Before it, this exact ledger
  // was green, and stayed green with every statement in the package deleted.
  const { dir, profilePath, floorsDir } = fixture([['test/spec/promqlsweep/sweep.go', 61, 1]], {
    'test/spec/promqlsweep': 0,
  });
  try {
    const { status, out } = run(profilePath, floorsDir);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /floor of 0/);
    assert.match(out, /test\/spec\/promqlsweep/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: an update refuses to record a 0 floor and leaves the ledger alone', () => {
  const { dir, profilePath, floorsDir } = fixture([['internal/untested/a.go', 10, 0]], {});
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /0 is not a floor/);
    assert.match(out, /internal\/untested: 0\.00% of 10 statements/);
    assert.deepEqual(loadShardedMap(floorsDir), {}, 'the ledger gained no entry');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a package with no floor fails rather than being waved through', () => {
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {});
  try {
    const { status, out } = run(profilePath, floorsDir);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /no floor/);
    assert.match(out, /internal\/new/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the update path refuses a profile whose lane provenance is unknown', () => {
  // The guard this replaces asked whether libchdb.so was on the machine, which
  // is a fact about the machine. With no record beside it, a profile could have
  // come from anywhere — including a CI run whose chdb lane never produced one.
  const err = resolveUpdateLanes(null, 'mode: set\n', 'cover-merged.out.lanes.json');
  assert.match(err.err, /does not exist/);
  assert.match(err.err, /cannot be established/);
});

test('the update path refuses a default-only profile and accepts a both-lane one', () => {
  const text = profile([['internal/a/one.go', 4, 1]]);
  const narrow = JSON.stringify({ lanes: 'default', profileSha256: profileDigest(text) });
  const full = JSON.stringify({ lanes: 'default+chdb', profileSha256: profileDigest(text) });
  assert.match(resolveUpdateLanes(narrow, text, 'r.json').err, /'default' lane set, not 'default\+chdb'/);
  assert.equal(resolveUpdateLanes(full, text, 'r.json').lanes, 'default+chdb');
});

test('a lane record that describes some other profile is not evidence about this one', () => {
  // The reason the record carries a digest at all: a stale record sitting in
  // the directory from an earlier local run would otherwise vouch for a profile
  // downloaded on top of it, which is the same "fact about the directory, not
  // about the profile" mistake as testing for libchdb.so.
  const text = profile([['internal/a/one.go', 4, 1]]);
  const other = JSON.stringify({ lanes: 'default+chdb', profileSha256: profileDigest('mode: set\n') });
  assert.match(resolveUpdateLanes(other, text, 'r.json').err, /describes a different profile/);
});

test('a lane record that is not JSON, or not shaped like a record, proves nothing', () => {
  const text = profile([['internal/a/one.go', 4, 1]]);
  assert.match(resolveUpdateLanes('not json', text, 'r.json').err, /not readable as JSON/);
  assert.match(resolveUpdateLanes('{"lanes":"default+chdb"}', text, 'r.json').err, /is not a lane record/);
  assert.match(resolveUpdateLanes('[]', text, 'r.json').err, /is not a lane record/);
});

test('end to end: the update refuses a default-only profile and writes no floor', () => {
  // The defect in full: a NEW package enrolled from a default-tag-only profile
  // gets a positive-but-too-low floor that passes enrollment, passes the gate,
  // and is never corrected — the ratchet only raises, and nothing raises it.
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {}, 'default');
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /'default' lane set, not 'default\+chdb'/);
    assert.deepEqual(loadShardedMap(floorsDir), {}, 'the ledger gained no entry');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: the update refuses a profile with no lane record at all', () => {
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {}, null);
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /does not exist/);
    assert.deepEqual(loadShardedMap(floorsDir), {}, 'the ledger gained no entry');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: the update accepts the both-lane profile and enrolls the package', () => {
  // The positive control for the two refusals above: same package, same
  // measurement, provenance the only difference.
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {});
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 0, `expected acceptance; output was:\n${out}`);
    assert.equal(loadShardedMap(floorsDir)['internal/new'], 99);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: the compare path stamps the record the update path reads', () => {
  // The handoff `just coverage-merge` -> `just update-coverage-floor` depends
  // on. It is stamped even when the comparison itself fails, because a red
  // floor gate is exactly when a package needs enrolling from that profile.
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {}, null);
  try {
    assert.equal(existsSync(laneRecordPath(profilePath)), false);
    const { status } = run(profilePath, floorsDir, { COVERAGE_LANES: 'default+chdb' });
    assert.equal(status, 1, 'the unfloored package still fails the comparison');
    const record = JSON.parse(readFileSync(laneRecordPath(profilePath), 'utf8'));
    assert.equal(record.lanes, 'default+chdb');
    assert.equal(record.profileSha256, profileDigest(readFileSync(profilePath, 'utf8')));
    // ...and the update path now accepts what the compare path stamped.
    assert.equal(run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' }).status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: a narrow compare path stamps a narrow record, which the update refuses', () => {
  // The full local shape of the defect: no libchdb.so, so `just coverage-merge`
  // produces a default-only profile, reports (rather than enforces) — and the
  // record it leaves behind is what stops those floors being written.
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {}, null);
  try {
    assert.equal(run(profilePath, floorsDir, { COVERAGE_LANES: 'default' }).status, 0, 'a narrow run only reports');
    assert.equal(JSON.parse(readFileSync(laneRecordPath(profilePath), 'utf8')).lanes, 'default');
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.deepEqual(loadShardedMap(floorsDir), {}, 'the ledger gained no entry');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: the lane guard runs BEFORE the ratchet, and neither writes on a regression', () => {
  // The pre-existing fail-closed property, re-pinned now that a second guard
  // sits in front of it: a package below its committed floor still aborts the
  // update with the ledger untouched, on a profile whose provenance is fine.
  const { dir, profilePath, floorsDir } = fixture([['internal/thin/a.go', 10, 0]], { 'internal/thin': 80 });
  try {
    const { status, out } = run(profilePath, floorsDir, { COVERAGE_UPDATE_FLOORS: '1' });
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /reviewable line/, 'the ratchet, not the lane guard, is what refused');
    assert.equal(loadShardedMap(floorsDir)['internal/thin'], 80);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('end to end: COVERAGE_LANES in the environment cannot talk the update path round', () => {
  // The obvious bypass, and the reason the update path reads a record rather
  // than an environment variable: whoever runs the update supplies the
  // environment, so an env var is a claim by the caller, not evidence about the
  // profile. (`run` already sets COVERAGE_LANES=default+chdb for every case
  // here — this names the property so it cannot be lost by changing a default.)
  const { dir, profilePath, floorsDir } = fixture([['internal/new/a.go', 10, 1]], {}, 'default');
  try {
    const { status } = run(profilePath, floorsDir, {
      COVERAGE_UPDATE_FLOORS: '1',
      COVERAGE_LANES: 'default+chdb',
      COVERAGE_REQUIRE_LANES: 'default+chdb',
    });
    assert.equal(status, 1, 'the environment claimed both lanes; the record on the profile did not');
    assert.deepEqual(loadShardedMap(floorsDir), {});
    // Nor does the update path rewrite the record it just refused.
    assert.equal(JSON.parse(readFileSync(laneRecordPath(profilePath), 'utf8')).lanes, 'default');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
