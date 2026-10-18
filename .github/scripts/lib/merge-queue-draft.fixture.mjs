// merge-queue-draft.fixture.mjs — a merge-queue draft and the API answers that
// go with it, shared by the tests of the module and of the two gates that use
// it. The draft body follows the shape Mergify writes: prose that names the
// batch and the pull requests queued ahead of it, then the metadata block.

export const REPO = 'tsouza/cerberus';
export const API = 'https://api.github.test';
export const DRAFT = 900;
export const DRAFT_HEAD = 'd'.repeat(40);
export const AHEAD = 899;

export function draftBody(numbers, { ahead = [AHEAD] } = {}) {
  const links = numbers.map((n) => `[#${n}](/${REPO}/pull/${n})`).join(', ');
  return [
    '> [!NOTE]',
    `> **This is a merge queue draft opened by Mergify. Nobody needs to act on it.**`,
    `> Mergify opened it for batch **quiet-lake** of the \`default\` queue, to test ${links} before it merges into \`main\`.`,
    '',
    '### Queued ahead of this batch',
    '',
    ...ahead.map((n) => `- [#${n}](/${REPO}/pull/${n})`),
    '',
    '<details>',
    '<summary>Metadata for CI tooling, read by <code>mergify ci</code></summary>',
    '',
    '```yaml',
    '---',
    'all_scopes: false',
    `checking_base_sha: ${'b'.repeat(40)}`,
    'previous_check_retries: []',
    'pull_requests:',
    ...numbers.flatMap((n) => [`  - number: ${n}`, '    scopes: []']),
    'scopes: []',
    '...',
    '',
    '```',
    '',
    '</details>',
  ].join('\n');
}

export function draftPullRequest({ numbers, overrides = {} }) {
  return {
    number: DRAFT,
    body: draftBody(numbers),
    user: { login: 'mergify[bot]', type: 'Bot' },
    head: { ref: 'mergify/merge-queue/0a1b2c3d4e', sha: DRAFT_HEAD, repo: { full_name: REPO } },
    base: { ref: 'main' },
    ...overrides,
  };
}

// fakeApi answers the draft, each batch member and each comparison. `members`
// maps a number to { title, body, author, base, compare }.
export function fakeApi({ draft, members }) {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(url);
    const ok = (body) => ({ ok: true, status: 200, statusText: 'OK', json: async () => body });
    if (url === `${API}/repos/${REPO}/pulls/${draft.number}`) return ok(draft);
    for (const [n, m] of Object.entries(members)) {
      const sha = `${n}`.padStart(40, '0');
      if (url === `${API}/repos/${REPO}/pulls/${n}`) {
        return ok({
          number: Number(n),
          title: m.title ?? `fix: batch member ${n}`,
          body: m.body,
          user: { login: m.author ?? 'someone' },
          base: { ref: m.base ?? 'main' },
          head: { sha },
        });
      }
      if (url === `${API}/repos/${REPO}/compare/${sha}...${draft.head.sha}`) {
        return ok({ status: m.compare ?? 'ahead' });
      }
    }
    return { ok: false, status: 404, statusText: 'Not Found', json: async () => ({}) };
  };
  return { fetchImpl, calls };
}
