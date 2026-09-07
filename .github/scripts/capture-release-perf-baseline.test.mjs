// capture-release-perf-baseline.test.mjs — node:test guard for the
// `capture-release-perf-baseline` recipe's extracted logic. Exercises the
// real script against a real git repository in a temp dir (both `--ref`
// and working-tree capture modes touch git directly, so mocking it would
// test the mock rather than the behaviour), never against libchdb — this
// script does no profiling, only a copy.

import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { run } from './capture-release-perf-baseline.mjs';

// scratchRepo builds a tiny throwaway git repo with a cardinality-baseline
// tree at HEAD, tags it, then adds a SECOND commit that changes one shard —
// so a test can capture either "the tag" or "the working tree" and see them
// differ, exactly like the real repo's v1.19.0-vs-HEAD situation.
function scratchRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'capture-release-perf-baseline-test-'));
  const run = (cmd, args) => execFileSync(cmd, args, { cwd: dir, encoding: 'utf8' });
  run('git', ['init', '-q']);
  run('git', ['config', 'user.email', 'test@example.com']);
  run('git', ['config', 'user.name', 'test']);
  mkdirSync(join(dir, 'test/perf/cardinality-baseline/promql'), { recursive: true });
  writeFileSync(join(dir, 'test/perf/cardinality-baseline/promql/rate_basic.json'), '{"fixture":"promql/rate_basic","scan_rows":1}\n');
  run('git', ['add', '-A']);
  run('git', ['commit', '-q', '-m', 'v1.0.0 state']);
  run('git', ['tag', '-a', 'v1.0.0', '-m', 'v1.0.0']);
  // A second commit changes the shard — the working tree now disagrees with
  // the tag, which is exactly the property the two capture modes must respect.
  writeFileSync(join(dir, 'test/perf/cardinality-baseline/promql/rate_basic.json'), '{"fixture":"promql/rate_basic","scan_rows":2}\n');
  run('git', ['add', '-A']);
  run('git', ['commit', '-q', '-m', 'post-release change']);
  return dir;
}

function withCwd(dir, fn) {
  const prev = process.cwd();
  process.chdir(dir);
  try {
    return fn();
  } finally {
    process.chdir(prev);
  }
}

test('captures from a git ref (tag), not the working tree', () => {
  const dir = scratchRepo();
  try {
    withCwd(dir, () => run(['--version', '1.0.0', '--ref', 'v1.0.0']));
    const captured = readFileSync(join(dir, 'test/perf/release-baseline/1.0.0/cardinality/promql/rate_basic.json'), 'utf8');
    assert.match(captured, /"scan_rows":1/, 'must reflect the TAG state, not the later working-tree change');
    const meta = JSON.parse(readFileSync(join(dir, 'test/perf/release-baseline/1.0.0/meta.json'), 'utf8'));
    assert.equal(meta.version, '1.0.0');
    assert.equal(meta.source_ref, 'v1.0.0');
    assert.equal(meta.shard_count, 1);
    // The commit the TAG points at, not the tag object's own sha (an
    // annotated tag's rev-parse without `^{commit}` returns the wrong
    // thing — this is the exact bug caught and fixed while building this).
    const commitSha = execFileSync('git', ['rev-parse', 'v1.0.0^{commit}'], { cwd: dir, encoding: 'utf8' }).trim();
    assert.equal(meta.source_commit, commitSha);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('captures from the working tree when no --ref is given', () => {
  const dir = scratchRepo();
  try {
    withCwd(dir, () => run(['--version', '1.0.0']));
    const captured = readFileSync(join(dir, 'test/perf/release-baseline/1.0.0/cardinality/promql/rate_basic.json'), 'utf8');
    assert.match(captured, /"scan_rows":2/, 'must reflect the CURRENT working-tree state, not the older tag');
    const meta = JSON.parse(readFileSync(join(dir, 'test/perf/release-baseline/1.0.0/meta.json'), 'utf8'));
    assert.equal(meta.source_ref, '(working tree)');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('refuses to overwrite an existing capture', () => {
  const dir = scratchRepo();
  try {
    withCwd(dir, () => run(['--version', '1.0.0', '--ref', 'v1.0.0']));
    let threw = false;
    const realExit = process.exit;
    process.exit = (code) => {
      threw = true;
      throw new Error(`exit(${code})`);
    };
    try {
      withCwd(dir, () => run(['--version', '1.0.0', '--ref', 'v1.0.0']));
    } catch {
      // expected — process.exit was stubbed to throw
    } finally {
      process.exit = realExit;
    }
    assert.ok(threw, 'a second capture at the same version must refuse, not silently overwrite');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a version with no shards is refused and leaves nothing committed', () => {
  const dir = mkdtempSync(join(tmpdir(), 'capture-release-perf-baseline-test-'));
  try {
    const gitRun = (args) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' });
    gitRun(['init', '-q']);
    gitRun(['config', 'user.email', 'test@example.com']);
    gitRun(['config', 'user.name', 'test']);
    mkdirSync(join(dir, 'test/perf/cardinality-baseline'), { recursive: true });
    writeFileSync(join(dir, 'test/perf/cardinality-baseline/.gitkeep'), '');
    gitRun(['add', '-A']);
    gitRun(['commit', '-q', '-m', 'empty baseline']);

    let threw = false;
    const realExit = process.exit;
    process.exit = (code) => {
      threw = true;
      throw new Error(`exit(${code})`);
    };
    try {
      withCwd(dir, () => run(['--version', '1.0.0']));
    } catch {
      // expected
    } finally {
      process.exit = realExit;
    }
    assert.ok(threw, 'zero .json shards must refuse rather than commit an empty release baseline');
    assert.ok(!existsSync(join(dir, 'test/perf/release-baseline/1.0.0')), 'the refused capture must clean up after itself');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('missing --version is refused', () => {
  let threw = false;
  const realExit = process.exit;
  process.exit = (code) => {
    threw = true;
    throw new Error(`exit(${code})`);
  };
  try {
    run([]);
  } catch {
    // expected
  } finally {
    process.exit = realExit;
  }
  assert.ok(threw);
});
