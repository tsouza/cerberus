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
  '\tif ns%stepNS != 0 {',
  '\t\treturn errModulo',
  '\t}',
  '\treturn errQuoted // rejects `"` in a name',
  '}',
  '',
  'var outsideAnyFunc = dupCond',
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

test('rejects a construct that sits outside the named func, after its closing brace', () => {
  // A func scope ends at its closing brace, not at the next declaration —
  // otherwise a package-level var between two funcs would be searchable under
  // the name of the func above it.
  const f = fixture();
  f.write('note.go', note('target.go:beta:`outsideAnyFunc`'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /matches no code line/);
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

test('unescapes a %% verb written inside a Go format string', () => {
  // `go vet` rejects a lone `%` in a format string, so a construct containing a
  // modulo has to be spelled `%%` there. Without the unescape it would be
  // uncitable from the `t.Fatalf` messages that carry most of these notes.
  const f = fixture();
  f.write('note.go', 'package fixture\n\nfunc f() { panic("mutant at target.go:`ns%%stepNS != 0`") }\n');
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
  assert.match(r.output, /2 place\(s\) where a note names code that cannot be verified/);
});

// ---------------------------------------------------------------------------
// Shape 2 (#2964): a line address written as prose. Each rejection is paired
// with the nearest-miss acceptance, because the whole question here was which
// spellings carry an address and which carry data.
// ---------------------------------------------------------------------------

// prose — a Go file whose doc comment is one line of note text.
const prose = (text) => `package fixture\n\n// ${text}\nvar _ = 0\n`;

test('rejects a bare prose line address', () => {
  const f = fixture();
  f.write('note.go', prose('The guard on line 613 survives because the arms agree.'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `line 613`/);
});

test('rejects a prose line address regardless of case', () => {
  const f = fixture();
  f.write('note.go', prose('Line 116 INVERT_LOGICAL flips the conjunction.'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `Line 116`/);
});

test('rejects a prose line:column address and reports it whole', () => {
  // The worst shape the issue found: an address with no filename at all, which
  // is strictly less verifiable than the `file.go:613` form already rejected.
  const f = fixture();
  f.write('note.go', prose('The `||` flipped to `&&` (line 92:20) takes the nil arm.'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `line 92:20`/);
});

test('rejects a prose line-range address', () => {
  const f = fixture();
  f.write('note.go', prose('Applies the extrapolation threshold (functions.go lines 273-276).'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `lines 273-276`/);
});

test('rejects an abbreviated prose column address', () => {
  const f = fixture();
  f.write('note.go', prose('INVERT_LOGICAL at col 14 flips the first `&&` to `||`.'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `col 14`/);
});

test('rejects an @-marked line:column address', () => {
  const f = fixture();
  f.write('note.go', prose('Negating `spansTable == ""` (@97:23) would wave it through.'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `@97:23`/);
});

test('rejects a prose line address inside a testing report message', () => {
  // A third of these live in `t.Errorf` format strings rather than comments,
  // so a comment-only scan would miss them.
  const f = fixture();
  f.write(
    'note.go',
    'package fixture\n\nfunc f(t T) {\n\tt.Errorf("outer anchor grid must be range(4) — line 408 arithmetic flipped")\n}\n',
  );
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /prose line address `line 408`/);
});

test('accepts a spelled-out column, which names a data column and not a source line', () => {
  // Measured 10 of 10 non-addresses on the tree: ClickHouse result columns and
  // a regex's column 0. Rejecting it would force a rewrite of correct prose.
  const f = fixture();
  f.write('note.go', prose('The scan binds column 1 into a String and column 2 into the label map.'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a digit-prefixed @, which is a fixture timestamp and not an address', () => {
  const f = fixture();
  f.write('note.go', prose('The TRUE last-two across both parts is (20@00:02, 30@00:04).'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts an unmarked N:M pair, the shape the gate deliberately does not model', () => {
  // Not an oversight and not a tolerance: `1:1` correspondences, clock times
  // and `host:9000` outnumber the addresses two to one among unmarked pairs,
  // and nothing lexical separates `76:23` the address from `23:59` the
  // timestamp. `docs/test-strategy.md` records the refusal. Pinned here so a
  // later widening has to be a deliberate edit to this case.
  const f = fixture();
  f.write('note.go', prose('A Head maps 1:1 onto a Signal, and the window closes at 23:59:30.'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a hyphenated line-N, which names a fixture log line and not a source line', () => {
  // `line-3` / `line-4` / `line-5` are the NAMES of log lines in an
  // internal/api/loki fixture, and they outnumber the one real hyphenated
  // address three to one. Requiring a space after the keyword costs that
  // instance — repointed by hand, leaving none — and buys immunity to these.
  const f = fixture();
  f.write('note.go', prose('The latest three timestamps should be line-3, line-4, line-5.'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a line address inside a quoted transcript, which is not the note prose', () => {
  // A tab-indented block quotes somebody else's output verbatim. Rewriting a
  // tool's own error text would falsify the record, and the block is governed
  // by shape 3 instead.
  const f = fixture();
  f.write(
    'note.go',
    'package fixture\n\n// The release run died here:\n//\n//\terror: recipe `up` failed on line 1306 with exit code 1\n//\n// so the pull is retried now.\nvar _ = 0\n',
  );
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a line address in a string that is not a report message', () => {
  // The parse-error text a test asserts on describes the USER'S input, and no
  // rewrite of it is available or wanted.
  const f = fixture();
  f.write('note.go', 'package fixture\n\nvar want = "parse error at line 5, col 0: x"\n');
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

// ---------------------------------------------------------------------------
// Shape 3 (#2969): the code block quoted under a citation. Only PROVABLE
// mis-attribution is rejected — a block quoting code that lives in a file other
// than the one the citation names. A block that resolves nowhere is left alone,
// because a mutant's rewritten form is indistinguishable from a stale quote.
// ---------------------------------------------------------------------------

// OTHER — a second file, so a block can quote real code from the wrong place.
const OTHER = ['package fixture', '', 'func gamma(x int) int {', '\tif somethingElseEntirely(x) {', '\t\treturn 2', '\t}', '\treturn variadic(1, 2)', '}', '', 'func variadic(a ...int) int { return len(a) }', ''].join('\n');

// quoting — a note citing target.go with `block` quoted underneath it.
const quoting = (block) =>
  `package fixture\n\n// The guard target.go:\`numAnchors - 1\`, whose form\n//\n//\t${block}\n//\n// the mutant rewrites.\nvar _ = 0\n`;

test('rejects a quoted block that lives in a file other than the one cited', () => {
  const f = fixture();
  f.write('other.go', OTHER);
  f.write('note.go', quoting('if somethingElseEntirely(x) {'));
  const r = run(f.dir);
  assert.equal(r.status, 1, r.output);
  assert.match(r.output, /is not in target\.go/);
  assert.match(r.output, /It lives in other\.go/);
});

test('accepts a quoted block that is in the cited file', () => {
  const f = fixture();
  f.write('other.go', OTHER);
  f.write('note.go', quoting('if n > 0 && n < 10 {'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a quoted block showing a form that exists nowhere — the mutant', () => {
  // The distinction the whole check turns on. A mutant's rewritten form is the
  // point of an adjudication note and resolves nowhere by construction, so it
  // is never asked about; only a block that resolves in the WRONG file is.
  const f = fixture();
  f.write('other.go', OTHER);
  f.write('note.go', quoting('if n > 0 || n < 10 {'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts an indented citation list, which is not quoted code', () => {
  // The naive reading treats these as code and is wrong every time: measured 23
  // of 23 non-resolving blocks on the tree were entries in a citation list.
  const f = fixture();
  f.write('other.go', OTHER);
  f.write('note.go', quoting('other.go:`if somethingElseEntirely(x) {`'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});

test('accepts a bare elision line, which names nothing to attribute', () => {
  // `...` is a substring of every variadic signature in the repository, so
  // without this it reports a mis-attribution that is an artefact of the search.
  const f = fixture();
  f.write('other.go', OTHER);
  f.write('note.go', quoting('...'));
  const r = run(f.dir);
  assert.equal(r.status, 0, r.output);
});
