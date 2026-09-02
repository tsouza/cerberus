// release-gate-drift.mjs — scheduled drift detector for the RELEASE GATE's
// expected check set.
//
// `release-preflight.mjs` gates a publish on `RELEASE_REQUIRED_CHECKS`, an
// EXPECTED set hand-maintained in release.yml. That set is only ever exercised
// mid-publish, and it can rot in two independent directions that the preflight
// itself structurally cannot see:
//
//   A. PROTECTION DRIFT — the branch gains a required context that is in
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
//   C. PIN DRIFT — the live required-context set and the repo's own pin of it
//      (`branchProtectionContexts`, in the protection-pin file below) disagree.
//      Every in-tree gate reasons off that pin, because a Go test has no
//      network — so a disagreement leaves those gates authoritative about a
//      configuration that is not in force. See `pinnedProtectionDrift` for
//      both halves.
//
// WHERE THE LIVE SET COMES FROM. GitHub replaced this repository's legacy
// branch-protection object with RULESETS, and deleted the legacy object:
// `GET /repos/{owner}/{repo}/branches/{branch}/protection` now answers
// `404 Branch not protected`. The live read is therefore
// `GET /repos/{owner}/{repo}/rules/branches/{branch}`, which returns the
// FLATTENED list of rules in force on that branch — every ruleset that matches
// it contributes, tagged with its `ruleset_id`. More than one can carry a
// `required_status_checks` rule (this repo already runs a second ruleset over
// `refs/heads/release/*.x`), so the governing set is the UNION across them,
// not the first one found. Rule types this script does not model — `deletion`,
// `non_fast_forward`, `pull_request`, `merge_queue` — are ignored by type
// rather than by position, so a new rule appearing alongside them is inert
// here instead of shifting an index.
//
// The names `protectionDrift` / `pinnedProtectionDrift` /
// `branchProtectionContexts` are kept: what they mean — the set of contexts a
// merge into the branch must pass — did not change, only the endpoint that
// reports it.
//
// So: run all three checks on a schedule, off the release critical path, where
// the fix is a one-line PR instead of a broken publish.
//
// The lists are read from release.yml itself rather than duplicated here —
// there is exactly one copy of the data and one parser for it. A parse that
// yields an empty required set is a hard failure, not an empty comparison,
// because a silently-empty set makes BOTH checks vacuously green.
//
// NO DEDICATED CREDENTIAL, ON PURPOSE. The legacy protection endpoint needed
// repository `administration:read`, which the workflow token cannot receive, so
// this lane carried a `BRANCH_PROTECTION_TOKEN` secret. That secret was never
// provisioned: every scheduled run from 2026-08-04 to 2026-09-01 aborted on
// `BRANCH_PROTECTION_TOKEN is unset` before reaching the API, so the only
// automated comparison between the pin and reality had never once produced a
// verdict — which is how `update-golden-guard` became a required context and
// stayed unpinned. The rules endpoint carries no such requirement: it is served
// to any credential that can read the repository, so the ordinary
// `${{ github.token }}` is sufficient and there is no secret left to be absent.
// Do not reintroduce one; a credential this lane cannot verify it has is
// indistinguishable, on a Monday, from a lane nobody reads.
//
// Env:
//   GITHUB_TOKEN       REQUIRED. Least-privilege workflow token, used for the
//                      live rules read as well as for commits, check-runs and
//                      commit statuses. Missing/denied/malformed data fails
//                      closed — never into an empty, vacuously green set.
//   GITHUB_REPOSITORY  owner/repo (runner-provided).
//   GITHUB_API_URL     API base (default https://api.github.com).
//   DRIFT_BRANCH       Branch whose governing rulesets are read (default
//                      `main`).
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

// GitHub caps `per_page` at 100 for the commit list, the check-run list and the
// combined-status list.
const maxPerPage = 100;

// A busy commit on `main` carries far more than one page of check-runs — 146 on
// 2733b38c7, across 26 check suites — so a single-page read is a TRUNCATED
// observation, and direction B reports every name that landed past the cut as a
// dead lane. `forbid-deferral` sat on page 2 of that commit while its
// push-triggered run was green: the first real verdict this detector ever
// produced would have been a false accusation against a healthy required lane,
// which is how a rot detector gets dismissed as noise a second time. Pages are
// therefore walked to exhaustion, with a bound so a runaway pagination loop
// fails loudly instead of spinning.
const maxObservationPages = 10;

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
        `${ctx}: ruleset-REQUIRED context in neither RELEASE_REQUIRED_CHECKS nor ` +
        `RELEASE_INFORMATIONAL_CHECKS — the release publishes without waiting for it. ` +
        `Add it to the required set, or de-gate it explicitly with a reason.`,
    );
}

// Direction C. The repo pins its own expected required-context set in
// `branchProtectionContexts` (test/regression/release_required_checks_test.go),
// and every in-tree gate that reasons about "what a PR must pass" reads that
// pin rather than the live setting — because a Go test does not reach the
// network. So the pin is authoritative in the tree and powerless over the repo,
// which leaves the two apart with nothing watching:
//
//   - LIVE MISSING. A context the pin names is not enforced. Every in-tree gate
//     still reasons as though it were, and the check keeps posting green on
//     every PR, so the lane looks gating from every angle except the only one
//     that decides a merge. This is not hypothetical: `pr-body` sat in the pin
//     while the live setting enforced 19 contexts, and the release preflight —
//     which DOES require it on the maintenance path — never disagreed, because
//     it reads the pin too.
//   - LIVE EXTRA. The ruleset enforces a context the pin does not name. That is
//     the drift the pin's own doc comment calls "the one drift this file cannot
//     see", and it lands on the maintenance path as a lane certified by
//     absence. `update-golden-guard` spent a week in exactly that state, while
//     this very comparison was inert for want of a credential.
//
// Set EQUALITY is the assertion, in both directions, because either half alone
// is a gate that reports on something other than what it claims to gate.
export function pinnedProtectionDrift({ liveContexts, pinned, branch = 'main', rulesetIds = [] }) {
  const live = new Set(liveContexts ?? []);
  const pin = new Set(pinned ?? []);
  const where = describeRulesets(rulesetIds);

  const missing = [...pin]
    .filter((ctx) => !live.has(ctx))
    .map(
      (ctx) =>
        `${ctx}: pinned in branchProtectionContexts but NOT required by ${where} on ` +
        `\`${branch}\`. The lane still posts green on every PR, so nothing in the tree can tell ` +
        `the difference — the check is advisory while every gate that reads the pin treats it as ` +
        `binding. Restore it by adding the context to that ruleset's required_status_checks rule ` +
        `(Settings -> Rules -> Rulesets, or gh api -X PUT repos/<owner>/<repo>/rulesets/<id> ` +
        `--input <edited-ruleset>.json) — or, if it is meant to be advisory, drop it from the pin ` +
        `and from RELEASE_REQUIRED_CHECKS in the same change.`,
    );

  const extra = [...live]
    .filter((ctx) => !pin.has(ctx))
    .map(
      (ctx) =>
        `${ctx}: required by ${where} on \`${branch}\` but absent from ` +
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

export async function apiJson(url, headers, what, fetchImpl = globalThis.fetch) {
  const res = await fetchImpl(url, { headers });
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.json();
}

// apiPaged — every item across every page, not just the first. `pick` pulls the
// item array out of a page body, because the check-run and combined-status
// endpoints wrap theirs under different keys. A short page ends the walk; a
// walk that never shortens throws rather than silently returning a prefix,
// since a prefix is exactly the truncation this function exists to remove.
export async function apiPaged({ url, headers, what, pick, fetchImpl = globalThis.fetch }) {
  const join = url.includes('?') ? '&' : '?';
  const items = [];
  for (let page = 1; page <= maxObservationPages; page++) {
    const body = await apiJson(`${url}${join}per_page=${maxPerPage}&page=${page}`, headers, what, fetchImpl);
    const batch = pick(body) ?? [];
    items.push(...batch);
    if (batch.length < maxPerPage) return items;
  }
  throw new Error(
    `${what}: still returning full pages after ${maxObservationPages} of them ` +
      `(${maxObservationPages * maxPerPage} items) — the observation set would be truncated, ` +
      `which reports healthy lanes as dead`,
  );
}

function tokenHeaders(token) {
  return {
    Accept: 'application/vnd.github+json',
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': '2022-11-28',
  };
}

// The one rule type this script models. Everything else the rules endpoint
// reports for a branch — `deletion`, `non_fast_forward`, `pull_request`,
// `merge_queue` — is ignored BY TYPE, so a rule added beside them changes
// nothing here rather than shifting a position.
const requiredStatusChecksRuleType = 'required_status_checks';

// describeRulesets — how a drift message names the thing that enforces a
// context. Plural because the union below can genuinely span rulesets, and a
// message that says "the ruleset" when two are in force sends the reader to
// edit the wrong one.
export function describeRulesets(rulesetIds = []) {
  const ids = [...new Set((rulesetIds ?? []).filter((id) => id !== undefined && id !== null))];
  if (ids.length === 0) return 'the ruleset(s) in force';
  if (ids.length === 1) return `ruleset #${ids[0]}`;
  return `rulesets ${ids.map((id) => `#${id}`).join(' + ')}`;
}

// rulesetContexts — the required contexts a merge into `branch` must pass,
// read out of the FLATTENED rule list `GET /repos/{o}/{r}/rules/branches/{b}`
// returns.
//
// The endpoint answers with one array covering EVERY active ruleset that
// matches the branch, each entry tagged `ruleset_id`. So there can be more than
// one `required_status_checks` rule and the governing set is their UNION — this
// repository already runs a second ruleset over `refs/heads/release/*.x`, and
// reading only the first match would under-report the moment a branch falls
// under two. Contexts are `{context, integration_id}` objects here, not the
// bare strings the legacy protection object carried.
//
// Every degenerate shape throws rather than returning an empty list: an empty
// live set makes directions A and C vacuously green, which is the failure mode
// this whole lane exists to prevent.
export function rulesetContexts(rules, branch = 'main') {
  if (!Array.isArray(rules)) {
    throw new Error(
      `the rules endpoint for ${branch} returned a non-list payload — the branch-rules read is ` +
        `broken or the response shape changed; either way the comparison has nothing to compare`,
    );
  }

  const checkRules = rules.filter((rule) => rule?.type === requiredStatusChecksRuleType);
  if (checkRules.length === 0) {
    throw new Error(
      `no ${requiredStatusChecksRuleType} rule is in force on ${branch} — every ruleset carrying ` +
        `one was deleted, disabled or set to evaluate-only, or the credential cannot see them; ` +
        `all of those make this check vacuous`,
    );
  }

  const contexts = [];
  const seen = new Set();
  const rulesetIds = [];
  for (const rule of checkRules) {
    const entries = rule?.parameters?.required_status_checks;
    if (!Array.isArray(entries)) {
      throw new Error(
        `a ${requiredStatusChecksRuleType} rule on ${branch} carries malformed ` +
          `parameters.required_status_checks`,
      );
    }
    if (rule.ruleset_id !== undefined && !rulesetIds.includes(rule.ruleset_id)) {
      rulesetIds.push(rule.ruleset_id);
    }
    for (const entry of entries) {
      const context = entry?.context;
      if (typeof context !== 'string' || context.trim() === '') {
        throw new Error(
          `a ${requiredStatusChecksRuleType} rule on ${branch} names a non-string or empty ` +
            `context — the live set cannot be trusted`,
        );
      }
      if (seen.has(context)) continue;
      seen.add(context);
      contexts.push(context);
    }
  }

  if (contexts.length === 0) {
    throw new Error(
      `the ${requiredStatusChecksRuleType} rule(s) on ${branch} require zero contexts — nothing ` +
        `gates a merge, and comparing against an empty set would report a clean bill of health`,
    );
  }

  return { contexts, rulesetIds };
}

export async function readRequiredContexts({
  apiBase,
  repo,
  branch,
  token,
  fetchImpl = globalThis.fetch,
}) {
  if (!token) {
    throw new Error(
      'GITHUB_TOKEN is unset — the live required-context comparison cannot run',
    );
  }
  const rules = await apiJson(
    `${apiBase}/repos/${repo}/rules/branches/${encodeURIComponent(branch)}`,
    tokenHeaders(token),
    'read branch rules',
    fetchImpl,
  );
  return rulesetContexts(rules, branch);
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
  if (!token) {
    throw new Error(
      'GITHUB_TOKEN is unset — commit/check/status evidence cannot be read',
    );
  }

  const { required, informational } = parseCheckLists(readFileSync(workflowPath, 'utf8'));
  const pinned = parsePinnedContexts(readFileSync(pinPath, 'utf8'));

  const headers = tokenHeaders(token);
  const { contexts: liveContexts, rulesetIds } = await readRequiredContexts({
    apiBase,
    repo,
    branch,
    token,
  });

  const commits = await apiJson(
    `${apiBase}/repos/${repo}/commits?sha=${encodeURIComponent(branch)}&per_page=${Math.min(history, maxPerPage)}`,
    headers,
    'list recent commits',
  );

  const observed = new Set();
  for (const c of commits) {
    for (const run of await apiPaged({
      url: `${apiBase}/repos/${repo}/commits/${c.sha}/check-runs`,
      headers,
      what: `read check-runs for ${c.sha}`,
      pick: (body) => body?.check_runs,
    })) {
      observed.add(run.name);
    }
    for (const status of await apiPaged({
      url: `${apiBase}/repos/${repo}/commits/${c.sha}/status`,
      headers,
      what: `read statuses for ${c.sha}`,
      pick: (body) => body?.statuses,
    })) {
      observed.add(status.context);
    }
  }

  const problems = [
    ...protectionDrift({ liveContexts, required, informational }),
    ...laneDrift({ required, observed: [...observed] }),
    ...pinnedProtectionDrift({ liveContexts, pinned, branch, rulesetIds }),
  ];

  appendStepSummary(
    [
      `## release gate drift — \`${branch}\``,
      '',
      `- required contexts in force via ${describeRulesets(rulesetIds)}: **${liveContexts.length}** ` +
        `(pinned: **${pinned.length}**)`,
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
    `release gate is in sync: ${liveContexts.length} context(s) required by ` +
      `${describeRulesets(rulesetIds)} on \`${branch}\` all accounted for and matching the ` +
      `${pinned.length}-name in-tree pin exactly, ${required.length} required lane(s) all still ` +
      `posting across ${commits.length} commit(s)`,
    { title: 'release gate drift' },
  );
}

async function selfTest() {
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
  assert.match(aDrift[0], /^brand-new-gate: ruleset-REQUIRED context in neither/);

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
  assert.match(cMissing[0], /^pr-body: pinned in branchProtectionContexts but NOT required by/);

  const cExtra = pinnedProtectionDrift({ liveContexts: [...pinned, 'brand-new-gate'], pinned });
  assert.equal(cExtra.length, 1, `expected exactly one problem, got: ${cExtra.join('; ')}`);
  assert.match(cExtra[0], /^brand-new-gate: required by the ruleset\(s\) in force on `main` but absent/);

  // Both halves at once are reported together, not short-circuited: a rename
  // presents as exactly this pair, and seeing only one end of it hides which.
  assert.equal(pinnedProtectionDrift({ liveContexts: ['check', 'pr-boddy'], pinned }).length, 3);

  // A pin that cannot be parsed must throw rather than compare against nothing.
  assert.throws(() => parsePinnedContexts('var other = []string{"check"}'), /slice literal not found/);
  assert.throws(() => parsePinnedContexts('var branchProtectionContexts = []string{}'), /parsed as empty/);

  // Pagination. The regression this exists for is exact: a required lane whose
  // check-run lands past the first page of a busy commit must still count as
  // observed, or direction B accuses a healthy lane of being dead.
  const pageOf = (n, start) => Array.from({ length: n }, (_, i) => ({ name: `run-${start + i}` }));
  const twoPageCommit = [
    { check_runs: pageOf(maxPerPage, 0) },
    { check_runs: [...pageOf(45, maxPerPage), { name: 'forbid-deferral' }] },
  ];
  const requestedPages = [];
  const walked = await apiPaged({
    url: 'https://api.invalid/repos/example/project/commits/deadbeef/check-runs',
    headers: {},
    what: 'read check-runs',
    pick: (body) => body?.check_runs,
    fetchImpl: async (url) => {
      requestedPages.push(url);
      return { ok: true, json: async () => twoPageCommit[requestedPages.length - 1] };
    },
  });
  assert.equal(requestedPages.length, 2, 'a full first page must be followed by a second request');
  assert.match(requestedPages[0], /\?per_page=100&page=1$/);
  assert.match(requestedPages[1], /\?per_page=100&page=2$/);
  assert.equal(walked.length, maxPerPage + 46);
  assert.ok(
    walked.some((r) => r.name === 'forbid-deferral'),
    'a name on page 2 must survive into the observation set',
  );
  assert.deepEqual(
    laneDrift({ required: ['forbid-deferral'], observed: walked.map((r) => r.name) }),
    [],
    'the page-2 name must not be reported as a dead lane',
  );
  // A single short page still costs exactly one request.
  requestedPages.length = 0;
  assert.deepEqual(
    await apiPaged({
      url: 'https://api.invalid/x',
      headers: {},
      what: 'read statuses',
      pick: (body) => body?.statuses,
      fetchImpl: async (url) => {
        requestedPages.push(url);
        return { ok: true, json: async () => ({ statuses: [{ context: 'legacy' }] }) };
      },
    }),
    [{ context: 'legacy' }],
  );
  assert.equal(requestedPages.length, 1);
  // Endless full pages must fail loudly rather than return a silent prefix.
  await assert.rejects(
    () =>
      apiPaged({
        url: 'https://api.invalid/x',
        headers: {},
        what: 'read check-runs',
        pick: (body) => body?.check_runs,
        fetchImpl: async () => ({ ok: true, json: async () => ({ check_runs: pageOf(maxPerPage, 0) }) }),
      }),
    /still returning full pages after 10 of them/,
  );

  // A drift message must send the reader to the right ruleset, and say so in
  // the plural when more than one is in force — an id-less "the ruleset" would
  // have them editing whichever they happened to open.
  assert.equal(describeRulesets([]), 'the ruleset(s) in force');
  assert.equal(describeRulesets([22113683]), 'ruleset #22113683');
  assert.equal(describeRulesets([22113683, 22113683, 18472142]), 'rulesets #22113683 + #18472142');
  assert.match(
    pinnedProtectionDrift({ liveContexts: [], pinned: ['check'], rulesetIds: [22113683] })[0],
    /NOT required by ruleset #22113683 on `main`/,
  );

  // A RECORDED response from GET /repos/tsouza/cerberus/rules/branches/main,
  // captured 2026-09-02, trimmed to the fields this parser reads. Its job is to
  // fail loudly if the endpoint's shape moves again — the previous shape change
  // (branch protection deleted in favour of rulesets) was found by a human
  // reading a red cron, not by a test.
  const recordedMainRules = [
    { type: 'deletion', ruleset_id: 22113683 },
    { type: 'non_fast_forward', ruleset_id: 22113683 },
    {
      type: 'pull_request',
      parameters: {
        required_approving_review_count: 0,
        required_review_thread_resolution: true,
        allowed_merge_methods: ['squash'],
      },
      ruleset_id: 22113683,
    },
    {
      type: 'required_status_checks',
      parameters: {
        strict_required_status_checks_policy: false,
        do_not_enforce_on_create: false,
        required_status_checks: [
          { context: 'check' },
          { context: 'lint' },
          { context: 'forbid-skip' },
          { context: 'probe' },
          { context: 'chart-validate' },
          { context: 'coverage' },
          { context: 'property (PromQL + LogQL + TraceQL, rapid N=500)' },
          { context: 'strict-scan' },
          { context: 'CodeQL' },
          { context: 'forbid-deferral' },
          { context: 'pr-body' },
          { context: 'quickstart' },
          { context: 'agpl-clean' },
          { context: 'schema-ddl' },
          { context: 'config-docs' },
          { context: 'link-check' },
          { context: 'update-golden-guard' },
        ],
      },
      ruleset_id: 22113683,
    },
  ];

  const recorded = rulesetContexts(recordedMainRules, 'main');
  assert.equal(recorded.contexts.length, 17);
  assert.deepEqual(recorded.rulesetIds, [22113683]);
  // The matrix name survives as ONE context, exactly as it must for direction A
  // to match it against the release list.
  assert.ok(recorded.contexts.includes('property (PromQL + LogQL + TraceQL, rapid N=500)'));
  // Non-status-check rules contribute nothing and cost nothing.
  assert.ok(!recorded.contexts.some((c) => c === undefined));

  // A branch under TWO rulesets: the governing set is the union, deduplicated,
  // and both ids are reported so the remediation hint can name them. Reading
  // only the first matching rule would silently under-report the live set,
  // which lands as a direction-C "pinned but not required" on a context that IS
  // required.
  const twoRulesets = rulesetContexts(
    [
      {
        type: 'required_status_checks',
        parameters: { required_status_checks: [{ context: 'check' }, { context: 'lint' }] },
        ruleset_id: 1,
      },
      { type: 'merge_queue', parameters: { grouping_strategy: 'ALLGREEN' }, ruleset_id: 1 },
      {
        type: 'required_status_checks',
        parameters: { required_status_checks: [{ context: 'lint' }, { context: 'quickstart' }] },
        ruleset_id: 2,
      },
    ],
    'release/9.9.x',
  );
  assert.deepEqual(twoRulesets.contexts, ['check', 'lint', 'quickstart']);
  assert.deepEqual(twoRulesets.rulesetIds, [1, 2]);

  // Every degenerate live payload must throw rather than become an empty set:
  // an empty live set makes directions A and C report "no drift" over nothing.
  for (const [rules, pattern] of [
    [null, /non-list payload/],
    [{ type: 'required_status_checks' }, /non-list payload/],
    [[], /no required_status_checks rule is in force/],
    [[{ type: 'deletion' }, { type: 'pull_request' }], /no required_status_checks rule is in force/],
    [[{ type: 'required_status_checks', parameters: {} }], /malformed parameters/],
    [
      [{ type: 'required_status_checks', parameters: { required_status_checks: {} } }],
      /malformed parameters/,
    ],
    [
      [{ type: 'required_status_checks', parameters: { required_status_checks: [] } }],
      /require zero contexts/,
    ],
    [
      [
        {
          type: 'required_status_checks',
          parameters: { required_status_checks: [{ context: 'check' }, { context: '' }] },
        },
      ],
      /non-string or empty context/,
    ],
    [
      [
        {
          type: 'required_status_checks',
          parameters: { required_status_checks: [{ context: 'check' }, { context: 7 }] },
        },
      ],
      /non-string or empty context/,
    ],
    [
      [
        {
          type: 'required_status_checks',
          parameters: { required_status_checks: [{ context: 'check' }, 'lint'] },
        },
      ],
      /non-string or empty context/,
    ],
  ]) {
    assert.throws(() => rulesetContexts(rules, 'main'), pattern, `expected a throw for ${JSON.stringify(rules)}`);
  }

  // Credential and transport failures must never degrade into an empty, green
  // comparison either.
  await assert.rejects(
    () =>
      readRequiredContexts({
        apiBase: 'https://api.invalid',
        repo: 'example/project',
        branch: 'main',
        token: '',
        fetchImpl: async () => {
          throw new Error('network should not be reached without a credential');
        },
      }),
    /GITHUB_TOKEN is unset/,
  );

  await assert.rejects(
    () =>
      readRequiredContexts({
        apiBase: 'https://api.invalid',
        repo: 'example/project',
        branch: 'main',
        token: 'workflow-token',
        fetchImpl: async () => ({ ok: false, status: 403, statusText: 'Forbidden' }),
      }),
    /read branch rules: HTTP 403 Forbidden/,
  );

  // The URL is the endpoint that still exists, the branch is path-escaped so a
  // maintenance line reads its own rules rather than 404ing, and the ordinary
  // workflow token is what carries the request — there is no second credential
  // to be absent.
  let requestedUrl = null;
  const live = await readRequiredContexts({
    apiBase: 'https://api.invalid',
    repo: 'example/project',
    branch: 'release/1.2.x',
    token: 'workflow-token',
    fetchImpl: async (url, options) => {
      requestedUrl = url;
      assert.equal(options.headers.Authorization, 'Bearer workflow-token');
      return { ok: true, json: async () => recordedMainRules };
    },
  });
  assert.equal(
    requestedUrl,
    'https://api.invalid/repos/example/project/rules/branches/release%2F1.2.x',
  );
  assert.equal(live.contexts.length, 17);

  log('release-gate-drift self-test: OK');
}

if (process.argv.includes('--self-test')) {
  selfTest().catch((e) => {
    error(String(e?.message ?? e), { title: 'release gate drift self-test' });
    process.exit(1);
  });
} else {
  main().catch((e) => {
    error(String(e?.message ?? e), { title: 'release gate drift' });
    process.exit(1);
  });
}
