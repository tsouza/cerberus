// rejection-parity-divergence-liveness.mjs — the divergence-tracking-issue
// liveness gate.
//
// The `class: "divergence"` entries in test/rejection-parity/catalogue/ make a
// ratchet claim (see test/rejection-parity/doc.go): cerberus deliberately
// rejects a query the reference backend answers, and an OPEN GitHub issue in
// this repository tracks closing the gap. The Go meta-tests under
// test/rejection-parity run offline and cannot dial the GitHub API, so they
// pin the STRUCTURAL half of that claim (TrackingIssue is a positive integer —
// see TestCatalogueEntriesAreClassified). This script pins the LIVENESS half:
// every cited issue actually exists, is an issue rather than a pull request,
// and is still open. A divergence entry whose tracking issue was closed by
// someone who never touched the catalogue is exactly the failure mode this
// catches — nothing about editing the issue touches this repository's tree,
// so only a check that re-reads the catalogue on every run (not just a diff)
// can see it. Wired as an extra step in the `forbid-deferral` job
// (.github/workflows/forbid-deferral.yml), which already runs on every
// pull_request / push / merge_group and already carries the `issues: read`
// grant this lookup needs — mirrors the same liveness discipline
// forbid-deferral.mjs enforces for deferral markers, just against the
// catalogue's own state instead of a diff.
//
// This is the load-bearing half of why `divergence` is not the allow-list
// test/rejection-parity/doc.go forbids: an allow-list entry, once accepted,
// requires nothing further of anyone. A divergence entry requires its
// tracking issue to STAY open, checked mechanically forever — closing the
// issue without deleting or reclassifying the entry is a build failure, not a
// silent success.
//
// Env contract:
//   GITHUB_REPOSITORY  REQUIRED. `owner/repo` (runner-provided).
//   GITHUB_TOKEN       REQUIRED. Needs `issues: read`.
//   GITHUB_API_URL     API base (runner-provided; default https://api.github.com).
//   CATALOGUE_DIR      Directory holding the catalogue's per-source-file JSON
//                      shards (default test/rejection-parity/catalogue).
//
// Exit codes: 0 = every divergence entry's tracking issue is open and is an
// issue (or there are no divergence entries at all), 1 = a missing / closed /
// PR-shaped citation, a malformed entry, or a capability fault (token cannot
// read issues — see issuesReadability in forbid-deferral.mjs, imported and
// reused unchanged here for the same 404-means-two-things reason its own
// header documents).

import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { issuesReadability } from './forbid-deferral.mjs';
import { appendStepSummary, error, log, notice } from './lib/gh.mjs';

const HTTP_NOT_FOUND = 404;

// SHARD_EXT is the suffix of every catalogue shard; the directory holds
// nothing else.
const SHARD_EXT = '.json';

// --- pure halves --------------------------------------------------------

// mergeShards — the catalogue is stored as one shard per lowering SOURCE FILE
// (test/rejection-parity/catalogue/internal__promql__lower.go.json and
// friends), so this gate reads the directory and concatenates. Order does not
// matter here: every entry is examined, and the entries carry their own site
// key. A shard with no `entries` array throws rather than contributing
// nothing — an unreadable shard must never be indistinguishable from a
// catalogue with no divergence entries, which would pass silently.
export function mergeShards(shards) {
  const entries = [];
  for (const { path: shardPath, doc } of shards) {
    const shardEntries = doc && Array.isArray(doc.entries) ? doc.entries : null;
    if (shardEntries === null) {
      throw new Error(`${shardPath} has no "entries" array — malformed or wrong file`);
    }
    entries.push(...shardEntries);
  }
  return { entries };
}

// readCatalogue — mergeShards over a real directory. An empty (or shardless)
// directory is an error for the same reason a malformed shard is: this gate
// reports "no divergence entries" as a PASS, so it must never reach that
// conclusion by having read nothing.
export function readCatalogue(dir, { readDir = readdirSync, readFile = (p) => readFileSync(p, 'utf8') } = {}) {
  const names = readDir(dir).filter((n) => n.endsWith(SHARD_EXT)).sort();
  if (names.length === 0) {
    throw new Error(`${dir} holds no ${SHARD_EXT} shard — the catalogue is a directory of per-source-file shards`);
  }
  return mergeShards(
    names.map((name) => {
      const shardPath = path.join(dir, name);
      return { path: shardPath, doc: JSON.parse(readFile(shardPath)) };
    }),
  );
}

// divergenceEntries — every class="divergence" entry in the catalogue,
// unchanged, in file order. Any other class is out of scope for this script;
// TestCatalogueEntriesAreClassified is what polices "rejection" / "internal".
export function divergenceEntries(catalogue) {
  const entries = catalogue && Array.isArray(catalogue.entries) ? catalogue.entries : null;
  if (entries === null) {
    throw new Error('catalogue has no "entries" array — malformed or wrong file');
  }
  return entries.filter((e) => e.class === 'divergence');
}

// structuralViolation — the check that needs no network call: TrackingIssue
// must be a positive integer. TestCatalogueEntriesAreClassified pins this in
// Go too; it is re-asserted here because this script must not resolve a
// non-number against the GitHub API.
export function structuralViolation(entry) {
  const n = entry.tracking_issue;
  if (typeof n !== 'number' || !Number.isInteger(n) || n <= 0) {
    return `tracking_issue is ${JSON.stringify(n)} — must be a positive integer issue number`;
  }
  return null;
}

// livenessViolation — given a resolved { kind, state } for one issue number
// (or null when the lookup 404s), the reason it fails the liveness claim, or
// null when it holds. Mirrors forbid-deferral.mjs's citationVerdict, narrowed
// to a single citation (a divergence entry cites exactly one tracking issue,
// not a set).
export function livenessViolation(n, resolved) {
  if (!resolved || resolved.kind === 'missing') {
    return `#${n} names nothing in this repository`;
  }
  if (resolved.kind === 'pull-request') {
    return `#${n} is a pull request, not an issue`;
  }
  if (resolved.state !== 'open') {
    return `#${n} is a ${resolved.state} issue — the divergence it tracked must be re-filed, ` +
      'and the catalogue entry must cite the new issue (or be deleted if the divergence is ' +
      'actually resolved, or reclassified to "rejection" if the reference backend now also rejects)';
  }
  return null;
}

// --- network halves (mirrors forbid-deferral.mjs's private helpers) -----

function apiHeaders(token) {
  return {
    Accept: 'application/vnd.github+json',
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': '2022-11-28',
  };
}

async function probeStatus(url, token) {
  const res = await fetch(url, { headers: apiHeaders(token) });
  return { ok: res.ok, status: res.status, statusText: res.statusText };
}

async function apiJson(url, token, what) {
  const res = await fetch(url, { headers: apiHeaders(token) });
  if (res.status === HTTP_NOT_FOUND) return null;
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.json();
}

// resolveIssue — one issue number -> { kind: 'missing' | 'pull-request' |
// 'issue', state } | null. The capability probe fires at most once per run,
// only once a lookup has actually come back 404 — see forbid-deferral.mjs's
// resolveRefs, which this mirrors for a single-number caller.
async function resolveIssues(numbers, { repo, token, apiBase }) {
  const resolved = new Map();
  let probed = false;
  for (const n of numbers) {
    const issue = await apiJson(`${apiBase}/repos/${repo}/issues/${n}`, token, `resolve #${n}`);
    if (issue === null) {
      if (!probed) {
        probed = true;
        const capability = await issuesReadability({
          repo,
          apiBase,
          get: (url) => probeStatus(url, token),
        });
        if (!capability.readable) throw new Error(capability.reason);
      }
      resolved.set(n, { kind: 'missing' });
      continue;
    }
    resolved.set(n, { kind: issue.pull_request ? 'pull-request' : 'issue', state: issue.state });
  }
  return resolved;
}

function requireEnv(name, why) {
  const v = process.env[name];
  if (v === undefined || v === '') {
    throw new Error(`${name} is unset — ${why}`);
  }
  return v;
}

// --- runner ---------------------------------------------------------------

async function main() {
  const repo = requireEnv('GITHUB_REPOSITORY', 'the gate cannot resolve a cited issue without it');
  const token = requireEnv('GITHUB_TOKEN', 'resolving a cited issue needs issues:read');
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
  const catalogueDir = process.env.CATALOGUE_DIR || 'test/rejection-parity/catalogue';

  const entries = divergenceEntries(readCatalogue(catalogueDir));

  const violations = [];
  const structurallyValid = [];
  for (const e of entries) {
    const structural = structuralViolation(e);
    if (structural) {
      violations.push({ site: e.site, detail: structural });
      continue;
    }
    structurallyValid.push(e);
  }

  const numbers = [...new Set(structurallyValid.map((e) => e.tracking_issue))].sort((a, b) => a - b);
  const resolved = await resolveIssues(numbers, { repo, token, apiBase });

  for (const e of structurallyValid) {
    const detail = livenessViolation(e.tracking_issue, resolved.get(e.tracking_issue));
    if (detail) violations.push({ site: e.site, detail });
  }

  const summary = [
    `- divergence entries: **${entries.length}**`,
    `- distinct tracking issues resolved: **${numbers.length}**`,
    `- violations: **${violations.length}**`,
  ];
  appendStepSummary(
    [
      '## rejection-parity divergence liveness',
      '',
      ...summary,
      '',
      violations.length === 0
        ? 'Every divergence entry cites an open tracking issue.'
        : violations.map((v) => `- ❌ ${v.site}: ${v.detail}`).join('\n'),
    ].join('\n'),
  );
  for (const line of summary) log(line.replace(/\*\*/g, ''));

  if (violations.length > 0) {
    for (const v of violations) {
      error(
        `${v.site}: ${v.detail}. See test/rejection-parity/doc.go for the divergence-class ` +
          'contract — a divergence entry must always point at an open, live tracking issue.',
        { title: 'rejection-parity-divergence-liveness' },
      );
    }
    error(
      `rejection-parity-divergence-liveness: ${violations.length} divergence entry(ies) with a ` +
        'dead tracking-issue citation.',
      { title: 'rejection-parity-divergence-liveness' },
    );
    process.exit(1);
  }

  notice(
    entries.length === 0
      ? 'rejection-parity-divergence-liveness: no divergence entries in the catalogue.'
      : `rejection-parity-divergence-liveness: ${entries.length} divergence entry(ies), all citing ` +
        'an open tracking issue.',
    { title: 'rejection-parity-divergence-liveness' },
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`rejection-parity-divergence-liveness: ${String(e?.message ?? e)}`, {
      title: 'rejection-parity-divergence-liveness',
    });
    process.exit(1);
  });
}
