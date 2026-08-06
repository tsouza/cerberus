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
//   C. PIN DRIFT — the live branch-protection set and the repo's own pin of it
//      (`branchProtectionContexts`, in the protection-pin file below) disagree.
//      Every in-tree gate reasons off that pin, because reading the live
//      setting needs repo-admin rights a Go test does not have — so a
//      disagreement leaves those gates authoritative about a configuration
//      that is not in force. See `pinnedProtectionDrift` for both halves.
//
// So: run all three checks on a schedule, off the release critical path, where
// the fix is a one-line PR instead of a broken publish.
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
//   PROTECTION_PIN_FILE  Path to the Go file holding the in-tree
//                      `branchProtectionContexts` pin that direction C compares
//                      the live setting against (default
//                      test/regression/release_required_checks_test.go).
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

// Direction C. The repo pins its own expected protection set in
// `branchProtectionContexts` (test/regression/release_required_checks_test.go),
// and every in-tree gate that reasons about "what a PR must pass" reads that
// pin rather than the live setting — because a Go test cannot call an endpoint
// that needs repo-admin rights. So the pin is authoritative in the tree and
// powerless over the repo, which leaves the two apart with nothing watching:
//
//   - LIVE MISSING. A context the pin names is not enforced. Every in-tree gate
//     still reasons as though it were, and the check keeps posting green on
//     every PR, so the lane looks gating from every angle except the only one
//     that decides a merge. This is not hypothetical: `pr-body` sat in the pin
//     while branch protection enforced 19 contexts, and the release preflight —
//     which DOES require it on the maintenance path — never disagreed, because
//     it reads the pin too.
//   - LIVE EXTRA. Protection enforces a context the pin does not name. That is
//     the drift the pin's own doc comment calls "the one drift this file cannot
//     see", and it lands on the maintenance path as a lane certified by
//     absence.
//
// Set EQUALITY is the assertion, in both directions, because either half alone
// is a gate that reports on something other than what it claims to gate.
export function pinnedProtectionDrift({ liveContexts, pinned }) {
  const live = new Set(liveContexts ?? []);
  const pin = new Set(pinned ?? []);

  const missing = [...pin]
    .filter((ctx) => !live.has(ctx))
    .map(
      (ctx) =>
        `${ctx}: pinned in branchProtectionContexts but NOT enforced by branch protection on ` +
        `\`main\`. The lane still posts green on every PR, so nothing in the tree can tell the ` +
        `difference — the check is advisory while every gate that reads the pin treats it as ` +
        `binding. Restore it with: gh api -X POST repos/<owner>/<repo>/branches/main/protection/` +
        `required_status_checks/contexts -f 'contexts[]=${ctx}' — or, if it is meant to be ` +
        `advisory, drop it from the pin and from RELEASE_REQUIRED_CHECKS in the same change.`,
    );

  const extra = [...live]
    .filter((ctx) => !pin.has(ctx))
    .map(
      (ctx) =>
        `${ctx}: enforced by branch protection on \`main\` but absent from ` +
        `branchProtectionContexts. The release-preflight totality assertion runs OVER that pin, ` +
        `so an unpinned context is certified by absence on the maintenance path. Add it to the ` +
        `pin, which forces it into RELEASE_REQUIRED_CHECKS or an explicit de-gating.`,
    );

  return [...missing, ...extra];
}

// The pin is a plain Go string-slice literal, so a regex over the source is
// enough and keeps this script free of a Go toolchain. A pin that fails to
// parse — renamed, reshaped, moved — throws rather than comparing against an
// empty set, which would report perfect agreement with whatever is live.
export function parsePinnedContexts(goText) {
  const block = /var\s+branchProtectionContexts\s*=\s*\[\]string\{([\s\S]*?)\}/.exec(goText ?? '');
  if (block === null) {
    throw new Error('branchProtectionContexts slice literal not found in the protection pin file');
  }
  const names = [...block[1].matchAll(/"((?:[^"\\]|\\.)*)"/g)].map(([, s]) => s.replace(/\\(.)/g, '$1'));
  if (names.length === 0) {
    throw new Error('branchProtectionContexts parsed as empty — the equality check would be vacuously green');
  }
  return names;
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
  const pinPath = process.env.PROTECTION_PIN_FILE || 'test/regression/release_required_checks_test.go';
  const branch = process.env.DRIFT_BRANCH || 'main';
  const history = Number(process.env.DRIFT_HISTORY || defaultHistoryCommits);
  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  if (!repo) throw new Error('GITHUB_REPOSITORY is unset');
  if (!token) throw new Error('GITHUB_TOKEN is unset — reading branch protection needs a repo-admin token');

  const { required, informational } = parseCheckLists(readFileSync(workflowPath, 'utf8'));
  const pinned = parsePinnedContexts(readFileSync(pinPath, 'utf8'));

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
    ...pinnedProtectionDrift({ liveContexts, pinned }),
  ];

  appendStepSummary(
    [
      `## release gate drift — \`${branch}\``,
      '',
      `- branch-protection required contexts: **${liveContexts.length}** (pinned: **${pinned.length}**)`,
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
    `release gate is in sync: ${liveContexts.length} protected context(s) all accounted for and ` +
      `matching the ${pinned.length}-name in-tree pin exactly, ${required.length} required lane(s) ` +
      `all still posting across ${commits.length} commit(s)`,
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

  // Direction C: equality holds, then each half of it broken on its own.
  const goPin = [
    'var branchProtectionContexts = []string{',
    '\t"check",',
    '\t"pr-body",',
    '\t"property (PromQL + LogQL + TraceQL, rapid N=500)",',
    '}',
    '',
    'const workflowsDir = "../../.github/workflows"',
  ].join('\n');
  const pinned = parsePinnedContexts(goPin);
  assert.deepEqual(pinned, ['check', 'pr-body', 'property (PromQL + LogQL + TraceQL, rapid N=500)']);

  assert.deepEqual(
    pinnedProtectionDrift({ liveContexts: [...pinned].reverse(), pinned }),
    [],
    'the comparison is set-wise, so ordering alone is not drift',
  );

  // The regression this direction exists for: a pinned context silently absent
  // from the live setting, which no in-tree gate can observe.
  const cMissing = pinnedProtectionDrift({
    liveContexts: ['check', 'property (PromQL + LogQL + TraceQL, rapid N=500)'],
    pinned,
  });
  assert.equal(cMissing.length, 1, `expected exactly one problem, got: ${cMissing.join('; ')}`);
  assert.match(cMissing[0], /^pr-body: pinned in branchProtectionContexts but NOT enforced/);

  const cExtra = pinnedProtectionDrift({ liveContexts: [...pinned, 'brand-new-gate'], pinned });
  assert.equal(cExtra.length, 1, `expected exactly one problem, got: ${cExtra.join('; ')}`);
  assert.match(cExtra[0], /^brand-new-gate: enforced by branch protection on `main` but absent/);

  // Both halves at once are reported together, not short-circuited: a rename
  // presents as exactly this pair, and seeing only one end of it hides which.
  assert.equal(pinnedProtectionDrift({ liveContexts: ['check', 'pr-boddy'], pinned }).length, 3);

  // A pin that cannot be parsed must throw rather than compare against nothing.
  assert.throws(() => parsePinnedContexts('var other = []string{"check"}'), /slice literal not found/);
  assert.throws(() => parsePinnedContexts('var branchProtectionContexts = []string{}'), /parsed as empty/);

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
