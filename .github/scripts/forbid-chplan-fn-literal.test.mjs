import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'forbid-chplan-fn-literal.mjs');

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-chplan-fn-literal-'));
  const git = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  git(['init', '--quiet']);
  writeFileSync(join(dir, 'ok.go'), 'package fixture\n\nvar _ = chplan.FnArray\n');
  git(['add', 'ok.go']);
  git(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'seed']);
  return {
    dir,
    write(path, source) {
      mkdirSync(join(dir, dirname(path)), { recursive: true });
      writeFileSync(join(dir, path), source);
    },
  };
}

function run(cwd) {
  const result = spawnSync(process.execPath, [CLI], { cwd, encoding: 'utf8' });
  return { status: result.status, output: `${result.stdout}${result.stderr}` };
}

test('passes named constants and dynamic resolution-table conversions', () => {
  const f = fixture();
  f.write('dynamic.go', 'package fixture\n\nfunc resolve(value string) { _ = chplan.Fn(value) }\n');
  const result = run(f.dir);
  assert.equal(result.status, 0, result.output);
});

test('rejects a tracked qualified raw Fn literal and names its line', () => {
  const f = fixture();
  f.write('bad.go', 'package fixture\n\nvar _ = chplan.Fn("arrayMap")\n');
  spawnSync('git', ['add', 'bad.go'], { cwd: f.dir });
  const result = run(f.dir);
  assert.notEqual(result.status, 0, result.output);
  assert.match(result.output, /::error::bad\.go:3:/);
});

test('rejects an untracked unqualified raw Fn literal', () => {
  const f = fixture();
  f.write('new.go', 'package fixture\n\nvar _ = Fn(`arrayMap`)\n');
  const result = run(f.dir);
  assert.notEqual(result.status, 0, result.output);
  assert.match(result.output, /new\.go:3/);
});

test('allows the resolution boundary negative test', () => {
  const f = fixture();
  f.write(
    'internal/chsql/fnresolution_test.go',
    'package chsql\n\nvar _ = chplan.Fn("not-a-declared-fn")\n',
  );
  const result = run(f.dir);
  assert.equal(result.status, 0, result.output);
});
