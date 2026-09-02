// Suite for forbid-contradicted-mutants.mjs. Every case drives the real CLI
// over a throwaway git repository, because the gate's scope comes from
// `git ls-files` and a unit test that bypassed that would not be testing what
// CI runs.
//
// The suite's job is to prove the gate CAN FAIL, one modelled shape at a time —
// a gate whose first green nobody has seen go red reports something other than
// what a reader takes it to mean. Each rejection case is paired with its
// nearest-miss acceptance case, because the two failure modes of this gate pull
// in opposite directions: too wide and it rejects the legitimate pattern of a
// killed mutant sitting beside a proven-equivalent sibling on ONE line; too
// narrow and it accepts the contradiction it exists to catch.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'forbid-contradicted-mutants.mjs');

// The cited file every fixture shares. `cap` models the capacity hint of
// cerberus issue #2958; the `||` line models two independent boundary mutants
// sharing one line, one killed and one equivalent.
const TARGET = [
  'package fixture',
  '',
  'func alpha(groupAliases []string) []int {',
  '\tfrags := make([]int, 0, len(groupAliases)*2+6)',
  '\treturn frags',
  '}',
  '',
  'func beta(cols []int) int {',
  '\tbest := -1',
  '\tfor _, r := range cols {',
  '\t\tif best < 0 || r < best {',
  '\t\t\tbest = r',
  '\t\t}',
  '\t}',
  '\treturn best',
  '}',
  '',
].join('\n');

const GREMLINS_YAML = [
  'silent: false',
  'mutants:',
  '  arithmetic-base:',
  '    enabled: true',
  '  conditionals-boundary:',
  '    enabled: true',
  '  conditionals-negation:',
  '    enabled: true',
  '  invert-logical:',
  '    enabled: true',
  '',
].join('\n');

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-contradicted-mutants-'));
  const git = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  git(['init', '--quiet']);
  writeFileSync(join(dir, 'target.go'), TARGET);
  // The gate reads its mutator vocabulary from `.gremlins.yaml` rather than a
  // hard-coded list, so every fixture repo needs one.
  writeFileSync(join(dir, '.gremlins.yaml'), GREMLINS_YAML);
  git(['add', 'target.go', '.gremlins.yaml']);
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

const FOOTER_CAP = [
  '// NOT KILLABLE — documented, not defended by a test.',
  '//',
  '// target.go:`len(groupAliases)*2+6` (ARITHMETIC_BASE) is a capacity hint,',
  '// unobservable from any exported surface.',
].join('\n');

test('rejects a kill claim on a mutant a NOT KILLABLE footer calls equivalent', () => {
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /claims to KILL `len\(groupAliases\)\*2\+6`/);
  assert.match(output, /1 mutant\(s\) carry two opposite verdicts/);
});

test('rejects the contradiction across two separate files', () => {
  const f = fixture();
  f.write(
    'kill_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
    ].join('\n'),
  );
  f.write('footer_test.go', ['package fixture', '', FOOTER_CAP, ''].join('\n'));
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /kill_test\.go:4: claims to KILL/);
  assert.match(output, /footer_test\.go:\d+ documents as equivalent/);
});

test('rejects it when the two verdicts quote the construct at different widths', () => {
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestBoundary kills the boundary flip at target.go:`best < 0`.',
      'func TestBoundary(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`if best < 0` (CONDITIONALS_BOUNDARY) is equivalent.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /claims to KILL `best < 0`/);
});

test('accepts a killed mutant beside an equivalent SIBLING on the same line', () => {
  // The legitimate shape this gate must not break: `best < 0 || r < best`
  // carries two independent CONDITIONALS_BOUNDARY mutants. One is killed, the
  // other is proven equivalent. Disjoint constructs on a shared line are
  // different mutants, not a contradiction.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestBoundary kills the `<` boundary flip on target.go:`best < 0`.',
      'func TestBoundary(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`r < best` (CONDITIONALS_BOUNDARY) is equivalent because',
      '// the boundary is unreachable.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
  assert.match(output, /no mutant carries both/);
});

test('accepts a test that DISCLAIMS the kill and defers to the footer', () => {
  // The correct remedy-1 pattern: a contract test that pins behaviour while
  // explicitly not claiming the kill. Its prose names the footer, so a gate
  // keyed on the phrase rather than on the footer opener would misread it as a
  // second verdict.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestShape pins the slice contents. It does NOT kill the',
      '// ARITHMETIC_BASE mutant at target.go:`len(groupAliases)*2+6`; see the',
      '// NOT KILLABLE note at the foot of this file for why it is equivalent.',
      'func TestShape(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
});

test('accepts a kill claim on a mutant no footer contradicts', () => {
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
  assert.match(output, /1 kill claim\(s\)/);
});

test('finds a footer that opens a paragraph mid-comment-run', () => {
  // prewhere_mutation_test.go carries an unrelated note and a NOT KILLABLE
  // footer in one unbroken `//` run. A gate that only recognised a footer at
  // the START of a comment block would read this file as having no verdict.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      '// CI-TIMING NOISE, not a test gap.',
      '//',
      '// Some unrelated prose about a flaky run.',
      '//',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`len(groupAliases)*2+6` (ARITHMETIC_BASE) is a capacity hint.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /claims to KILL/);
});

test('PATHSPECS narrows the scanned set', () => {
  const f = fixture();
  f.write(
    'skipped/adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  f.write('other/clean_test.go', ['package fixture', '', 'func TestClean(t *testing.T) {}', ''].join('\n'));
  // In scope by default -> caught. Narrowed to a DIFFERENT, matching pathspec
  // -> clean. The second pathspec must match something, or the pass would come
  // from the vacuous-scan guard rather than from the narrowing.
  assert.equal(run(f.dir).status, 1);
  assert.equal(run(f.dir, { PATHSPECS: 'other/*_test.go' }).status, 0);
});

test('fails closed when the pathspec matches no file', () => {
  // A green over zero files is not evidence. This is the same rule
  // verify-code-citations.mjs applies to its own scope.
  const f = fixture();
  f.write('adjudication_test.go', ['package fixture', '', FOOTER_CAP, ''].join('\n'));
  const { status, output } = run(f.dir, { PATHSPECS: 'nothing_here/*_test.go' });
  assert.notEqual(status, 0, output);
  assert.match(output, /vacuous/);
});

test('accepts two verdicts on one construct when they name DIFFERENT mutators', () => {
  // The legitimate shape found while building this gate:
  // `err != nil && srcErr == nil` carries CONDITIONALS_NEGATION mutants a test
  // kills AND an INVERT_LOGICAL mutant a footer proves equivalent. One
  // expression, different mutants, two compatible verdicts.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestNegation kills both CONDITIONALS_NEGATION mutants of',
      '// target.go:`best < 0 || r < best`.',
      'func TestNegation(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`best < 0 || r < best` (INVERT_LOGICAL, `||` -> `&&`) is',
      '// equivalent.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
});

test('still rejects when the two verdicts name the SAME mutator', () => {
  // The nearest miss of the case above: change only the footer's mutator so the
  // two sets intersect, and the same fixture must now fail. Without this pair,
  // the acceptance above could be passing because the gate stopped working.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestNegation kills both CONDITIONALS_NEGATION mutants of',
      '// target.go:`best < 0 || r < best`.',
      'func TestNegation(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`best < 0 || r < best` (CONDITIONALS_NEGATION) is',
      '// equivalent.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /claims to KILL/);
});

test('reports the pair when only ONE side names a mutator', () => {
  // Fail closed: naming different mutators is evidence of different mutants,
  // but naming none is no evidence, so the pair must still surface. This is the
  // shape the second real false kill had — "the boundary flip", unnamed.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestBoundary kills the boundary flip at target.go:`best < 0`.',
      'func TestBoundary(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`best < 0` (CONDITIONALS_BOUNDARY) is equivalent.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
});

test('reads a sentence-initial `Kills` claim, not just the canonical header', () => {
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity pins the slice contents.',
      '//',
      '// Kills the ARITHMETIC_BASE mutant of',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
});

test('does not read a kill-switch or a pod-kill as a claim', () => {
  // `kill` lowercase and singular appears all over the tree in unrelated prose.
  // Reading it as a claim would make every such comment a candidate verdict.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestSwitch pins the absolute kill-switch, and models the ch-pod-kill',
      '// failure. Tests that kill the LIVED mutants live elsewhere.',
      '// It touches target.go:`len(groupAliases)*2+6` only incidentally.',
      'func TestSwitch(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
});

test('fails closed when the mutator vocabulary cannot be read', () => {
  // An empty vocabulary would make every named-mutator set empty, and an empty
  // set names nothing — which would silently WIDEN the gate rather than break
  // it. That has to be an error, not a default.
  const f = fixture();
  f.write('.gremlins.yaml', 'silent: false\n');
  f.write(
    'adjudication_test.go',
    ['package fixture', '', FOOTER_CAP, ''].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.notEqual(status, 0, output);
  assert.match(output, /mutants:/);
});

test('reads an equivalence verdict written outside a NOT KILLABLE footer', () => {
  // Several packages keep a file-header ledger instead of the prescribed
  // footer ("EQUIVALENCE verdicts (no killing test possible …)", "Genuinely
  // equivalent"). Reading only the footer opener left ten such verdicts
  // invisible, one of them a capacity-hint ARITHMETIC_BASE — this gate's own
  // bug class, in another package.
  const f = fixture();
  f.write(
    'ledger_test.go',
    [
      'package fixture',
      '',
      '// EQUIVALENCE verdicts (no killing test possible — documented here so',
      '// the next reader does not re-derive them):',
      '//',
      '//   - target.go:`len(groupAliases)*2+6` ARITHMETIC_BASE. The `*2` is a',
      '//     slice CAPACITY pre-allocation hint. Genuinely equivalent.',
      '',
    ].join('\n'),
  );
  f.write(
    'kill_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /ledger_test\.go:\d+ documents as equivalent/);
});

test('attributes mutators per PARAGRAPH, not per comment run', () => {
  // A footer routinely adjudicates several mutants in one unbroken `//` run.
  // Collecting the run's whole vocabulary and stamping it on every citation
  // would let an unrelated neighbouring paragraph lend its mutator name to
  // this one, and the different-mutators defence would stop discriminating.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestNegation kills both CONDITIONALS_NEGATION mutants of',
      '// target.go:`best < 0 || r < best`.',
      'func TestNegation(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`best < 0 || r < best` (INVERT_LOGICAL) is equivalent.',
      '//',
      '// Separately, an unrelated CONDITIONALS_NEGATION elsewhere in the file',
      '// is also equivalent, for reasons of its own.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 0, output);
});

test('reports one contradiction once, however many verdicts restate it', () => {
  // A footer and the disclaiming test that points at it are both verdicts.
  // Reporting the same kill claim against each would read as two defects.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      '// TestShape pins the contents. The mutant at',
      '// target.go:`len(groupAliases)*2+6` is equivalent; see the footer.',
      'func TestShape(t *testing.T) {}',
      '',
      FOOTER_CAP,
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
  assert.match(output, /1 mutant\(s\) carry two opposite verdicts/);
});

test('a footer paragraph that never says "equivalent" is still a verdict', () => {
  // The real exemplars footer argues one mutant is byte-identical without ever
  // writing the word. Requiring the word inside every footer paragraph would
  // have lost half of what this gate was built to catch.
  const f = fixture();
  f.write(
    'adjudication_test.go',
    [
      'package fixture',
      '',
      '// TestCapacity kills the ARITHMETIC_BASE mutant at',
      '// target.go:`len(groupAliases)*2+6`.',
      'func TestCapacity(t *testing.T) {}',
      '',
      '// NOT KILLABLE — documented, not defended by a test.',
      '//',
      '// target.go:`len(groupAliases)*2+6` (ARITHMETIC_BASE) produces a',
      '// byte-identical statement; only two dead allocations differ.',
      '',
    ].join('\n'),
  );
  const { status, output } = run(f.dir);
  assert.equal(status, 1, output);
});
