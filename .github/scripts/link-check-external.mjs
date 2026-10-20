// link-check-external — turn lychee's JSON verdict into the weekly link-rot
// report, separating links that are dead from links a bot challenge hid.
//
// lychee reports every non-2xx answer as an error. Some hosts answer an
// automated client with an interstitial bot challenge instead of the page: a
// `429` / `403` whose body is a "security checkpoint" a browser solves and a
// link checker cannot. That status says nothing about whether the page exists.
// The host marks such a response explicitly with a vendor header, so this
// script re-probes every HTTP-status error once and classifies the answer by
// that header — never by URL:
//
//   - `x-vercel-mitigated: challenge` — Vercel's Security Checkpoint.
//   - `cf-mitigated: challenge`       — Cloudflare's challenge platform.
//
// A re-probe answered with a 2xx status means the page is there: the link is
// alive and lychee's error was transient. A challenged link is reported in its
// own section and is not counted as dead; an alive link is only counted.
// Every other error is dead and opens or updates the tracking issue.
//
// ENV CONTRACT
//   LYCHEE_JSON        — path to `lychee --format json` output. Required.
//   REPORT_PATH        — markdown report to write. Required.
//   GITHUB_TOKEN       — token for the tracking-issue lookup. Optional; without
//                        it (or GITHUB_REPOSITORY) no lookup is made.
//   GITHUB_REPOSITORY  — `owner/repo` that holds the tracking issue.
//   GITHUB_API_URL     — REST API base. Optional; defaults to
//                        https://api.github.com.
//   ISSUE_TITLE        — title of the tracking issue. Required with the token.
//   ISSUE_LABEL        — label the tracking issue carries. Required with the token.
//
// OUTPUTS ($GITHUB_OUTPUT)
//   dead_count    — number of dead links (challenged links excluded).
//   issue_number  — the open tracking issue to update, or empty to create one.
//
// Exit: 0 once the report is written, dead links or not — rot is reported,
// never a red lane. 1 when the inputs are missing or unreadable, or when the
// tracking-issue lookup fails (a report that cannot reach its issue is lost).

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice, setOutput } from './lib/gh.mjs';
import { DEFAULT_API_BASE, ghPaginate } from './lib/gh-api.mjs';

// The vendor headers that mark a response as a bot challenge, each mapped to
// the value that means "challenged".
export const CHALLENGE_HEADERS = Object.freeze({
  'x-vercel-mitigated': 'challenge',
  'cf-mitigated': 'challenge',
});

const probeTimeoutMs = 20_000;
const probeUserAgent = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130 Safari/537.36';

// isBotChallenge — true when a response's headers carry a challenge marker.
// `headers` is a fetch `Headers` (or anything with a case-insensitive `get`).
export function isBotChallenge(headers) {
  return Object.entries(CHALLENGE_HEADERS).some(
    ([name, value]) => (headers.get(name) ?? '').trim().toLowerCase() === value,
  );
}

// The verdicts probeLink returns.
export const PROBE_ALIVE = 'alive';
export const PROBE_CHALLENGED = 'challenged';
export const PROBE_DEAD = 'dead';

// probeLink — re-request `url` once and classify the answer: PROBE_ALIVE for
// a 2xx response, PROBE_CHALLENGED for a bot challenge, PROBE_DEAD otherwise.
// A transport failure is dead.
export async function probeLink(url, { fetchImpl = globalThis.fetch } = {}) {
  try {
    const res = await fetchImpl(url, {
      redirect: 'follow',
      headers: { 'User-Agent': probeUserAgent, Accept: 'text/html,*/*' },
      signal: AbortSignal.timeout(probeTimeoutMs),
    });
    if (res.ok) return PROBE_ALIVE;
    return isBotChallenge(res.headers) ? PROBE_CHALLENGED : PROBE_DEAD;
  } catch {
    return PROBE_DEAD;
  }
}

// classify — split lychee's error_map into dead, challenged and alive
// entries. Only errors that carry an HTTP status are probed: a DNS or
// connection failure never reached a server that could have answered.
export async function classify(report, { probe = probeLink } = {}) {
  const dead = [];
  const challenged = [];
  const alive = [];
  for (const [input, entries] of Object.entries(report.error_map ?? {})) {
    for (const entry of entries) {
      const row = { input, ...entry };
      const verdict = typeof entry.status?.code === 'number' ? await probe(entry.url) : PROBE_DEAD;
      if (verdict === PROBE_ALIVE) alive.push(row);
      else if (verdict === PROBE_CHALLENGED) challenged.push(row);
      else dead.push(row);
    }
  }
  return { dead, challenged, alive };
}

function formatEntry({ url, status, span }) {
  const code = typeof status?.code === 'number' ? status.code : 'ERROR';
  const at = span ? ` (at ${span.line}:${span.column})` : '';
  return `* [${code}] <${url}>${at} | ${status?.text ?? 'unknown failure'}`;
}

function byInput(rows) {
  const grouped = new Map();
  for (const row of rows) {
    if (!grouped.has(row.input)) grouped.set(row.input, []);
    grouped.get(row.input).push(row);
  }
  return grouped;
}

// renderReport — the markdown body of the tracking issue.
export function renderReport(report, { dead, challenged, alive = [] }) {
  const lines = [
    '# Summary',
    '',
    '| Status            | Count |',
    '|-------------------|-------|',
    `| Total             | ${report.total ?? 0} |`,
    `| Unique            | ${report.unique ?? 0} |`,
    `| Successful        | ${report.successful ?? 0} |`,
    `| Timeouts          | ${report.timeouts ?? 0} |`,
    `| Redirected        | ${report.redirects ?? 0} |`,
    `| Excluded          | ${report.excludes ?? 0} |`,
    `| Dead              | ${dead.length} |`,
    `| Bot-challenged    | ${challenged.length} |`,
    `| Alive on re-probe | ${alive.length} |`,
    '',
  ];
  if (dead.length > 0) {
    lines.push('## Dead links per input', '');
    for (const [input, rows] of byInput(dead)) {
      lines.push(`### ${input}`, '', ...rows.map(formatEntry), '');
    }
  }
  if (challenged.length > 0) {
    lines.push(
      '## Bot-challenged (not counted as dead)',
      '',
      'The host answered with a bot challenge, so the page could not be verified.',
      '',
    );
    for (const [input, rows] of byInput(challenged)) {
      lines.push(`### ${input}`, '', ...rows.map(formatEntry), '');
    }
  }
  return lines.join('\n');
}

// findTrackingIssue — the open issue with `title` and `label`, so a rerun
// updates it instead of opening another one with the same title.
export async function findTrackingIssue({
  repo,
  token,
  title,
  label,
  apiBase = DEFAULT_API_BASE,
  fetchImpl = globalThis.fetch,
}) {
  const url = `${apiBase}/repos/${repo}/issues?state=open&labels=${encodeURIComponent(label)}`;
  const issues = await ghPaginate({ url, token, what: 'list link-rot issues', fetchImpl });
  const match = issues
    .filter((issue) => !issue.pull_request && issue.title === title)
    .sort((a, b) => a.number - b.number)[0];
  return match ? match.number : null;
}

async function main() {
  const { LYCHEE_JSON, REPORT_PATH, GITHUB_TOKEN, GITHUB_REPOSITORY, GITHUB_API_URL, ISSUE_TITLE, ISSUE_LABEL } =
    process.env;
  if (!LYCHEE_JSON || !REPORT_PATH) {
    error('link-check-external: LYCHEE_JSON and REPORT_PATH are required');
    return 1;
  }
  let report;
  try {
    report = JSON.parse(readFileSync(LYCHEE_JSON, 'utf8'));
  } catch (cause) {
    error(`link-check-external: cannot read lychee JSON at ${LYCHEE_JSON}: ${cause.message}`);
    return 1;
  }

  const verdict = await classify(report);
  writeFileSync(REPORT_PATH, renderReport(report, verdict));
  for (const row of verdict.challenged) {
    notice(`bot challenge, not verifiable: ${row.url} (${row.input}:${row.span?.line ?? '?'})`);
  }
  log(
    `link-check-external: ${verdict.dead.length} dead, ${verdict.challenged.length} bot-challenged, ` +
      `${verdict.alive.length} alive on re-probe`,
  );
  setOutput('dead_count', String(verdict.dead.length));

  let issueNumber = null;
  if (verdict.dead.length > 0 && GITHUB_TOKEN && GITHUB_REPOSITORY) {
    if (!ISSUE_TITLE || !ISSUE_LABEL) {
      error('link-check-external: ISSUE_TITLE and ISSUE_LABEL are required with GITHUB_TOKEN');
      return 1;
    }
    issueNumber = await findTrackingIssue({
      repo: GITHUB_REPOSITORY,
      token: GITHUB_TOKEN,
      title: ISSUE_TITLE,
      label: ISSUE_LABEL,
      apiBase: GITHUB_API_URL || DEFAULT_API_BASE,
    });
  }
  setOutput('issue_number', issueNumber === null ? '' : String(issueNumber));
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(await main());
}
