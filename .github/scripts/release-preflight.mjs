// release-preflight.mjs — refuse to publish a release unless EVERY CI check
// on the tagged commit is green.
//
// Why: RC / GA tags are cut from `main` HEAD, so the tagged commit is a main
// commit whose push-triggered workflows (ci, compatibility, chdb, coverage,
// e2e dashboard+chaos, mutation, perf-profile, property, CodeQL, …) have
// already run. Branch protection only gates a SUBSET of those (the required
// checks) at PR time; this guard raises the bar for a release specifically:
// the whole of main must be green on the exact commit being tagged, including
// the informational lanes. If ANY check on the commit concluded non-green —
// or is still pending — the release aborts before goreleaser publishes.
//
// Only `release.yml` triggers on tags (verified: no other workflow has a
// `tags:` trigger), so the tagged commit's check-runs are exactly the
// push-to-main runs plus this release run's own jobs. The latter are excluded
// by name (RELEASE_SELF_JOBS) to avoid a self-deadlock.
//
// Env:
//   GITHUB_TOKEN       token with checks:read + statuses:read (the default
//                      workflow token has this).
//   GITHUB_REPOSITORY  "owner/name".
//   GITHUB_SHA         the tagged commit SHA (== the main commit).
//   GITHUB_API_URL     API base (default https://api.github.com).
//   RELEASE_SELF_JOBS  comma-separated check-run names belonging to THIS
//                      release workflow, excluded from the gate
//                      (default "preflight,goreleaser").
//
// Exit 0 when every non-self check on the commit is success/skipped/neutral;
// exit 1 (with ::error:: annotations) otherwise.

import { error, notice, log } from './lib/gh.mjs';

const repo = process.env.GITHUB_REPOSITORY;
const sha = process.env.GITHUB_SHA;
const token = process.env.GITHUB_TOKEN;
const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
const selfJobs = new Set(
  (process.env.RELEASE_SELF_JOBS ?? 'preflight,goreleaser')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean),
);

if (!repo || !sha || !token) {
  error('release-preflight: GITHUB_REPOSITORY, GITHUB_SHA and GITHUB_TOKEN are all required');
  process.exit(1);
}

const headers = {
  authorization: `Bearer ${token}`,
  accept: 'application/vnd.github+json',
  'x-github-api-version': '2022-11-28',
  'user-agent': 'cerberus-release-preflight',
};

async function getJSON(url) {
  const res = await fetch(url, { headers });
  if (!res.ok) {
    throw new Error(`GET ${url} -> ${res.status} ${res.statusText}`);
  }
  return res.json();
}

// All check-runs for the commit (GitHub Actions jobs, CodeQL, app checks),
// following pagination until a short page.
async function allCheckRuns() {
  const out = [];
  let page = 1;
  for (;;) {
    const data = await getJSON(
      `${apiBase}/repos/${repo}/commits/${sha}/check-runs?per_page=100&page=${page}`,
    );
    const runs = data.check_runs ?? [];
    out.push(...runs);
    if (runs.length < 100) break;
    page += 1;
  }
  return out;
}

// Legacy combined commit status (some security apps post here, not as a
// check-run). `state` is the rolled-up success/failure/pending.
async function combinedStatus() {
  return getJSON(`${apiBase}/repos/${repo}/commits/${sha}/status?per_page=100`);
}

// The release gate cares about the MERGE-TIME signal (push / pull_request /
// manual dispatch), NOT scheduled nightly re-runs. The nightly e2e re-runs the
// deep, slow lanes on whatever commit is main HEAD — notably the BFS `crawl`
// shard, which is a ~50-min long pole that routinely hits its timeout and ends
// `cancelled`/`failure`. Because those nightly check-runs share the SAME name
// as the push ones and carry a higher id, a naive latest-per-name pick lets a
// hung nightly supersede the green push result and block a release forever.
// So resolve each check-run's triggering workflow event and drop the scheduled
// ones; the push/PR/dispatch results are the merge-time truth the gate wants.
const SCHEDULED_EVENT = 'schedule';
const runEventCache = new Map();
async function checkRunEvent(cr) {
  const m = /\/actions\/runs\/(\d+)/.exec(cr.details_url || '');
  if (!m) return null; // non-Actions check (CodeQL / security app) — keep it
  const runId = m[1];
  if (runEventCache.has(runId)) return runEventCache.get(runId);
  let ev = null;
  try {
    const run = await getJSON(`${apiBase}/repos/${repo}/actions/runs/${runId}`);
    ev = run.event ?? null;
  } catch {
    ev = null; // on any resolution error, fail SAFE: keep the check (don't hide a red)
  }
  runEventCache.set(runId, ev);
  return ev;
}

// A check-run is green when it completed with an accepting conclusion. A job
// that is genuinely not applicable to this commit reports `skipped` (path
// filters, `if:` guards) and counts as green — that is a deliberate pass, not
// a failure.
const GREEN_CONCLUSIONS = new Set(['success', 'skipped', 'neutral']);

const [allRuns, status] = await Promise.all([allCheckRuns(), combinedStatus()]);

// Drop scheduled (nightly) check-runs; keep push / PR / dispatch ones.
const checkRuns = [];
for (const cr of allRuns) {
  if ((await checkRunEvent(cr)) === SCHEDULED_EVENT) continue;
  checkRuns.push(cr);
}

// Re-runs leave multiple check-runs with the same name; keep only the most
// recent per name (highest id) so a green re-run supersedes an earlier fail.
const latestByName = new Map();
for (const cr of checkRuns) {
  const prev = latestByName.get(cr.name);
  if (!prev || cr.id > prev.id) latestByName.set(cr.name, cr);
}

const problems = [];
let gated = 0;

for (const cr of latestByName.values()) {
  if (selfJobs.has(cr.name)) continue; // never gate on this release run itself
  gated += 1;
  if (cr.status !== 'completed') {
    problems.push(`${cr.name}: still ${cr.status} (not completed)`);
  } else if (!GREEN_CONCLUSIONS.has(cr.conclusion)) {
    problems.push(`${cr.name}: ${cr.conclusion}`);
  }
}

// Legacy statuses: each individual context must be success.
for (const st of status.statuses ?? []) {
  gated += 1;
  if (st.state !== 'success') {
    problems.push(`${st.context}: status ${st.state}`);
  }
}

if (problems.length > 0) {
  error(
    `release-preflight: commit ${sha.slice(0, 8)} (main) is NOT all-green — refusing to publish the release. ` +
      `Fix every CI lane on main, then re-tag.`,
  );
  for (const p of problems.sort()) error(`  - ${p}`);
  process.exit(1);
}

notice(
  `release-preflight: all ${gated} CI checks on commit ${sha.slice(0, 8)} are green — proceeding with the release.`,
);
log(`release-preflight: ${gated} checks verified green on ${sha}`);
process.exit(0);
