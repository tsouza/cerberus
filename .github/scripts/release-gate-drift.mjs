// release-gate-drift.mjs — scheduled drift detector for the RELEASE GATE's
// expected check set.
//
// `release-preflight.mjs` gates a publish on `RELEASE_REQUIRED_CHECKS`, an
// EXPECTED set hand-maintained in release.yml. That set is only ever exercised
// mid-publish, and it can rot in two independent directions that the preflight
// itself structurally cannot see:
//
//   A. PROTECTION DRIFT — branch protection gains a required context that is in
//      neither `RELEASE_REQUIRED_CHECKS` nor `RELEASE_INFORMATIONAL_CHECKS`.
//      Every PR now has to pass it, but the release does not wait for it, so it
//      publishes past a lane the repo considers gating. The preflight cannot
//      catch this: its set is an allow-list of names to WAIT for, and a name
//      nobody listed is a name nobody waits for. Fails silently, in the
//      dangerous direction.
//
//   B. LANE DRIFT — a name in `RELEASE_REQUIRED_CHECKS` stops matching any lane
//      that actually posts a check-run (a job renamed, a matrix leg's display
//      string reworded, a workflow deleted). The preflight then waits out its
//      full window on a check-run that will never arrive and ABORTS the
//      release. Fails closed rather than open, but it fails at the single worst
//      moment — mid-publish, with the tag already cut. `property (PromQL +
//      LogQL + TraceQL, rapid N=500)` is the standing reminder that these names
//      are display strings, not identifiers.
//
// So: run both checks on a schedule, off the release critical path, where the
// fix is a one-line PR instead of a broken publish.
//
// The lists are read from release.yml itself rather than duplicated here —
// there is exactly one copy of the data and one parser for it. A parse that
// yields an empty required set is a hard failure, not an empty comparison,
// because a silently-empty set makes BOTH checks vacuously green.
//
// Env:
//   GITHUB_TOKEN       REQUIRED. Reading branch protection needs a token with
//                      repo-admin rights — the default `GITHUB_TOKEN` does NOT
//                      qualify, so the workflow passes `RELEASE_PAT`.
//   GITHUB_REPOSITORY  owner/repo (runner-provided).
//   GITHUB_API_URL     API base (default https://api.github.com).
//   DRIFT_BRANCH       Branch whose protection is compared (default `main`).
//   DRIFT_HISTORY      How many recent commits on that branch are scanned for
//                      posted check-run names (default 20).
//   RELEASE_WORKFLOW   Path to the workflow holding the lists
//                      (default .github/workflows/release.yml).
//   argv `--self-test` runs the pure-logic assertions and exits.
//
// Exit: 0 when both directions are clean; 1 on any drift, a failed API read, or
// an unparseable release.yml.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import process from 'node:process';

import { error, notice, log, appendStepSummary } from './lib/gh.mjs';

// How far back the lane-drift scan looks. A single commit is not enough: lanes
// legitimately condition themselves off (a docs-only push skips `check`), so
// one commit's check-run set is a subset of the lane inventory, not the whole
// of it. A name absent from EVERY commit in this window is dead, not skipped.
const defaultHistoryCommits = 20;

// GitHub caps `per_page` at 100 for both the commit list and the check-run list.
const maxPerPage = 100;

// A block scalar's content is indented deeper than its key; the first line at or
// below the key's indentation ends it. Comment lines sit at the key's own
// indentation in release.yml, so they terminate the scalar rather than becoming
// content — which is why they need no stripping here.
export function parseBlockScalar(yamlText, key) {
  const lines = yamlText.split('\n');
  const start = lines.findIndex((l) => new RegExp(`^(\\s*)${key}:\\s*\\|\\s*$`).test(l));
  if (start === -1) return null;
  const keyIndent = lines[start].length - lines[start].trimStart().length;
  const items = [];
  for (const line of lines.slice(start + 1)) {
    if (line.trim() === '') continue;
    const indent = line.length - line.trimStart().length;
    if (indent <= keyIndent) break;
    items.push(line.trim());
  }
  return items;
}

// parseCheckLists — pull the three lists out of release.yml. Throws rather than
// returning a partial result: every caller below treats an empty list as
// "nothing to compare", so a silent parse failure would report a clean bill of
// health on a set it never read.
export function parseCheckLists(yamlText) {
  const required = parseBlockScalar(yamlText, 'RELEASE_REQUIRED_CHECKS');
  const informational = parseBlockScalar(yamlText, 'RELEASE_INFORMATIONAL_CHECKS');
  if (required === null) {
    throw new Error('RELEASE_REQUIRED_CHECKS block scalar not found in the release workflow');
  }
  if (informational === null) {
    throw new Error('RELEASE_INFORMATIONAL_CHECKS block scalar not found in the release workflow');
  }
  if (required.length === 0) {
    throw new Error('RELEASE_REQUIRED_CHECKS parsed as empty — both drift checks would be vacuously green');
  }
  return { required, informational };
}

// Same prefix rule release-preflight.mjs applies, so "covered" here means
// exactly what "not gated on" means there. Duplicating the semantics with a
// different match would make this detector agree with a preflight that
// disagrees with it.
function isInformational(name, informational) {
  return (informational ?? []).some((p) => p && name.startsWith(p));
}

// Direction A. A live required context is accounted for when the release either
// WAITS for it (exact name in `required`) or has explicitly DE-GATED it (prefix
// in `informational`). Anything else is a context the repo gates PRs on and the
// release does not gate publishes on.
export function protectionDrift({ liveContexts, required, informational }) {
  const req = new Set(required);
  return (liveContexts ?? [])
    .filter((ctx) => !req.has(ctx) && !isInformational(ctx, informational))
    .map(
      (ctx) =>
        `${ctx}: branch-protection REQUIRED context in neither RELEASE_REQUIRED_CHECKS nor ` +
        `RELEASE_INFORMATIONAL_CHECKS — the release publishes without waiting for it. ` +
        `Add it to the required set, or de-gate it explicitly with a reason.`,
    );
}

// Direction B. `required` names are check-run display strings; `observed` is
// every display string seen across the scanned window. A required name that
// nothing posts is a name the preflight will block the next release waiting for.
export function laneDrift({ required, observed }) {
  const seen = new Set(observed ?? []);
  return (required ?? [])
    .filter((name) => !seen.has(name))
    .map(
      (name) =>
        `${name}: RELEASE_REQUIRED_CHECKS name posted no check-run in the scanned window — ` +
        `nothing produces it any more, so the next release will wait out its full ` +
        `window on it and abort. Fix the name, or drop the lane from the required set.`,
    );
}

async function apiJson(url, headers, what) {
  const res = await fetch(url, { headers });
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.json();
}

async function main() {
  const workflowPath = process.env.RELEASE_WORKFLOW || '.github/workflows/release.yml';
  const branch = process.env.DRIFT_BRANCH || 'main';
  const history = Number(process.env.DRIFT_HISTORY || defaultHistoryCommits);
  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  if (!repo) throw new Error('GITHUB_REPOSITORY is unset');
  if (!token) throw new Error('GITHUB_TOKEN is unset — reading branch protection needs a repo-admin token');

  const { required, informational } = parseCheckLists(readFileSync(workflowPath, 'utf8'));

  const headers = {
    Accept: 'application/vnd.github+json',
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': '2022-11-28',
  };

  const protection = await apiJson(
    `${apiBase}/repos/${repo}/branches/${encodeURIComponent(branch)}/protection`,
    headers,
    'read branch protection',
  );
  const liveContexts = protection?.required_status_checks?.contexts ?? [];
  if (liveContexts.length === 0) {
    throw new Error(
      `branch protection on ${branch} reports zero required contexts — either protection was ` +
        `removed or the token cannot see it; both make this check vacuous`,
    );
  }

  const commits = await apiJson(
    `${apiBase}/repos/${repo}/commits?sha=${encodeURIComponent(branch)}&per_page=${Math.min(history, maxPerPage)}`,
    headers,
    'list recent commits',
  );

  const observed = new Set();
  for (const c of commits) {
    const runs = await apiJson(
      `${apiBase}/repos/${repo}/commits/${c.sha}/check-runs?per_page=${maxPerPage}`,
      headers,
      `read check-runs for ${c.sha}`,
    );
    for (const run of runs?.check_runs ?? []) observed.add(run.name);
    for (const s of (await apiJson(`${apiBase}/repos/${repo}/commits/${c.sha}/status`, headers, 'read statuses'))
      ?.statuses ?? []) {
      observed.add(s.context);
    }
  }

  const problems = [
    ...protectionDrift({ liveContexts, required, informational }),
    ...laneDrift({ required, observed: [...observed] }),
  ];

  appendStepSummary(
    [
      `## release gate drift — \`${branch}\``,
      '',
      `- branch-protection required contexts: **${liveContexts.length}**`,
      `- \`RELEASE_REQUIRED_CHECKS\`: **${required.length}**`,
      `- \`RELEASE_INFORMATIONAL_CHECKS\`: **${informational.length}**`,
      `- commits scanned for posted lane names: **${commits.length}**`,
      '',
      problems.length === 0 ? 'No drift.' : problems.map((p) => `- ❌ ${p}`).join('\n'),
    ].join('\n'),
  );

  if (problems.length > 0) {
    for (const p of problems) error(p, { title: 'release gate drift' });
    process.exit(1);
  }

  notice(
    `release gate is in sync: ${liveContexts.length} protected context(s) all accounted for, ` +
      `${required.length} required lane(s) all still posting across ${commits.length} commit(s)`,
    { title: 'release gate drift' },
  );
}

function selfTest() {
  const yaml = [
    '        env:',
    '          RELEASE_REQUIRED_CHECKS: |',
    '            check',
    '            property (PromQL + LogQL + TraceQL, rapid N=500)',
    '            migration-e2e',
    '          # a comment at key indentation ends the scalar',
    '          RELEASE_INFORMATIONAL_CHECKS: |',
    '            compose-smoke-shard-info',
    '            mutation',
    '        steps:',
  ].join('\n');

  const { required, informational } = parseCheckLists(yaml);
  assert.deepEqual(required, ['check', 'property (PromQL + LogQL + TraceQL, rapid N=500)', 'migration-e2e']);
  assert.deepEqual(informational, ['compose-smoke-shard-info', 'mutation']);

  // An unparseable / empty required set must throw, not compare against nothing.
  assert.throws(() => parseCheckLists('env:\n  OTHER: |\n    x\n'), /RELEASE_REQUIRED_CHECKS block scalar not found/);
  assert.throws(
    () => parseCheckLists('  RELEASE_REQUIRED_CHECKS: |\nsteps:\n  RELEASE_INFORMATIONAL_CHECKS: |\n    m\n'),
    /parsed as empty/,
  );

  // Direction A: covered exactly, covered by prefix, and uncovered.
  assert.deepEqual(
    protectionDrift({ liveContexts: ['check', 'mutation'], required, informational }),
    [],
    'an exact required name and a prefix-de-gated name are both accounted for',
  );
  const aDrift = protectionDrift({ liveContexts: ['check', 'brand-new-gate'], required, informational });
  assert.equal(aDrift.length, 1, `expected exactly one problem, got: ${aDrift.join('; ')}`);
  assert.match(aDrift[0], /^brand-new-gate: branch-protection REQUIRED context in neither/);

  // Direction B: a name nothing posts, versus a full set that everything posts.
  assert.deepEqual(laneDrift({ required, observed: required }), []);
  const bDrift = laneDrift({ required, observed: ['check', 'migration-e2e'] });
  assert.equal(bDrift.length, 1, `expected exactly one problem, got: ${bDrift.join('; ')}`);
  assert.match(bDrift[0], /^property \(PromQL \+ LogQL \+ TraceQL, rapid N=500\): RELEASE_REQUIRED_CHECKS name posted no/);

  // Negative control for the comma trap parseCheckList exists to avoid: the
  // matrix name survives the block-scalar parse as ONE name, so it matches the
  // one check-run that posts it. Split on `,` it would drift in direction B.
  assert.deepEqual(
    laneDrift({
      required: ['property (PromQL + LogQL + TraceQL, rapid N=500)'],
      observed: ['property (PromQL + LogQL + TraceQL, rapid N=500)'],
    }),
    [],
  );

  log('release-gate-drift self-test: OK');
}

if (process.argv.includes('--self-test')) {
  selfTest();
} else {
  main().catch((e) => {
    error(String(e?.message ?? e), { title: 'release gate drift' });
    process.exit(1);
  });
}
