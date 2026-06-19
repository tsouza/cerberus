// release-preflight.mjs — refuse to publish a release unless the tagged commit
// IS main's HEAD and EVERY check on that commit is FULLY SETTLED GREEN.
//
// The rule, with no softening:
//   1. The tagged commit MUST equal the current HEAD of the default branch
//      (main). You release the tip of main, never an older/side commit.
//   2. EVERY check-run + legacy status on that commit must be COMPLETED
//      (nothing still running / queued) AND green (success / skipped / neutral).
//      One running check, one failure, one cancelled/timed-out lane -> the
//      release ABORTS. No flaky-lane exclusions, no scheduled-event heuristics,
//      no "informational" passes. Main CI is fully complete + green, period.
//
// The ONLY exclusion is THIS release run's own jobs (preflight / goreleaser /
// chart-release). They are necessarily in-progress while preflight runs, so
// gating on them would deadlock — excluding them is structural, not a flakiness
// heuristic. They are identified by name via RELEASE_SELF_JOBS.
//
// `skipped` counts as green: a job a path-filter / `if:` guard deliberately did
// not run is a settled non-failure (e.g. the `changes` / `gate` no-ops), not a
// red and not "still running". Treating it as a failure would make the gate
// impossible to satisfy.
//
// Re-runs leave several check-runs with the same name; the latest (highest id)
// is the current state of that check, so a green re-run supersedes an earlier
// red. That is the check's present truth, not a flakiness exclusion.
//
// Env:
//   GITHUB_TOKEN       token with checks:read + statuses:read + contents:read.
//   GITHUB_REPOSITORY  "owner/name".
//   GITHUB_SHA         the tagged commit SHA.
//   GITHUB_API_URL     API base (default https://api.github.com).
//   RELEASE_SELF_JOBS  comma-separated check-run names belonging to THIS
//                      release workflow, excluded from the gate
//                      (default "preflight,goreleaser,chart-release").
//
// argv `--self-test` runs the in-process assertion suite and exits.

import { error, notice, log } from './lib/gh.mjs';

// A check-run is green when it completed with an accepting conclusion.
export const GREEN_CONCLUSIONS = new Set(['success', 'skipped', 'neutral']);

// evaluate is the pure gate: given main's HEAD sha, the tagged sha, the raw
// check-runs, the legacy statuses, and the self-job name set, it returns the
// list of blocking problems (empty == release may proceed) and the count of
// gated checks. No network, no exclusions beyond self-jobs — exported so the
// self-test pins the exact pass/fail boundary.
export function evaluate({ mainHead, taggedSha, checkRuns, statuses, selfJobs }) {
  const problems = [];
  if (!mainHead) {
    problems.push('could not resolve the default-branch (main) HEAD commit');
    return { problems, gated: 0 };
  }
  if (taggedSha !== mainHead) {
    problems.push(
      `tagged commit ${taggedSha.slice(0, 8)} is NOT main HEAD ${mainHead.slice(0, 8)} — ` +
        `a release may only be cut from the tip of main`,
    );
    return { problems, gated: 0 };
  }

  // Latest-per-name: the most recent run is the check's current state.
  const latest = new Map();
  for (const cr of checkRuns) {
    const prev = latest.get(cr.name);
    if (!prev || cr.id > prev.id) latest.set(cr.name, cr);
  }

  let gated = 0;
  for (const cr of latest.values()) {
    if (selfJobs.has(cr.name)) continue; // this release run's own jobs (structural)
    gated += 1;
    if (cr.status !== 'completed') {
      problems.push(`${cr.name}: still ${cr.status} (not completed)`);
    } else if (!GREEN_CONCLUSIONS.has(cr.conclusion)) {
      problems.push(`${cr.name}: ${cr.conclusion}`);
    }
  }

  // Legacy combined statuses (e.g. GitGuardian) — each context must be success.
  for (const st of statuses ?? []) {
    if (selfJobs.has(st.context)) continue;
    gated += 1;
    if (st.state !== 'success') {
      problems.push(`${st.context}: status ${st.state}`);
    }
  }

  return { problems, gated };
}

// ---------------------------------------------------------------------------
// self-test
// ---------------------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };
  const self = new Set(['preflight', 'goreleaser', 'chart-release']);
  const cr = (name, status, conclusion, id = 1) => ({ name, status, conclusion, id });

  // Happy path: tag == HEAD, all green, self-jobs in-progress are ignored.
  let r = evaluate({
    mainHead: 'abc', taggedSha: 'abc', selfJobs: self,
    checkRuns: [
      cr('check', 'completed', 'success'),
      cr('lint', 'completed', 'success'),
      cr('compose-smoke-shard-info (shard-crawl, …)', 'completed', 'success'),
      cr('dashboard', 'completed', 'success'),
      cr('changes', 'completed', 'skipped'),
      cr('preflight', 'in_progress', null),
      cr('goreleaser', 'in_progress', null),
    ],
    statuses: [{ context: 'GitGuardian Security Checks', state: 'success' }],
  });
  assert(r.problems.length === 0, 'all-green tip should pass: ' + r.problems.join('; '));
  // 5 non-self check-runs (check, lint, crawl, dashboard, changes) + 1 status.
  assert(r.gated === 6, `expected 6 gated (self-jobs excluded), got ${r.gated}`);

  // Tag is NOT main HEAD -> reject (this is exactly the v1.1.0 mistake).
  r = evaluate({ mainHead: 'def', taggedSha: 'abc', selfJobs: self, checkRuns: [], statuses: [] });
  assert(r.problems.length === 1 && /NOT main HEAD/.test(r.problems[0]), 'non-tip tag must fail');

  // A still-running NON-self check -> reject (no "running" allowed).
  r = evaluate({
    mainHead: 'abc', taggedSha: 'abc', selfJobs: self,
    checkRuns: [cr('coverage', 'in_progress', null)], statuses: [],
  });
  assert(r.problems.some((p) => /coverage: still in_progress/.test(p)), 'running lane must block');

  // A failure / cancellation -> reject. NO flaky exclusion for crawl/dashboard.
  r = evaluate({
    mainHead: 'abc', taggedSha: 'abc', selfJobs: self,
    checkRuns: [
      cr('compose-smoke-shard-info (shard-crawl, …)', 'completed', 'failure'),
      cr('dashboard', 'completed', 'cancelled'),
    ],
    statuses: [],
  });
  assert(r.problems.length === 2, 'crawl + dashboard reds must BOTH block (no exclusion)');

  // Re-run: an earlier failure superseded by a later success -> green.
  r = evaluate({
    mainHead: 'abc', taggedSha: 'abc', selfJobs: self,
    checkRuns: [
      cr('chaos', 'completed', 'failure', 1),
      cr('chaos', 'completed', 'success', 2),
    ],
    statuses: [],
  });
  assert(r.problems.length === 0, 'green re-run should supersede earlier fail');

  // Legacy status failure -> reject.
  r = evaluate({
    mainHead: 'abc', taggedSha: 'abc', selfJobs: self,
    checkRuns: [], statuses: [{ context: 'sec-scan', state: 'failure' }],
  });
  assert(r.problems.some((p) => /sec-scan: status failure/.test(p)), 'legacy status fail must block');

  // Unresolved main HEAD -> reject (fail safe).
  r = evaluate({ mainHead: null, taggedSha: 'abc', selfJobs: self, checkRuns: [], statuses: [] });
  assert(r.problems.length === 1 && /resolve/.test(r.problems[0]), 'missing main HEAD must fail');

  notice('release-preflight --self-test: all assertions passed');
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

const repo = process.env.GITHUB_REPOSITORY;
const sha = process.env.GITHUB_SHA;
const token = process.env.GITHUB_TOKEN;
const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
const selfJobs = new Set(
  (process.env.RELEASE_SELF_JOBS ?? 'preflight,goreleaser,chart-release')
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

// HEAD commit of the repo's default branch (main).
async function mainHeadSha() {
  const repoInfo = await getJSON(`${apiBase}/repos/${repo}`);
  const branch = repoInfo.default_branch || 'main';
  const b = await getJSON(`${apiBase}/repos/${repo}/branches/${branch}`);
  return b.commit?.sha ?? null;
}

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

async function combinedStatus() {
  return getJSON(`${apiBase}/repos/${repo}/commits/${sha}/status?per_page=100`);
}

const [mainHead, checkRuns, status] = await Promise.all([
  mainHeadSha(),
  allCheckRuns(),
  combinedStatus(),
]);

const { problems, gated } = evaluate({
  mainHead,
  taggedSha: sha,
  checkRuns,
  statuses: status.statuses ?? [],
  selfJobs,
});

if (problems.length > 0) {
  error(
    `release-preflight: refusing to publish — main is NOT fully complete + green on the ` +
      `tagged commit ${sha.slice(0, 8)}. Every CI lane on main must be settled green; then re-tag the tip.`,
  );
  for (const p of problems.sort()) error(`  - ${p}`);
  process.exit(1);
}

notice(
  `release-preflight: tagged commit ${sha.slice(0, 8)} is main HEAD and all ${gated} CI checks ` +
    `are settled green — proceeding with the release.`,
);
log(`release-preflight: ${gated} checks verified settled-green on main HEAD ${sha}`);
process.exit(0);
