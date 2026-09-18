// guard-heavy-local.test.mjs — node:test guard for the heavy-local PreToolUse
// hook, mirroring test/regression/guard_git_hook_test.go for its sibling.
//
// Two halves. The pure classifier (`kindsOf`) is driven over every shape the
// hook got wrong when it split on newlines and grepped for substrings: a
// commit-message heredoc that mentions gremlins, a grep for the runner's own
// file name, `just mutate-chdb` (the heaviest run in the repo, absent from the
// old hand list), `bash -c "just mutate"` and an `xargs gremlins` tail. Then
// the hook is run end to end, JSON payload in and exit code out, exactly as
// the harness drives it.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { kindsOf, recipeSets } from './guard-heavy-local.mjs';
import { segments, splitWords } from './shell-words.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const HOOK = join(HERE, 'guard-heavy-local.mjs');
const REPO_ROOT = join(HERE, '..', '..');

const SETS = recipeSets([
  'build', 'test', 'mutate', 'mutate-pkg', 'mutate-chdb', 'semantic-mutate',
  'update-golden', 'migration-golden', 'update-parity-ledgers',
  'update-cardinality-baseline', 'update-solver-decision-baseline', 'update-coverage-floor',
]);

const kinds = (command) => [...kindsOf(command, SETS)].sort();

test('recipeSets derives the families by name shape', () => {
  assert.deepEqual([...SETS.mutation].sort(), ['mutate', 'mutate-chdb', 'mutate-pkg']);
  assert.deepEqual(
    [...SETS.golden].sort(),
    ['migration-golden', 'update-cardinality-baseline', 'update-golden', 'update-parity-ledgers', 'update-solver-decision-baseline'],
  );
  assert.equal(SETS.mutation.has('semantic-mutate'), false, 'a single hand-authored mutant is not a gremlins sweep');
  assert.equal(SETS.golden.has('update-coverage-floor'), false, 'the coverage ledger is derived from a CI artifact');
});

test('a mutation recipe is blocked whichever of the family it is', () => {
  for (const cmd of ['just mutate', 'just mutate-pkg ./internal/chsql', 'just mutate-chdb', 'rtk just mutate', 'just -f Justfile mutate']) {
    assert.deepEqual(kinds(cmd), ['mutation'], cmd);
  }
});

test('a golden recipe is blocked whichever of the family it is', () => {
  for (const cmd of [
    'just update-golden promql',
    'just migration-golden',
    'just update-parity-ledgers',
    'just update-solver-decision-baseline',
    'cd /tmp/x && just update-cardinality-baseline',
  ]) {
    assert.deepEqual(kinds(cmd), ['golden'], cmd);
  }
});

test('a direct gremlins or runner invocation is blocked', () => {
  for (const cmd of [
    'gremlins unleash -t chdb ./internal/...',
    '/home/me/go/bin/gremlins unleash',
    'node .github/scripts/mutation-run.mjs',
    'MODE=leg node .github/scripts/mutation-run.mjs',
  ]) {
    assert.deepEqual(kinds(cmd), ['mutation'], cmd);
  }
});

test('the wrapped and piped shapes are unwrapped and blocked', () => {
  for (const [cmd, want] of [
    ['bash -c "just mutate"', ['mutation']],
    ["sh -c 'cd /x && just mutate-pkg ./y'", ['mutation']],
    ['echo ./internal/chsql | xargs gremlins unleash -i', ['mutation']],
    ['printf "%s\\n" pkg | xargs -n 1 -I {} gremlins unleash {}', ['mutation']],
    ['timeout 30m just mutate', ['mutation']],
    ['nice -n 10 just mutate', ['mutation']],
    ['systemd-run --scope --user -p MemoryMax=2G just mutate', ['mutation']],
    ['echo "$(just update-golden promql)"', ['golden']],
    ['just mutate && just update-golden all', ['golden', 'mutation']],
  ]) {
    assert.deepEqual(kinds(cmd), want, cmd);
  }
});

test('a heredoc that merely mentions the runner or gremlins is data, not a run', () => {
  const commit = [
    "git commit -q -F - <<'EOF'",
    'fix(mutation): cap the runner',
    '',
    'gremlins runs uncapped otherwise; see mutation-run.mjs and just mutate.',
    'EOF',
  ].join('\n');
  assert.deepEqual(kinds(commit), []);
  const unquoted = ['cat <<EOF', 'just mutate', 'EOF', 'ls'].join('\n');
  assert.deepEqual(kinds(unquoted), []);
});

test('a grep or echo that names the runner file is not a run', () => {
  for (const cmd of [
    'grep -n foo .github/scripts/mutation-run.mjs',
    'echo "gremlins unleash is heavy"',
    "echo 'just mutate'",
    'cat .github/scripts/mutation-run.mjs | head',
    'git log --oneline -- .github/scripts/mutation-run.mjs',
    'ls just/mutation.just',
  ]) {
    assert.deepEqual(kinds(cmd), [], cmd);
  }
});

test('an ordinary just recipe or an unrelated command is allowed', () => {
  for (const cmd of ['just build', 'just test', 'just semantic-mutate M-1', 'just update-coverage-floor', 'go test ./...']) {
    assert.deepEqual(kinds(cmd), [], cmd);
  }
});

test('splitWords honours quotes, escapes, separators and heredocs', () => {
  assert.deepEqual(splitWords('a "b c" \'d e\' f\\ g'), [['a', 'b c', 'd e', 'f g']]);
  assert.deepEqual(splitWords('a && b; c | d || e\nf'), [['a'], ['b'], ['c'], ['d'], ['e'], ['f']]);
  assert.deepEqual(splitWords('a "x && y" b'), [['a', 'x && y', 'b']]);
  assert.deepEqual(splitWords('cat <<EOF\nnot a; command\nEOF\nls'), [['cat'], ['ls']]);
  assert.deepEqual(splitWords('x "$(just mutate)"'), [['x'], ['just', 'mutate']]);
  assert.deepEqual(splitWords('(cd a && b)'), [['cd', 'a'], ['b']]);
});

test('segments strips wrappers and unwraps shells and xargs', () => {
  assert.deepEqual(segments('FOO=1 rtk just mutate'), [['just', 'mutate']]);
  assert.deepEqual(segments('bash -c "just mutate"'), [['bash', '-c', 'just mutate'], ['just', 'mutate']]);
  assert.deepEqual(segments('echo x | xargs -0 -n 1 gremlins unleash'), [['echo', 'x'], ['xargs', '-0', '-n', '1', 'gremlins', 'unleash'], ['gremlins', 'unleash']]);
  assert.deepEqual(segments('timeout 5m just build'), [['just', 'build']]);
});

function runHook(command, env = {}) {
  const payload = JSON.stringify({ tool_name: 'Bash', cwd: REPO_ROOT, tool_input: { command } });
  const res = spawnSync(process.execPath, [HOOK], {
    cwd: REPO_ROOT,
    input: payload,
    encoding: 'utf8',
    env: { ...process.env, CERBERUS_ALLOW_HEAVY_LOCAL: '', ...env },
  });
  return { status: res.status, stderr: res.stderr };
}

test('end to end: the hook blocks a mutation run and explains why', () => {
  const { status, stderr } = runHook('just mutate-chdb');
  assert.equal(status, 2);
  assert.match(stderr, /refusing to run mutation testing/);
});

test('end to end: the hook blocks a golden regeneration and names the workflow instead', () => {
  const { status, stderr } = runHook('just update-golden promql');
  assert.equal(status, 2);
  assert.match(stderr, /gh workflow run update-golden\.yml/);
});

test('end to end: the hook allows a commit message that talks about gremlins', () => {
  const commit = "git commit -F - <<'EOF'\nfix: gremlins runs uncapped otherwise\nEOF";
  const { status, stderr } = runHook(commit);
  assert.equal(status, 0, stderr);
});

test('end to end: the override lifts the block for one command', () => {
  assert.equal(runHook('just mutate', { CERBERUS_ALLOW_HEAVY_LOCAL: '1' }).status, 0);
  assert.equal(runHook('CERBERUS_ALLOW_HEAVY_LOCAL=1 just mutate').status, 0);
});
