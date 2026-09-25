import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import { classify, diffFiles, moduleDirs, parseBuildTags, passes, run } from './go-fix-check.mjs';

const config = `run:
  timeout: 18m
  # comment inside the block's preamble
  build-tags:
    - agpl_oracle
    # a comment between entries is skipped, not a terminator
    - chdb

linters:
  default: none
`;

test('parseBuildTags reads the run.build-tags list and stops at the next key', () => {
  assert.deepEqual(parseBuildTags(config), ['agpl_oracle', 'chdb']);
});

test('parseBuildTags rejects a config with no build-tags block', () => {
  assert.throws(() => parseBuildTags('run:\n  timeout: 1m\n'), /no `build-tags:` block/);
});

test('parseBuildTags rejects an empty build-tags block', () => {
  assert.throws(() => parseBuildTags('run:\n  build-tags:\nlinters:\n'), /empty/);
});

test('the real .golangci.yml parses to a non-empty, sorted union', () => {
  const tags = parseBuildTags(readFileSync('.golangci.yml', 'utf8'));
  assert.ok(tags.length > 0);
  assert.deepEqual([...tags].sort(), tags);
});

test('diffFiles lists every rewritten file in order', () => {
  const diff = [
    '--- /repo/a.go (old)',
    '+++ /repo/a.go (new)',
    '@@ -1 +1 @@',
    '--- /repo/dir/b.go (old)',
    '+++ /repo/dir/b.go (new)',
  ].join('\n');
  assert.deepEqual(diffFiles(diff), ['/repo/a.go', '/repo/dir/b.go']);
  assert.deepEqual(diffFiles(''), []);
});

test('passes cover the tag union and the untagged build', () => {
  assert.deepEqual(passes(['a', 'b']), [
    { name: 'tagged', args: ['-tags=a,b'] },
    { name: 'untagged', args: [] },
  ]);
});

test('moduleDirs lists the root module first, then each nested one', () => {
  assert.deepEqual(moduleDirs('test/oracle/go.mod\0go.mod\0bench/histogram/go.mod\0'), [
    '.',
    'bench/histogram',
    'test/oracle',
  ]);
  assert.deepEqual(moduleDirs(''), []);
});

// go1.26 `go fix -diff` exits 1 with the diff on stdout and nothing on stderr
// when it has a rewrite to report, and 1 with the error on stderr and nothing
// on stdout when a package fails to load.
const pendingRes = (dir) => ({ status: 1, stdout: `--- ${dir}/x.go (old)\n+++ ${dir}/x.go (new)\n`, stderr: '' });
const brokenRes = { status: 1, stdout: '', stderr: "# x\nfix: ./y.go:2:9: expected ')', found '{'\n" };

test('classify separates a pending rewrite from a go fix failure', () => {
  assert.equal(classify('check', { status: 0, stdout: '', stderr: '' }), 'clean');
  assert.equal(classify('check', pendingRes('/r')), 'pending');
  assert.equal(classify('check', brokenRes), 'failed');
  assert.equal(classify('check', { ...pendingRes('/r'), stderr: 'load error' }), 'failed');
  assert.equal(classify('check', { ...pendingRes('/r'), status: 2 }), 'failed');
  assert.equal(classify('apply', { status: 0, stdout: 'ignored', stderr: '' }), 'clean');
  assert.equal(classify('apply', pendingRes('/r')), 'failed');
});

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'go-fix-check-'));
  const cfg = join(dir, '.golangci.yml');
  writeFileSync(cfg, config);
  return { dir, cfg };
}

// gitAnd — a runner answering `git ls-files` with the given go.mod list and
// delegating every `go` call to goRunner.
function gitAnd(goMods, goRunner) {
  return (cmd, args, opts) =>
    cmd === 'git' ? { status: 0, stdout: goMods.map((m) => `${m}\0`).join(''), stderr: '' } : goRunner(cmd, args, opts);
}

test('run passes when every module is clean in both passes', () => {
  const { dir, cfg } = fixture();
  const calls = [];
  const runner = gitAnd(['go.mod', 'nested/go.mod'], (cmd, args, opts) => {
    calls.push([opts.cwd, cmd, ...args]);
    return { status: 0, stdout: '', stderr: '' };
  });
  assert.equal(run({ configPath: cfg, root: dir, runner }), 0);
  assert.deepEqual(calls, [
    [join(dir, '.'), 'go', 'fix', '-diff', '-tags=agpl_oracle,chdb', './...'],
    [join(dir, '.'), 'go', 'fix', '-diff', './...'],
    [join(dir, 'nested'), 'go', 'fix', '-diff', '-tags=agpl_oracle,chdb', './...'],
    [join(dir, 'nested'), 'go', 'fix', '-diff', './...'],
  ]);
});

test('run fails when either pass reports a pending rewrite', () => {
  const { dir, cfg } = fixture();
  const runner = gitAnd(['go.mod'], (_cmd, args) =>
    args.includes('-tags=agpl_oracle,chdb') ? { status: 0, stdout: '', stderr: '' } : pendingRes(dir),
  );
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('run fails on a pending rewrite in a nested module only', () => {
  const { dir, cfg } = fixture();
  const runner = gitAnd(['go.mod', 'nested/go.mod'], (_cmd, _args, opts) =>
    opts.cwd === join(dir, 'nested') ? pendingRes(join(dir, 'nested')) : { status: 0, stdout: '', stderr: '' },
  );
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('run fails when go fix itself fails', () => {
  const { dir, cfg } = fixture();
  const runner = gitAnd(['go.mod'], () => brokenRes);
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('run fails when no go.mod is tracked', () => {
  const { dir, cfg } = fixture();
  const runner = gitAnd([], () => ({ status: 0, stdout: '', stderr: '' }));
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('apply mode rewrites in place without -diff', () => {
  const { dir, cfg } = fixture();
  const calls = [];
  const runner = gitAnd(['go.mod'], (_cmd, args) => {
    calls.push(args);
    return { status: 0, stdout: 'ignored', stderr: '' };
  });
  assert.equal(run({ mode: 'apply', configPath: cfg, root: dir, runner }), 0);
  assert.equal(calls.length, 2);
  assert.ok(calls.every((a) => !a.includes('-diff')));
});
