// Tests for the weekly external link-check report classifier.

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  classify,
  findTrackingIssue,
  isBotChallenge,
  PROBE_ALIVE,
  PROBE_CHALLENGED,
  PROBE_DEAD,
  probeLink,
  renderReport,
} from './link-check-external.mjs';

// A Vercel Security Checkpoint answer, header for header as deepwiki.com sent
// it to an automated client: 429 plus `x-vercel-mitigated: challenge`.
const vercelChallenge = new Headers({
  server: 'Vercel',
  'x-vercel-mitigated': 'challenge',
  'content-type': 'text/html; charset=utf-8',
});

function lycheeReport(errorMap) {
  return { total: 10, unique: 8, successful: 7, timeouts: 0, redirects: 0, excludes: 0, error_map: errorMap };
}

const rateLimited = {
  url: 'https://deepwiki.com/tsouza/cerberus',
  status: { text: 'Rejected status code: 429 Too Many Requests', code: 429 },
  span: { line: 27, column: 1 },
};
const notFound = {
  url: 'https://github.com/grafana/loki/tree/main/pkg/logql/bench/queries',
  status: { text: 'Rejected status code: 404 Not Found', code: 404 },
  span: { line: 160, column: 3 },
};
const protocolError = {
  url: 'https://clickhouse.com/docs/reference/settings',
  status: { text: 'HTTP/2 protocol error. Server may not support HTTP/2 properly' },
  span: { line: 1257, column: 1111 },
};

test('a Vercel or Cloudflare challenge header is a bot challenge', () => {
  assert.equal(isBotChallenge(vercelChallenge), true);
  assert.equal(isBotChallenge(new Headers({ 'cf-mitigated': 'challenge' })), true);
  assert.equal(isBotChallenge(new Headers({ 'X-Vercel-Mitigated': ' Challenge ' })), true);
});

test('a plain rate limit or an unrelated mitigation value is not a bot challenge', () => {
  assert.equal(isBotChallenge(new Headers({ server: 'Vercel', 'retry-after': '30' })), false);
  assert.equal(isBotChallenge(new Headers({ 'x-vercel-mitigated': 'deny' })), false);
  assert.equal(isBotChallenge(new Headers({})), false);
});

test('probeLink reads the re-probe response status and headers', async () => {
  const challenged = await probeLink('https://example.test/', {
    fetchImpl: async () => new Response('checkpoint', { status: 429, headers: vercelChallenge }),
  });
  assert.equal(challenged, PROBE_CHALLENGED);

  const gone = await probeLink('https://example.test/', {
    fetchImpl: async () => new Response('', { status: 404 }),
  });
  assert.equal(gone, PROBE_DEAD);

  const plainRateLimit = await probeLink('https://example.test/', {
    fetchImpl: async () => new Response('', { status: 429, headers: { 'retry-after': '30' } }),
  });
  assert.equal(plainRateLimit, PROBE_DEAD);
});

test('a 2xx re-probe is alive, even with a challenge header on it', async () => {
  const ok = await probeLink('https://example.test/', {
    fetchImpl: async () => new Response('page', { status: 200 }),
  });
  assert.equal(ok, PROBE_ALIVE);

  const okWithHeader = await probeLink('https://example.test/', {
    fetchImpl: async () => new Response('page', { status: 200, headers: vercelChallenge }),
  });
  assert.equal(okWithHeader, PROBE_ALIVE);
});

test('a transport failure on re-probe keeps the link dead', async () => {
  const verdict = await probeLink('https://example.test/', {
    fetchImpl: async () => {
      throw new TypeError('fetch failed');
    },
  });
  assert.equal(verdict, PROBE_DEAD);
});

test('classify moves challenged and alive HTTP errors out of the dead set', async () => {
  const probed = [];
  const probe = async (url) => {
    probed.push(url);
    return url === rateLimited.url ? PROBE_CHALLENGED : PROBE_DEAD;
  };
  const { dead, challenged } = await classify(
    lycheeReport({ 'README.md': [rateLimited], 'docs/compatibility.md': [notFound], 'docs/operations.md': [protocolError] }),
    { probe },
  );
  assert.deepEqual(
    challenged.map((r) => r.url),
    [rateLimited.url],
  );
  assert.deepEqual(dead.map((r) => r.url).sort(), [notFound.url, protocolError.url].sort());
  // A status-less transport error never reached a server, so it is not probed.
  assert.deepEqual(probed.sort(), [notFound.url, rateLimited.url].sort());
});

test('a 429 without a challenge header stays dead', async () => {
  const { dead, challenged, alive } = await classify(lycheeReport({ 'README.md': [rateLimited] }), {
    probe: async () => PROBE_DEAD,
  });
  assert.equal(dead.length, 1);
  assert.equal(challenged.length, 0);
  assert.equal(alive.length, 0);
});

test('an HTTP error whose re-probe answers 2xx is alive, not dead', async () => {
  const { dead, challenged, alive } = await classify(
    lycheeReport({ 'README.md': [rateLimited], 'docs/compatibility.md': [notFound] }),
    { probe: async (url) => (url === notFound.url ? PROBE_ALIVE : PROBE_DEAD) },
  );
  assert.deepEqual(
    alive.map((r) => r.url),
    [notFound.url],
  );
  assert.deepEqual(
    dead.map((r) => r.url),
    [rateLimited.url],
  );
  assert.equal(challenged.length, 0);
  const md = renderReport(lycheeReport({}), { dead, challenged, alive });
  assert.match(md, /\| Dead +\| 1 \|/);
  assert.match(md, /\| Alive on re-probe \| 1 \|/);
  assert.equal(md.includes(notFound.url), false, 'an alive link is not listed');
});

test('the report lists dead and challenged links in separate sections', () => {
  const report = lycheeReport({});
  const md = renderReport(report, {
    dead: [{ input: 'docs/compatibility.md', ...notFound }],
    challenged: [{ input: 'README.md', ...rateLimited }],
  });
  const deadAt = md.indexOf('## Dead links per input');
  const challengedAt = md.indexOf('## Bot-challenged (not counted as dead)');
  assert.ok(deadAt >= 0 && challengedAt > deadAt);
  assert.match(md.slice(deadAt, challengedAt), /\[404\] <https:\/\/github\.com\/grafana\/loki\/.*> \(at 160:3\)/);
  assert.match(md.slice(challengedAt), /\[429\] <https:\/\/deepwiki\.com\/tsouza\/cerberus> \(at 27:1\)/);
  assert.match(md, /\| Dead +\| 1 \|/);
  assert.match(md, /\| Bot-challenged +\| 1 \|/);
});

test('a status-less error renders as ERROR', () => {
  const md = renderReport(lycheeReport({}), { dead: [{ input: 'docs/operations.md', ...protocolError }], challenged: [] });
  assert.match(md, /\* \[ERROR\] <https:\/\/clickhouse\.com\/docs\/reference\/settings> \(at 1257:1111\) \| HTTP\/2 protocol error/);
  assert.doesNotMatch(md, /Bot-challenged \(not counted/);
});

test('findTrackingIssue returns the oldest open issue with the exact title, skipping PRs', async () => {
  const title = 'Weekly external link check found dead links';
  const seen = [];
  const fetchImpl = async (url) => {
    seen.push(url);
    return Response.json([
      { number: 3779, title },
      { number: 3700, title, pull_request: {} },
      { number: 3766, title },
      { number: 3001, title: 'something else' },
    ]);
  };
  const number = await findTrackingIssue({ repo: 'o/r', token: 't', title, label: 'link-rot', fetchImpl });
  assert.equal(number, 3766);
  assert.match(seen[0], /\/repos\/o\/r\/issues\?state=open&labels=link-rot&per_page=100&page=1$/);
});

test('findTrackingIssue honours a non-default API base', async () => {
  const seen = [];
  await findTrackingIssue({
    repo: 'o/r',
    token: 't',
    title: 'Weekly external link check found dead links',
    label: 'link-rot',
    apiBase: 'https://ghe.example.test/api/v3',
    fetchImpl: async (url) => {
      seen.push(url);
      return Response.json([]);
    },
  });
  assert.match(seen[0], /^https:\/\/ghe\.example\.test\/api\/v3\/repos\/o\/r\/issues\?/);
});

test('findTrackingIssue returns null when no open issue matches', async () => {
  const number = await findTrackingIssue({
    repo: 'o/r',
    token: 't',
    title: 'Weekly external link check found dead links',
    label: 'link-rot',
    fetchImpl: async () => Response.json([]),
  });
  assert.equal(number, null);
});
