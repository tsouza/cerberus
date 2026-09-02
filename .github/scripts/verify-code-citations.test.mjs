// Suite for verify-code-citations.mjs. Every case drives the real CLI over a
// throwaway git repository, because the gate's scope comes from `git ls-files`
// and a unit test that bypassed that would not be testing what CI runs.
//
// The suite's job is to prove the gate CAN FAIL, one modelled shape at a time —
// a gate whose first green nobody has seen go red reports something other than
// what a reader takes it to mean. Each rejection case is paired with the
// nearest-miss acceptance case, so a regression that widens the gate into
// uselessness (accept everything) and one that narrows it into noise (reject
// everything) both show up here.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'verify-code-citations.mjs');

// The cited file every fixture shares. `guard` appears once; `dup` twice, once
// in each of two funcs, so a citation naming it needs the func scope.
const TARGET = [
  'package fixture',
  '',
  '// numAnchors-1 is described here, in a comment, so the searcher must skip it.',
  'func alpha(n int) int {',
  '\tif n > 0 && n < 10 {',
  '\t\treturn numAnchors - 1',
  '\t}',
  '\tif dupCond != nil {',
  '\t\treturn 0',
  '\t}',
  '',
  '\treturn 1',
  '}',
  '',
  'func beta() error {',
  '\tif dupCond != nil {',
  '\t\treturn nil',
  '\t}',
  '\treturn errQuoted // rejects `"` in a name',
  '}',
  '',
].join('\n');

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'verify-code-citations-'));
  const git = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  git(['init', '--quiet']);
  writeFileSync(join(dir, 'target.go'), TARGET);
  git(['add', 'target.go']);
  git(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'seed']);
  return {
    dir,
    write(path, source) {
      mkdirSync(join(dir, dirname(path)), { recursive: true });
      writeFileSync(join(dir, path), source);
    },
  };
}

function run(cwd, env = {}) {
  const result = spawnSync(process.execPath, [CLI], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, ...env },
  });
  return { status: result.status, output: `${result.stdout}${result.stderr}` };
}

// note — a Go file holding one comment-borne citation.
const note = (citation) => `package fixture\n\n// A mutation note about ${citation} and why it survives.\nvar _ = 0\n`;

test('accepts a construct that resolves to exactly one code line', () => {
  const f = fixture();
  f.write('note.go', note('target.go:`numAnchors - 1`'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a construct scoped to the func that makes it unique', () => {
  const f = fixture();
  f.write('note.go', note('target.go:beta:`dupCond != nil`'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a citation whose path is written relative to the repo root', () => {
  const f = fixture();
  f.write('sub/note.go', note('target.go:`numAnchors - 1`'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a construct written with different whitespace than the source', () => {
  const f = fixture();
  f.write('note.go', note('target.go:`if n > 0 && n < 10 {`'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a construct split across a wrapped comment block', () => {
  const f = fixture();
  f.write('note.go', 'package fixture\n\n// The guard at target.go:`if n > 0 &&\n// n < 10 {` survives because the arms agree.\nvar _ = 0\n');
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('rejects a bare line-number citation — the shape that drifts', () => {
  const f = fixture();
  f.write('note.go', note('target.go:6'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /line-number citation/);
});

test('rejects a line:column citation', () => {
  const f = fixture();
  f.write('note.go', note('target.go:6:12'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /line-number citation/);
});

test('rejects a line-range citation', () => {
  const f = fixture();
  f.write('note.go', note('target.go:5-6'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /line-number citation/);
});

test('rejects a construct that matches nothing — the construct changed', () => {
  const f = fixture();
  f.write('note.go', note('target.go:`numAnchors - 2`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /matches no code line/);
});

test('rejects a construct that matches more than one code line', () => {
  const f = fixture();
  f.write('note.go', note('target.go:`dupCond != nil`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /matches 2 code lines/);
});

test('rejects a construct that lives only in a comment of the cited file', () => {
  // The provable-drift class the issue measured: a citation that resolves to a
  // comment line cannot be naming a construct a mutation operator applies to.
  const f = fixture();
  f.write('note.go', note('target.go:`described here, in a comment`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /matches no code line/);
});

test('rejects a citation whose file is not in this repository', () => {
  const f = fixture();
  f.write('note.go', note('promql/engine.go:`dropName := true`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /does not resolve to a file in this repository/);
});

test('rejects a path that escapes the repository root', () => {
  const f = fixture();
  f.write('note.go', note('../../etc/passwd.go:`root`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /does not resolve to a file in this repository/);
});

test('rejects a scope that is not a top-level func of the cited file', () => {
  const f = fixture();
  f.write('note.go', note('target.go:gamma:`dupCond != nil`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /is not a top-level func/);
});

test('rejects an unterminated construct rather than reading past it', () => {
  const f = fixture();
  f.write('note.go', note('target.go:`numAnchors - 1'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /unterminated construct/);
});

test('rejects an empty construct', () => {
  const f = fixture();
  f.write('note.go', note('target.go:``'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /empty construct/);
});

test('unescapes a construct written inside a Go string literal', () => {
  // A citation in a `t.Fatalf` message has to spell a double quote `\"` for the
  // compiler. The gate compares against a source line that never had the
  // backslash, so it undoes that one escape — otherwise every string-borne
  // citation of a quote-carrying construct would be unfixable.
  const f = fixture();
  f.write('note.go', 'package fixture\n\nfunc f() { panic("mutant at target.go:`errQuoted // rejects `+"`"+`\\"`+"`"+`` in a name`") }\n');
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('leaves prose and the rejection-parity site-id format alone', () => {
  // `.go:` followed by anything that is not a digit or a construct carries no
  // address, so nothing can drift. The catalogue's `path.go:func#hash` site ids
  // are a data format, and catalogue_test.go builds deliberately fabricated
  // ones that no resolver should be asked to resolve.
  const f = fixture();
  f.write(
    'note.go',
    'package fixture\n\n// See target.go: the alpha guard, and target.go:alpha for the func.\n' +
      'var site = "internal/promql/a.go:fnA#0000aaaa"\n',
  );
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('fails when the pathspec matches no Go file at all', () => {
  // A gate that passes because it found nothing to check reports the same green
  // as a satisfied one.
  const f = fixture();
  const r = run(f.dir, { PATHSPECS: 'no/such/dir/*.go' });
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /matched no Go file/);
});

test('reports every violation in one run rather than stopping at the first', () => {
  const f = fixture();
  f.write('a.go', note('target.go:6'));
  f.write('b.go', note('target.go:`numAnchors - 2`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /2 source citation\(s\) do not resolve/);
});
