// dependabot-pr-title.test.mjs — pins for the Dependabot title repair
// (issue #3647). The fixtures are real Dependabot titles from this repository,
// plus constructed ones for the later rewrite steps that no real title has
// needed yet. The limit is read from the repository's own .commitlintrc.json,
// so these cases exercise the threshold the `pr-body` gate enforces.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

import { landingSubject } from './commitlint-pr-title.mjs';
import {
  DEPENDABOT_LOGIN,
  headerMaxLength,
  planTitle,
  run,
  shortenTitle,
} from './dependabot-pr-title.mjs';

const MAX = headerMaxLength(JSON.parse(readFileSync(new URL('../../.commitlintrc.json', import.meta.url), 'utf8')));
const NUMBER = '3644';
const REPO = 'tsouza/cerberus';
const API = 'https://api.example.test';

const plan = (title, author = DEPENDABOT_LOGIN, number = NUMBER) =>
  planTitle({ title, number, author, maxLength: MAX });

// A title whose landing subject is exactly `length` characters.
const titleWithLandingLength = (length) => {
  const head = 'chore(deps): bump actions/checkout from 6 to 7 ';
  return head + 'x'.repeat(length - head.length - ` (#${NUMBER})`.length);
};

// [title, expected rewrite] — each rewrite also asserted to keep the
// dependency and target version and to fit the limit.
const REWRITES = [
  [
    // #3644: the incident.
    'chore(deps-dev): bump @types/node from 26.5.1 to 26.6.1 in /test/e2e/playwright in the playwright-deps group',
    'chore(deps-dev): bump @types/node from 26.5.1 to 26.6.1 in /test/e2e/playwright',
  ],
  [
    'chore(deps-dev): Bump @playwright/test from 1.61.1 to 1.62.0 in /test/e2e/playwright in the playwright-deps group',
    'chore(deps-dev): Bump @playwright/test from 1.61.1 to 1.62.0 in /test/e2e/playwright',
  ],
  [
    'chore(deps): Bump google.golang.org/grpc from 1.82.0 to 1.82.1 in the go-deps group across 1 directory',
    'chore(deps): Bump google.golang.org/grpc from 1.82.0 to 1.82.1 in the go-deps group',
  ],
  [
    // Group and directory removal are not enough: the source version goes.
    'chore(deps): bump github.com/apache/thrift from 0.23.1-0.20260429145742-d2acd3c49e58 to ' +
      '0.24.1-0.20260601120000-0123456789ab in the go-deps group',
    'chore(deps): bump github.com/apache/thrift to 0.24.1-0.20260601120000-0123456789ab',
  ],
  [
    // Even `bump <dependency> to <version>` is too long: leading path segments go.
    'chore(deps): bump github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter ' +
      'from 0.139.0 to 0.140.0 in the upstream-parsers group',
    'chore(deps): bump .../opentelemetry-collector-contrib/exporter/clickhouseexporter to 0.140.0',
  ],
];

for (const [title, expected] of REWRITES) {
  test(`over-long title is rewritten: ${title}`, () => {
    assert.ok(landingSubject(title, NUMBER).length > MAX, 'fixture must start over the limit');
    assert.deepEqual(plan(title), { action: 'rewrite', title: expected });
    assert.ok(landingSubject(expected, NUMBER).length <= MAX);
  });

  test(`rewrite keeps the prefix, dependency tail and target version: ${title}`, () => {
    const [, prefix, dependency, target] = /^(\S+: [Bb]ump )(\S+) .*?\bto (\S+)/.exec(title);
    assert.ok(expected.startsWith(prefix));
    assert.ok(expected.includes(`${dependency.split('/').at(-1)} `));
    assert.ok(expected.includes(` to ${target}`));
  });

  test(`rewrite is idempotent: ${title}`, () => {
    assert.deepEqual(plan(expected), { action: 'keep' });
  });
}

test('the first fitting step wins: a group-only overflow keeps the directory and source version', () => {
  const [title, expected] = REWRITES[0];
  assert.equal(shortenTitle(title, { number: NUMBER, maxLength: MAX }), expected);
  assert.match(expected, / from 26\.5\.1 /);
  assert.match(expected, / in \/test\/e2e\/playwright$/);
});

test('the limit is measured on the landing subject, including the (#N) suffix', () => {
  assert.deepEqual(plan(titleWithLandingLength(MAX)), { action: 'keep' });
  assert.deepEqual(plan(titleWithLandingLength(MAX + 1)), {
    action: 'rewrite',
    title: titleWithLandingLength(MAX + 1).replace(' from 6 to 7', ' to 7'),
  });
  // The same title fits under a shorter PR number and not under a longer one.
  const title = titleWithLandingLength(MAX);
  assert.deepEqual(plan(title, DEPENDABOT_LOGIN, '1'), { action: 'keep' });
  assert.equal(plan(title, DEPENDABOT_LOGIN, '1234567').action, 'rewrite');
});

test('already-compliant dependabot titles are kept', () => {
  for (const title of [
    'chore(deps): bump actions/checkout from 6 to 7',
    'chore(deps): bump golang.org/x/tools from 0.49.0 to 0.50.0 in the go-deps group',
    'chore(deps): Bump the github-actions group across 1 directory with 10 updates',
    'chore(deps-dev): bump the playwright-deps group in /test/e2e/playwright with 2 updates',
  ]) {
    assert.deepEqual(plan(title), { action: 'keep' }, title);
  }
});

test('non-dependabot PRs are skipped, however long the title', () => {
  const [title] = REWRITES[0];
  for (const author of ['tsouza', 'renovate[bot]', 'dependabot', 'github-actions[bot]', '']) {
    assert.deepEqual(plan(title, author), { action: 'skip' }, author);
  }
});

test('a title no step can shorten enough is unfixable, not truncated', () => {
  const title = `chore(deps): bump left-pad from 1.0.0 to 1.0.1-${'a'.repeat(MAX)}`;
  assert.deepEqual(plan(title), { action: 'unfixable', length: landingSubject(title, NUMBER).length });
  const unknownShape = `chore(deps): update ${'x'.repeat(MAX)}`;
  assert.equal(plan(unknownShape).action, 'unfixable');
});

test('headerMaxLength reads the rule and rejects a config without it', () => {
  assert.equal(headerMaxLength({ rules: { 'header-max-length': [2, 'always', 72] } }), 72);
  assert.throws(() => headerMaxLength({ rules: {} }), /header-max-length/);
  assert.throws(() => headerMaxLength({ rules: { 'header-max-length': [2, 'always'] } }), /header-max-length/);
});

// ---- run(): the API orchestration, against a recording fake fetch ----

function fakeGitHub({ title, patchedTitle }) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const method = init.method ?? 'GET';
    const auth = (init.headers?.authorization ?? init.headers?.Authorization ?? '').replace('Bearer ', '');
    calls.push({ url, method, auth, body: init.body ? JSON.parse(init.body) : undefined });
    const body = method === 'PATCH'
      ? { title: patchedTitle ?? JSON.parse(init.body).title }
      : { title, body: 'Bumps things.' };
    return { ok: true, status: 200, statusText: 'OK', json: async () => body };
  };
  return { calls, fetchImpl };
}

const runWith = (gh, overrides = {}) =>
  run({
    number: NUMBER,
    author: DEPENDABOT_LOGIN,
    repository: REPO,
    apiUrl: API,
    readToken: 'read-token',
    editToken: 'edit-token',
    maxLength: MAX,
    fetchImpl: gh.fetchImpl,
    ...overrides,
  });

test('run rewrites an over-long live title with the edit token', async () => {
  const [title, expected] = REWRITES[0];
  const gh = fakeGitHub({ title });
  assert.equal(await runWith(gh), 0);
  assert.deepEqual(
    gh.calls.map(({ method, url, auth }) => [method, url, auth]),
    [
      ['GET', `${API}/repos/${REPO}/pulls/${NUMBER}`, 'read-token'],
      ['PATCH', `${API}/repos/${REPO}/pulls/${NUMBER}`, 'edit-token'],
    ],
  );
  assert.deepEqual(gh.calls[1].body, { title: expected });
});

test('run leaves a compliant title alone and needs no edit token', async () => {
  const gh = fakeGitHub({ title: 'chore(deps): bump actions/checkout from 6 to 7' });
  assert.equal(await runWith(gh, { editToken: '' }), 0);
  assert.deepEqual(gh.calls.map((c) => c.method), ['GET']);
});

test('run never reads or edits a non-dependabot PR', async () => {
  const gh = fakeGitHub({ title: REWRITES[0][0] });
  assert.equal(await runWith(gh, { author: 'tsouza' }), 0);
  assert.deepEqual(gh.calls, []);
});

test('run fails without editing when a rewrite is needed but the edit token is missing', async () => {
  const gh = fakeGitHub({ title: REWRITES[0][0] });
  assert.equal(await runWith(gh, { editToken: '' }), 1);
  assert.deepEqual(gh.calls.map((c) => c.method), ['GET']);
});

test('run fails without editing on an unfixable title', async () => {
  const gh = fakeGitHub({ title: `chore(deps): update ${'x'.repeat(MAX)}` });
  assert.equal(await runWith(gh), 1);
  assert.deepEqual(gh.calls.map((c) => c.method), ['GET']);
});

test('run fails when the edited title does not read back as planned', async () => {
  const gh = fakeGitHub({ title: REWRITES[0][0], patchedTitle: REWRITES[0][0] });
  assert.equal(await runWith(gh), 1);
});

test('run fails on missing inputs', async () => {
  const gh = fakeGitHub({ title: REWRITES[0][0] });
  assert.equal(await runWith(gh, { number: '' }), 1);
  assert.equal(await runWith(gh, { readToken: '' }), 1);
  assert.deepEqual(gh.calls, []);
});
