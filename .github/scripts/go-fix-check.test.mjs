import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import { diffFiles, parseBuildTags, passes, run } from './go-fix-check.mjs';

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

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'go-fix-check-'));
  const cfg = join(dir, '.golangci.yml');
  writeFileSync(cfg, config);
  return { dir, cfg };
}

test('run passes when both passes produce no diff', () => {
  const { dir, cfg } = fixture();
  const calls = [];
  const runner = (cmd, args) => {
    calls.push([cmd, ...args]);
    return { status: 0, stdout: '', stderr: '' };
  };
  assert.equal(run({ configPath: cfg, root: dir, runner }), 0);
  assert.deepEqual(calls, [
    ['go', 'fix', '-diff', '-tags=agpl_oracle,chdb', './...'],
    ['go', 'fix', '-diff', './...'],
  ]);
});

test('run fails when either pass reports a pending rewrite', () => {
  const { dir, cfg } = fixture();
  const runner = (_cmd, args) => ({
    status: 0,
    stdout: args.includes('-tags=agpl_oracle,chdb') ? '' : `--- ${dir}/x.go (old)\n+++ ${dir}/x.go (new)\n`,
    stderr: '',
  });
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('run fails when go fix itself fails', () => {
  const { dir, cfg } = fixture();
  const runner = () => ({ status: 1, stdout: '', stderr: 'build constraints exclude all Go files' });
  assert.equal(run({ configPath: cfg, root: dir, runner }), 1);
});

test('apply mode rewrites in place without -diff', () => {
  const { dir, cfg } = fixture();
  const calls = [];
  const runner = (cmd, args) => {
    calls.push(args);
    return { status: 0, stdout: 'ignored', stderr: '' };
  };
  assert.equal(run({ mode: 'apply', configPath: cfg, root: dir, runner }), 0);
  assert.ok(calls.every((a) => !a.includes('-diff')));
});
