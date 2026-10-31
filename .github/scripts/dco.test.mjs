// dco.test.mjs — pins for the DCO sign-off gate (dco.mjs).
//
// The trailer parsing is pinned directly, and the gate as a whole runs
// against REAL git repos in a temp dir: which commits count (merge commits and
// file-less commits do not) is git behaviour, and a mock agreeing with the
// module's own assumptions could pass while real history slipped through.
//
// The negative controls are the point: an unsigned commit fails even beside a
// signed one, a sign-off naming someone other than the author fails, and an
// unresolvable range fails rather than reading as "nothing to check".

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { checkCommit, isServiceBotAuthor, run, signOffEmails } from './dco.mjs';

const AUTHOR = { authorName: 'Ada Lovelace', authorEmail: 'ada@example.com' };

test('signOffEmails reads every trailer, lower-cased', () => {
  const message = 'feat: x\n\nbody\n\nSigned-off-by: Ada Lovelace <Ada@Example.com>\nSigned-off-by: Bot <bot@example.com>';
  assert.deepEqual(signOffEmails(message), ['ada@example.com', 'bot@example.com']);
});

test('signOffEmails ignores a sign-off that is not a whole line', () => {
  assert.deepEqual(signOffEmails('feat: x\n\nsee Signed-off-by: Ada <ada@example.com> in the docs'), []);
});

test("checkCommit accepts the author's own sign-off, in any letter case", () => {
  assert.equal(checkCommit({ ...AUTHOR, message: 'feat: x\n\nSigned-off-by: Ada Lovelace <ADA@example.com>' }), null);
});

test('checkCommit rejects a commit with no sign-off', () => {
  assert.match(checkCommit({ ...AUTHOR, message: 'feat: x' }), /no Signed-off-by trailer/);
});

test("checkCommit rejects a sign-off that is not the author's", () => {
  assert.match(
    checkCommit({ ...AUTHOR, message: 'feat: x\n\nSigned-off-by: Someone Else <else@example.com>' }),
    /signed off by else@example\.com, but the author is Ada Lovelace <ada@example\.com>/,
  );
});

// Dependabot's real commit identity (PR #4008): its author email is a GitHub
// bot noreply address, and its sign-off names support@github.com.
const DEPENDABOT = {
  authorName: 'dependabot[bot]',
  authorEmail: '49699333+dependabot[bot]@users.noreply.github.com',
};
const DEPENDABOT_MESSAGE = 'chore(deps): bump x\n\nSigned-off-by: dependabot[bot] <support@github.com>';

test('isServiceBotAuthor accepts the listed service accounts only', () => {
  assert.equal(isServiceBotAuthor(DEPENDABOT.authorEmail), true);
  assert.equal(isServiceBotAuthor('41898282+github-actions[bot]@users.noreply.github.com'), true);
  assert.equal(isServiceBotAuthor('github-actions[bot]@users.noreply.github.com'), true);
  assert.equal(isServiceBotAuthor('122435+tsouza@users.noreply.github.com'), false);
  assert.equal(isServiceBotAuthor('dependabot[bot]@example.com'), false);
  assert.equal(isServiceBotAuthor('49699333+dependabot[bot]@users.noreply.github.com.example.com'), false);
});

test("checkCommit rejects an AI agent bot's unsigned commit", () => {
  assert.match(
    checkCommit({
      authorName: 'devin-ai-integration[bot]',
      authorEmail: '158243242+devin-ai-integration[bot]@users.noreply.github.com',
      message: 'feat: x',
    }),
    /no Signed-off-by trailer/,
  );
});

test("checkCommit exempts Dependabot's commit despite its mismatched sign-off", () => {
  assert.equal(checkCommit({ ...DEPENDABOT, message: DEPENDABOT_MESSAGE }), null);
});

test('checkCommit still rejects an unsigned commit from a human noreply address', () => {
  assert.match(
    checkCommit({ authorName: 'Someone', authorEmail: '1+someone@users.noreply.github.com', message: 'feat: x' }),
    /no Signed-off-by trailer/,
  );
});

function git(cwd, args) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8' });
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  return res.stdout.trim();
}

function commitFile(cwd, name, message, extra = []) {
  writeFileSync(path.join(cwd, name), `${name}\n`);
  git(cwd, ['add', name]);
  git(cwd, ['commit', '-q', ...extra, '-m', message]);
  return git(cwd, ['rev-parse', 'HEAD']);
}

// makeRepo: base -> signed commit -> unsigned empty commit -> signed commit on
// a side branch, merged back with an unsigned merge commit.
function makeRepo() {
  const cwd = mkdtempSync(path.join(tmpdir(), 'dco-'));
  git(cwd, ['init', '-q', '--initial-branch=main']);
  git(cwd, ['config', 'user.name', AUTHOR.authorName]);
  git(cwd, ['config', 'user.email', AUTHOR.authorEmail]);
  const base = commitFile(cwd, 'a.txt', 'feat: add a');
  commitFile(cwd, 'b.txt', 'feat: add b', ['-s']);
  git(cwd, ['commit', '-q', '--allow-empty', '-m', 'Initial plan']);
  git(cwd, ['checkout', '-q', '-b', 'side']);
  commitFile(cwd, 'c.txt', 'feat: add c', ['-s']);
  git(cwd, ['checkout', '-q', 'main']);
  commitFile(cwd, 'd.txt', 'feat: add d', ['-s']);
  git(cwd, ['merge', '-q', '--no-ff', '--no-edit', 'side']);
  return { cwd, base, head: () => git(cwd, ['rev-parse', 'HEAD']) };
}

test('run passes when every contributed commit is signed off, skipping merges and empty commits', () => {
  const repo = makeRepo();
  try {
    assert.equal(run({ baseSha: repo.base, headSha: repo.head(), cwd: repo.cwd }), 0);
  } finally {
    rmSync(repo.cwd, { recursive: true, force: true });
  }
});

test('run fails on one unsigned commit among signed ones', () => {
  const repo = makeRepo();
  try {
    commitFile(repo.cwd, 'e.txt', 'fix: unsigned e');
    assert.equal(run({ baseSha: repo.base, headSha: repo.head(), cwd: repo.cwd }), 1);
  } finally {
    rmSync(repo.cwd, { recursive: true, force: true });
  }
});

test('run passes a Dependabot-authored commit beside signed human commits', () => {
  const repo = makeRepo();
  try {
    commitFile(repo.cwd, 'g.txt', DEPENDABOT_MESSAGE, [
      `--author=${DEPENDABOT.authorName} <${DEPENDABOT.authorEmail}>`,
    ]);
    assert.equal(run({ baseSha: repo.base, headSha: repo.head(), cwd: repo.cwd }), 0);
  } finally {
    rmSync(repo.cwd, { recursive: true, force: true });
  }
});

test("run fails on a commit signed off by someone other than its author", () => {
  const repo = makeRepo();
  try {
    commitFile(repo.cwd, 'f.txt', 'fix: f\n\nSigned-off-by: Someone Else <else@example.com>');
    assert.equal(run({ baseSha: repo.base, headSha: repo.head(), cwd: repo.cwd }), 1);
  } finally {
    rmSync(repo.cwd, { recursive: true, force: true });
  }
});

test('run fails on an unresolvable range or a missing bound', () => {
  const repo = makeRepo();
  try {
    assert.equal(run({ baseSha: 'no-such-ref', headSha: repo.head(), cwd: repo.cwd }), 1);
    assert.equal(run({ baseSha: '', headSha: repo.head(), cwd: repo.cwd }), 1);
  } finally {
    rmSync(repo.cwd, { recursive: true, force: true });
  }
});
