// coverage-chdb.test.mjs — node:test guard for the `coverage-chdb` recipe's
// extracted logic (CLAUDE.md invariant 15, tsouza/cerberus#3113): the
// CHDB_INSTALL_PATH/libchdb.so branch (missing env, graceful skip, and the
// COVERAGE_REQUIRE_LANES fail-closed tail reached either way), the
// `go list`-derived -coverpkg join, the main sweep's argv shape and its
// stdout line filter, and exit-code propagation on a failed `go test` — all
// against a fake `go` executable, so no real chDB or libchdb.so is needed.
//
// mainSweepArgv()'s shape (composite tags, no -skip, PERF_SHARD_COUNT tied
// to RATCHET_FANOUT) is pinned by perf-coverage-fanout.test.mjs instead —
// see that file's own header — to keep the two coverage-lane scripts' cross
// checks together with the fan-out they both touch.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { main, filterCoverpkgLine } from './coverage-chdb.mjs';

test('filterCoverpkgLine rewrites only the -coverpkg echo tail, leaving everything else untouched', () => {
  assert.equal(
    filterCoverpkgLine(
      'ok  \tgithub.com/tsouza/cerberus/internal/foo\t0.5s\tcoverage: 42.0% of statements in github.com/tsouza/cerberus/internal/foo,github.com/tsouza/cerberus/internal/bar',
    ),
    'ok  \tgithub.com/tsouza/cerberus/internal/foo\t0.5s\tcoverage: 42.0% of statements',
  );
  assert.equal(filterCoverpkgLine('--- FAIL: TestSomething (0.01s)'), '--- FAIL: TestSomething (0.01s)');
  assert.equal(filterCoverpkgLine(''), '');
});

function tmpDir() {
  return mkdtempSync(join(tmpdir(), 'coverage-chdb-test-'));
}

function floorlessEnv(overrides = {}) {
  return { ...process.env, ...overrides };
}

test('missing CHDB_INSTALL_PATH fails closed without touching the filesystem', async () => {
  const dir = tmpDir();
  try {
    const status = await main({ cwd: dir, env: floorlessEnv() });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('libchdb.so absent: graceful skip, exit 0, no COVERAGE_REQUIRE_LANES set', async () => {
  const dir = tmpDir();
  try {
    const status = await main({ cwd: dir, env: floorlessEnv({ CHDB_INSTALL_PATH: join(dir, 'no-such-libchdb.so') }) });
    assert.equal(status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('libchdb.so absent + COVERAGE_REQUIRE_LANES=default+chdb + no cover-chdb.out: fails closed', async () => {
  const dir = tmpDir();
  try {
    const status = await main({
      cwd: dir,
      env: floorlessEnv({ CHDB_INSTALL_PATH: join(dir, 'no-such-libchdb.so'), COVERAGE_REQUIRE_LANES: 'default+chdb' }),
    });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('libchdb.so absent + COVERAGE_REQUIRE_LANES=default+chdb + a cover-chdb.out already present: the tail only checks the file, not why it is there', async () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover-chdb.out'), 'mode: set\n');
    const status = await main({
      cwd: dir,
      env: floorlessEnv({ CHDB_INSTALL_PATH: join(dir, 'no-such-libchdb.so'), COVERAGE_REQUIRE_LANES: 'default+chdb' }),
    });
    assert.equal(status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('libchdb.so present but CERBERUS_RAPID_SEED unset: fails closed before running go at all', async () => {
  const dir = tmpDir();
  const libchdb = join(dir, 'libchdb.so');
  writeFileSync(libchdb, '');
  try {
    const status = await main({ cwd: dir, env: floorlessEnv({ CHDB_INSTALL_PATH: libchdb }), go: '/nonexistent/should-not-run' });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// fakeGo emulates the two `go` subcommands this script actually calls:
//   list -tags <tags> ./...      -> prints two fake package names
//   test ... -coverprofile <f>   -> writes a fake profile at <f> and exits
//                                   with $FAKE_GO_TEST_EXIT (default 0),
//                                   echoing a -coverpkg-shaped summary line
//                                   so the streaming filter has something
//                                   real to rewrite.
// perf-coverage-fanout.mjs's own legs reach this SAME fake go when GO is
// forwarded into the fan-out's env (see the "runs the real fan-out" test
// below) — its legs also invoke `go test ... -coverprofile <f>`, so no
// special-casing is needed.
function writeFakeGo(dir) {
  const path = join(dir, 'fake-go.sh');
  writeFileSync(
    path,
    [
      '#!/usr/bin/env bash',
      'set -eu',
      'case "$1" in',
      '  list)',
      '    echo "github.com/tsouza/cerberus/internal/fakepkg1"',
      '    echo "github.com/tsouza/cerberus/internal/fakepkg2"',
      '    ;;',
      '  test)',
      '    profile=""',
      '    prev=""',
      '    for arg in "$@"; do',
      '      if [ "$prev" = "-coverprofile" ]; then profile="$arg"; fi',
      '      prev="$arg"',
      '    done',
      '    echo "mode: set" > "$profile"',
      '    echo "ok  	$profile	0.1s	coverage: 42.0% of statements in github.com/tsouza/cerberus/internal/fakepkg1,github.com/tsouza/cerberus/internal/fakepkg2"',
      '    exit "${FAKE_GO_TEST_EXIT:-0}"',
      '    ;;',
      '  *)',
      '    echo "fake-go: unsupported subcommand $1" >&2',
      '    exit 1',
      '    ;;',
      'esac',
      '',
    ].join('\n'),
  );
  chmodSync(path, 0o755);
  return path;
}

// collectingStream is the `stdout` test seam main() writes filtered lines
// to — an injected sink rather than a monkey-patch of the real
// process.stdout, which would race other tests' own reporter output under
// the test runner's default per-file concurrency.
function collectingStream() {
  const chunks = [];
  return {
    write: (chunk) => {
      chunks.push(chunk.toString());
      return true;
    },
    get output() {
      return chunks.join('');
    },
  };
}

test('libchdb.so present: joins go list output into -coverpkg, writes the profile, and filters the coverpkg echo off stdout', async () => {
  const dir = tmpDir();
  const libchdb = join(dir, 'libchdb.so');
  writeFileSync(libchdb, '');
  const go = writeFakeGo(dir);
  const stdout = collectingStream();
  try {
    const status = await main({
      cwd: dir,
      go,
      stdout,
      env: floorlessEnv({ CHDB_INSTALL_PATH: libchdb, CERBERUS_RAPID_SEED: '20260903', SKIP_RATCHET_FANOUT: '1' }),
    });
    assert.equal(status, 0);
    assert.equal(readFileSync(join(dir, 'cover-chdb.out'), 'utf8'), 'mode: set\n');
    assert.match(stdout.output, /coverage: 42\.0% of statements\n/, 'the -coverpkg tail must be rewritten');
    assert.ok(!stdout.output.includes('fakepkg1,github.com'), 'the raw package-list echo must not reach stdout unfiltered');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a failing go test propagates its real exit code without running the fan-out', async () => {
  const dir = tmpDir();
  const libchdb = join(dir, 'libchdb.so');
  writeFileSync(libchdb, '');
  const go = writeFakeGo(dir);
  try {
    const status = await main({
      cwd: dir,
      go,
      stdout: collectingStream(),
      env: floorlessEnv({
        CHDB_INSTALL_PATH: libchdb,
        CERBERUS_RAPID_SEED: '20260903',
        SKIP_RATCHET_FANOUT: '1',
        FAKE_GO_TEST_EXIT: '3',
      }),
    });
    assert.equal(status, 3);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('SKIP_RATCHET_FANOUT unset: the real fan-out runs and produces the extra shard profiles', async () => {
  const dir = tmpDir();
  const libchdb = join(dir, 'libchdb.so');
  writeFileSync(libchdb, '');
  const go = writeFakeGo(dir);
  try {
    const status = await main({
      cwd: dir,
      go,
      stdout: collectingStream(),
      env: floorlessEnv({ CHDB_INSTALL_PATH: libchdb, CERBERUS_RAPID_SEED: '20260903', GO: go }),
    });
    assert.equal(status, 0);
    // RATCHET_FANOUT=3: the main sweep above covers shard 1, this fan-out
    // covers shards 2 and 3 — proving main() really invoked
    // perf-coverage-fanout.mjs (with GO/TAGS/COVERPKG wired through), not
    // just logged that it would.
    assert.equal(readFileSync(join(dir, 'cover-chdb-ratchet-2.out'), 'utf8'), 'mode: set\n');
    assert.equal(readFileSync(join(dir, 'cover-chdb-ratchet-3.out'), 'utf8'), 'mode: set\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
